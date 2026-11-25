package service

// 快照生命周期 L2/L3/L5 的服务级编排测试（批⑥b §4.4）：L3 start 拒绝与重归一化、
// L2 孤儿回收不重拉、L5 经 Monitor 自愈路径回写磁盘。L4（reload）的用例在
// reload_test.go；单元级用例在 snapshot_test.go。拆出自 reload_test.go（单文件
// ≤400 行）。

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStartRejectedByNormalizer (L3 拒绝分支)：用户文件含被拒键 → ok:false +
// rejected:true + 错误点名键；不 spawn（无内核 sock、无 pid 文件）、R 原封不动、
// 代际不被消耗（修好文件后同一代际可重试）。
func TestStartRejectedByNormalizer(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	sentinel := []byte("sentinel\n")
	if err := os.WriteFile(RuntimeIn(conf.CoreHome), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf.UserRuntime, []byte(rejectedUserConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	resp, err := cl.Start(1)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if resp.OK || !resp.Rejected || !strings.Contains(resp.Error, "external-ui") {
		t.Fatalf("Start = %+v err %v, want rejected naming the key", resp, err)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may spawn behind a rejected config (status pid %d)", st.PID)
	}
	if _, err := os.Stat(conf.KernelSock); !os.IsNotExist(err) {
		t.Fatalf("kernel sock must not exist (err = %v)", err)
	}
	if _, err := os.Stat(cfg.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("core.pid must not exist (err = %v)", err)
	}
	staged, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil || !bytes.Equal(staged, sentinel) {
		t.Fatalf("R = %q err %v, want the sentinel untouched", staged, err)
	}

	// 被拒的尝试不消耗代际（批⑥b c3b 审查 P3）：修好文件后，同一代际的 start 必须
	// 被准入——curGen 只在 spawn 成功之后才前进。
	if err := os.WriteFile(conf.UserRuntime, []byte("mode: rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retry, err := cl.Start(1)
	if err != nil || !retry.OK || retry.PID <= 0 {
		t.Fatalf("Start(1) after fixing the config = %+v err %v, want admitted on the same generation", retry, err)
	}
}

// TestStartRenormalizesAfterUserEdit (L3)：start 从不沿用旧快照——用户在服务空闲期
// 间改的配置，下一次 start 必须落进 R。
func TestStartRenormalizesAfterUserEdit(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	if err := os.WriteFile(conf.UserRuntime, []byte("mode: rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := cl.Start(1); err != nil || !r.OK {
		t.Fatalf("Start(1) = %+v err %v, want ok", r, err)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 用户改配置后第二次 start：R 必须是新内容的归一化产物
	if err := os.WriteFile(conf.UserRuntime, []byte("mixed-port: 17890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := cl.Start(2); err != nil || !r.OK {
		t.Fatalf("Start(2) = %+v err %v, want ok", r, err)
	}
	product, rej := NormalizeRuntime([]byte("mixed-port: 17890\n"))
	if rej != nil {
		t.Fatalf("normalize fixture rejected at %s: %s", rej.Key, rej.Reason)
	}
	staged, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil || string(staged) != string(product.Bytes) {
		t.Fatalf("R = %q err %v, want the NEW normalized product %q", staged, err, product.Bytes)
	}
}

// TestOrphanReclaimedWithoutSnapshotStaysDown (L2)：快照为 nil（无 R）→ 孤儿仍被
// 回收，但不重拉——没有内核起来；服务继续监听，status 照常应答 ok:true pid=0。
func TestOrphanReclaimedWithoutSnapshotStaysDown(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	// 不预置 R → L1 种子没有产物 → 快照 nil
	orphan := exec.Command(fakeCoreBin, "-d", conf.CoreHome, "-f", RuntimeIn(conf.CoreHome),
		"-ext-ctl-unix", conf.KernelSock)
	orphan.Env = []string{"FAKE_CORE_MODE=normal"}
	if err := orphan.Start(); err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = orphan.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default:
			_ = orphan.Process.Kill()
		}
	})
	waitForFile(t, conf.KernelSock, 5*time.Second)
	if err := os.WriteFile(cfg.PIDPath, []byte(fmt.Sprintf("%d\n", orphan.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	startStubServer(t, cfg)
	// 孤儿被回收（等待 SIGTERM/兜底 SIGKILL 生效——killGrace 至多 5s）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(orphan.Process.Pid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(orphan.Process.Pid) {
		t.Fatal("the orphan must be reclaimed even without a snapshot")
	}
	// 没有新内核被拉起：原生 sock 拨不通（孤儿残留的 sock 文件不会被清——内核退出
	// 不 unlink，liveness 才是有意义的断言）
	if probeAlive(conf.KernelSock) {
		t.Fatal("no core may respawn without a normalized snapshot")
	}
	if _, err := os.Stat(cfg.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("core.pid must stay absent (err = %v)", err)
	}
	// 服务仍在监听：status 照常应答（pid = 0）
	cl := NewClient(cfg.SockPath, 10*time.Second)
	st, err := cl.Status()
	if err != nil || !st.OK || st.PID != 0 {
		t.Fatalf("Status = %+v err %v, want ok pid=0 (service stays up)", st, err)
	}
}

// TestSelfHealRestoresRuntimeFromSnapshot (L5 经 Monitor 自愈路径)：运行中 R 被篡改
// → SIGKILL 内核 → 自愈 respawn 的 PreSpawn 钩子按快照把磁盘补回。
func TestSelfHealRestoresRuntimeFromSnapshot(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	srv, _ := startStubServerTuned(t, cfg, nil)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK || first.PID <= 0 {
		t.Fatalf("initial Start = %+v err %v", first, err)
	}
	snap := srv.snapshotCopy()
	if snap == nil {
		t.Fatal("start must leave a snapshot behind")
	}
	// 篡改磁盘（生产中只有 root 能造成；测试以同账号模拟）
	if err := os.WriteFile(RuntimeIn(conf.CoreHome), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub core: %v", err)
	}
	// 等自愈 respawn：core.pid 指向新的存活内核（判据同 TestMonitorRespawnRewritesCorePID）
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(cfg.PIDPath); err == nil {
			var pid int
			if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); perr == nil &&
				pid > 0 && pid != first.PID && processAlive(pid) && probeAlive(conf.KernelSock) {
				staged, rerr := os.ReadFile(RuntimeIn(conf.CoreHome))
				if rerr != nil || !bytes.Equal(staged, snap) {
					t.Fatalf("R after self-heal = %q err %v, want restored from the snapshot", staged, rerr)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("self-heal respawn did not complete within 12s")
}
