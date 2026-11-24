package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 服务运行期复核执行目录信任链（review P2）：Install 时的校验只跑一次；此后祖先目录
// 若用户/admin 可写，执行目录可被整个换成符号链接。以下用例经 trustRecorder 驱动
// 复核结果——复核逻辑本身由 ownership_test.go 覆盖。

// TestServiceRunFailsClosedWhenExecDirUntrusted 启动复核不达标 → Run 返回错误
// （CLI exit 1 → launchd 节流重试，R3 fail-closed）：不监听、不收编孤儿（复核先于
// 收编——孤儿内核原样存活）、不 spawn。
func TestServiceRunFailsClosedWhenExecDirUntrusted(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	// 存活的"孤儿"内核 + core.pid：若收编先于复核，它会被 SIGTERM 回收
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
	if err := os.WriteFile(cfg.PIDPath, []byte(strconv.Itoa(orphan.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(cfg)
	trust := allowTrust(srv)
	trust.failWith(errors.New("exec dir entry /usr/local is owned by uid 501 (must be root)"))

	err := srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fail-closed") || !strings.Contains(err.Error(), "/usr/local") {
		t.Fatalf("Run = %v, want a fail-closed ownership error naming /usr/local", err)
	}
	if trust.count() != 1 {
		t.Fatalf("startup re-check calls = %d, want exactly 1 (adoption never reached a spawn)", trust.count())
	}
	if _, err := os.Stat(cfg.SockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service must not listen when the exec dir is untrusted (err = %v)", err)
	}
	if !processAlive(orphan.Process.Pid) {
		t.Fatal("the orphan must be left alone: the re-check runs before adoption")
	}
}

// TestServiceRunFailsClosedWhenHomeUntrusted 内核工作目录复核（批⑥b I2）不达标 →
// Run fail-closed：不监听、不收编孤儿。与执行目录复核同序（先执行目录、后工作目录
// ——exec 计数 1 证明工作目录复核紧跟其后、收编从未发生）。
func TestServiceRunFailsClosedWhenHomeUntrusted(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
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
	if err := os.WriteFile(cfg.PIDPath, []byte(strconv.Itoa(orphan.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(cfg)
	trust := allowTrust(srv)
	trust.failHomeWith(errors.New("service: exec dir entry /tmp/x/service-data is group/other-accessible (0755)"))

	err := srv.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "core home failed the trust check") {
		t.Fatalf("Run = %v, want a fail-closed core-home error", err)
	}
	if trust.count() != 1 || trust.homeCount() != 1 {
		t.Fatalf("re-check calls exec=%d home=%d, want 1/1 (adoption never reached a spawn)", trust.count(), trust.homeCount())
	}
	if _, err := os.Stat(cfg.SockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service must not listen when the core home is untrusted (err = %v)", err)
	}
	if !processAlive(orphan.Process.Pid) {
		t.Fatal("the orphan must be left alone: the re-check runs before adoption")
	}
}

// TestServiceSpawnRechecksExecDir 每次 spawn 前复核：运行中执行目录变得不可信后，
// start 请求被拒并返回明确错误（不 spawn）；恢复后同一代际的 start 可重新准入
// （被拒的尝试不消耗代际）。
func TestServiceSpawnRechecksExecDir(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	trust := startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK {
		t.Fatalf("initial Start = %+v err = %v", first, err)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err) // 进入未托管态（否则下一次 start 走幂等 no-op）
	}
	base := trust.count()

	// 执行目录被换成符号链接
	trust.failWith(errors.New("exec dir entry /usr/local/libexec/tradewind is a symbolic link (refusing)"))
	rejected, err := cl.Start(2)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if rejected.OK || !strings.Contains(rejected.Error, "refusing to spawn") || !strings.Contains(rejected.Error, "symbolic link") {
		t.Fatalf("Start with an untrusted exec dir = %+v, want an explicit refusal", rejected)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may be spawned behind an untrusted exec dir (status pid %d)", st.PID)
	}
	if got := trust.count() - base; got != 1 {
		t.Fatalf("re-check calls for the refused spawn = %d, want exactly 1 (Manager.Spawn never reached)", got)
	}

	// 恢复可信 → 同一代际重新准入
	trust.failWith(nil)
	accepted, err := cl.Start(2)
	if err != nil || !accepted.OK || accepted.PID <= 0 {
		t.Fatalf("Start after the exec dir is sound again = %+v err = %v, want admitted", accepted, err)
	}
}

// TestServiceSpawnRechecksHomeTrust 每次 spawn 前复核内核工作目录（批⑥b I2）：
// 运行中 <core_home> 变得不可信后，start 请求被拒并返回明确错误（不 spawn——
// home 计数恰 +1 证明 Manager.Spawn 的 PreSpawn 从未到达）；恢复后同一代际的
// start 可重新准入。
func TestServiceSpawnRechecksHomeTrust(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	trust := startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	if _, err := cl.Start(1); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	base := trust.homeCount()

	trust.failHomeWith(errors.New("service: exec dir entry /tmp/x/logs is a symbolic link (refusing)"))
	rejected, err := cl.Start(2)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if rejected.OK || !strings.Contains(rejected.Error, "refusing to spawn, core home") {
		t.Fatalf("Start with an untrusted core home = %+v, want an explicit refusal", rejected)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may be spawned behind an untrusted core home (status pid %d)", st.PID)
	}
	if got := trust.homeCount() - base; got != 1 {
		t.Fatalf("home re-check calls for the refused spawn = %d, want exactly 1 (Manager.Spawn never reached)", got)
	}

	trust.failHomeWith(nil)
	accepted, err := cl.Start(2)
	if err != nil || !accepted.OK || accepted.PID <= 0 {
		t.Fatalf("Start after the core home is sound again = %+v err = %v, want admitted", accepted, err)
	}
}

// TestServiceMonitorRespawnRechecksHomeTrust Monitor 崩溃自愈的 respawn 同样复核
// 内核工作目录（PreSpawn 钩子）：内核被杀后钩子拒绝 → 放弃重启（fail-closed），
// 绝不在不可信的 <core_home> 里拉起 root 内核；随后的显式 start 也被拒。
func TestServiceMonitorRespawnRechecksHomeTrust(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	trust := startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK || first.PID <= 0 {
		t.Fatalf("initial Start = %+v err = %v", first, err)
	}
	base := trust.homeCount()

	// <core_home> 被换成符号链接，随后内核崩溃（Monitor 退避 1s 后 respawn）
	trust.failHomeWith(errors.New("service: exec dir entry /tmp/x/service-data is a symbolic link (refusing)"))
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub core: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for trust.homeCount() <= base && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if trust.homeCount() <= base {
		t.Fatal("the Monitor respawn never ran the PreSpawn core-home re-check")
	}
	if probeAlive(conf.KernelSock) {
		t.Fatal("a core was respawned behind an untrusted core home")
	}
	rejected, err := cl.Start(2)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if rejected.OK || !strings.Contains(rejected.Error, "refusing to spawn, core home") {
		t.Fatalf("Start after the refused respawn = %+v, want an explicit refusal", rejected)
	}
}

// TestServiceMonitorRespawnRechecksExecDir Monitor 崩溃自愈的 respawn 同样复核：内核
// 被杀后钩子拒绝 → 放弃重启（fail-closed），绝不经被换掉的执行目录执行；随后的
// 显式 start 也被拒。
func TestServiceMonitorRespawnRechecksExecDir(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	trust := startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK || first.PID <= 0 {
		t.Fatalf("initial Start = %+v err = %v", first, err)
	}
	base := trust.count()

	// 评审 P2 场景：执行目录被换掉，随后内核崩溃（Monitor 退避 1s 后 respawn）
	trust.failWith(errors.New("exec dir tampered"))
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub core: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for trust.count() <= base && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if trust.count() <= base {
		t.Fatal("the Monitor respawn never ran the PreSpawn re-check")
	}
	// respawn 被拒 → 没有新内核起来
	if probeAlive(conf.KernelSock) {
		t.Fatal("a core was respawned behind an untrusted exec dir")
	}
	rejected, err := cl.Start(2)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if rejected.OK || !strings.Contains(rejected.Error, "refusing to spawn") {
		t.Fatalf("Start after the refused respawn = %+v, want an explicit refusal", rejected)
	}
}
