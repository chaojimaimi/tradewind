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

// TestOrphanDeadPIDRecordRespawnsFromSnapshot (L2/c8 ③)：core.pid 记录指向已死 pid
// → 服务启动时按快照重拉。这正是真机缺陷形态（2026-11-26）：内核被 launchd 对死
// 作业的清理带走晚于服务被拉回、早于收编检查——旧实现死分支直接 return，服务停在
// "running, no managed core" 半态；有快照说明上一会话在跑，恢复它是既有设计意图。
func TestOrphanDeadPIDRecordRespawnsFromSnapshot(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	// 预置 R 为当前归一化器的合法产物（属主 = 当前用户）→ L1 种子据此采纳快照
	seed, rej := NormalizeRuntime([]byte("proxies: []\n"))
	if rej != nil {
		t.Fatalf("seed normalize rejected at %s: %s", rej.Key, rej.Reason)
	}
	if err := os.WriteFile(RuntimeIn(conf.CoreHome), seed.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 造一个"已死 pid 记录"：Run 收尸过的进程 pid 必然不在（ESRCH）
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	if processAlive(dead.Process.Pid) {
		t.Skipf("pid %d unexpectedly alive (reused); not a dead-record fixture", dead.Process.Pid)
	}
	if err := os.WriteFile(cfg.PIDPath, []byte(fmt.Sprintf("%d\n", dead.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	startStubServer(t, cfg)
	// 死分支也重拉：core.pid 记录新内核、原生 sock 可达（不再是半态）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(cfg.PIDPath)
		if err == nil {
			var newPID int
			if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &newPID); perr == nil &&
				newPID > 0 && newPID != dead.Process.Pid && processAlive(newPID) && probeAlive(conf.KernelSock) {
				// L5 核对通过：重拉后的 R 仍是种子字节（未被回写）
				staged, rerr := os.ReadFile(RuntimeIn(conf.CoreHome))
				if rerr != nil || !bytes.Equal(staged, seed.Bytes) {
					t.Fatalf("runtime.yaml after the dead-branch respawn = %q err %v, want the seeded bytes", staged, rerr)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the service must respawn from the snapshot when core.pid points at a dead kernel (15s)")
}

// TestOrphanStaleButAliveRecordReclaimedOnlyOnMatch (L2/c8 ③)：pid 复用防护保留——
// 记录指向的 pid 活着但身份不符（命令行不含 -d <core_home>）= 内核已不在（记录是
// 陈旧的）→ 不回收（绝不 SIGTERM 一个不相干进程）、但照新逻辑按快照重拉。本用例用
// 测试进程自身的 pid 当"活而不符"记录：若回归成"活即回收"，测试进程会收到 SIGTERM
// 而当场死掉。
func TestOrphanStaleButAliveRecordReclaimedOnlyOnMatch(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	seed, rej := NormalizeRuntime([]byte("proxies: []\n"))
	if rej != nil {
		t.Fatalf("seed normalize rejected at %s: %s", rej.Key, rej.Reason)
	}
	if err := os.WriteFile(RuntimeIn(conf.CoreHome), seed.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 活但身份不符的记录：测试进程自己的命令行是 go test / 本包测试二进制，绝不含
	// "-d <core_home>"
	if err := os.WriteFile(cfg.PIDPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	startStubServer(t, cfg)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(cfg.PIDPath)
		if err == nil {
			var newPID int
			if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &newPID); perr == nil &&
				newPID > 0 && newPID != os.Getpid() && processAlive(newPID) && probeAlive(conf.KernelSock) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a stale-alive record must be respawned over from the snapshot (without reclaiming the mismatched pid)")
}

// TestNoPIDRecordStartsNoCore (L2/c8 ③ 回归锚)：没有 core.pid 记录（文件不存在）→
// 完全维持现状——即使快照在也不自发拉核（全新安装不自己长出内核，等用户域 start）。
func TestNoPIDRecordStartsNoCore(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	// 快照在（R 合法 → L1 种子采纳），但无 pid 记录
	seed, rej := NormalizeRuntime([]byte("proxies: []\n"))
	if rej != nil {
		t.Fatalf("seed normalize rejected at %s: %s", rej.Key, rej.Reason)
	}
	if err := os.WriteFile(RuntimeIn(conf.CoreHome), seed.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	startStubServer(t, cfg)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if probeAlive(conf.KernelSock) {
			t.Fatal("no core may spawn without a core.pid record (fresh install stays core-less)")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(cfg.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("core.pid must stay absent without a record (err = %v)", err)
	}
	cl := NewClient(cfg.SockPath, 10*time.Second)
	st, err := cl.Status()
	if err != nil || !st.OK || st.PID != 0 {
		t.Fatalf("Status = %+v err %v, want ok pid=0 (service up, no core)", st, err)
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
