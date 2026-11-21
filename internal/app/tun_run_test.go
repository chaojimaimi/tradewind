package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// stubTunSeams 覆盖 TUN 分流全局缝隙（客户端工厂→假服务；锚→临时文件），
// t.Cleanup 还原。绝不触真 /var/run socket / 真系统锚。
func stubTunSeams(t *testing.T, svc *fakeTunService) string {
	t.Helper()
	old := tunNewClient
	tunNewClient = func(time.Duration) (tunServiceClientT, error) { return svc, nil }
	t.Cleanup(func() { tunNewClient = old })
	anchor := filepath.Join(stubSocketDir(t), "service.json")
	if err := os.WriteFile(anchor, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCfg := tunServiceConfigPath
	tunServiceConfigPath = anchor
	t.Cleanup(func() { tunServiceConfigPath = oldCfg })
	return anchor
}

// versionMux 最小 /version 桩（双确认/探活/就绪轮询共用）。
func versionMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	return mux
}

// listenVersionStub 在指定 home 的 core.sock 上起 /version 桩（startCoreViaService
// 双确认、postHandshake 探活、waitCoreReady 用）。
func listenVersionStub(t *testing.T, home paths.Home) {
	t.Helper()
	ln, err := net.Listen("unix", home.SockFile())
	if err != nil {
		t.Fatalf("listen stub sock: %v", err)
	}
	srv := &http.Server{Handler: versionMux()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(home.SockFile())
	})
}

// TestStartCoreViaServiceGenerationCarried startCore 分流（§2.1 分流 1）：服务 start
// 请求携带 state.Generation+1（§3.1 携带方统一=start 进程）；服务拒收 → 显式报错。
func TestStartCoreViaServiceGenerationCarried(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	st := store.DefaultState()
	st.Core.Generation = 7
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	svc := &fakeTunService{startOK: true, startPID: 4242}
	stubTunSeams(t, svc)
	listenVersionStub(t, home) // /version 双确认

	pid, err := startCoreViaService(context.Background(), home, "stub")
	if err != nil {
		t.Fatalf("startCoreViaService: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want 4242", pid)
	}
	if len(svc.starts) != 1 || svc.starts[0] != 8 {
		t.Fatalf("service start gens = %v, want [8] (state 7 + 1)", svc.starts)
	}

	// 服务拒收 → 报错带指引
	svc2 := &fakeTunService{startRefused: "stale generation"}
	stubTunSeams(t, svc2)
	_, err = startCoreViaService(context.Background(), home, "stub")
	if err == nil || !strings.Contains(err.Error(), "stale generation") {
		t.Fatalf("err = %v, want refusal surfaced", err)
	}
}

// TestRunTunBranchEndToEnd Run TUN 分流端到端（三处分流联动，桩化全部外效）：
// tun_enabled=true → 不 spawn 用户态（root pid 来自服务 status）、state.Core.Tun=true、
// ApplySysProxy 强制 false；SIGINT → 服务 stop + markStopped（Tun 清零）。
func TestRunTunBranchEndToEnd(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	seedRuntimeInputs(t, home)
	settings := store.DefaultSettings()
	settings.TunEnabled = true
	settings.PanelEnabled = false // 跳过网关（避免测试绑真实端口）
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	writeExecutableCore(t, home) // Run 的 VerifyBinary 前置（TUN 模式下不 spawn，仅校验）
	listenVersionStub(t, home)   // startCoreViaService 双确认 + postHandshake 探活

	svc := &fakeTunService{startOK: true, startPID: 424242}
	stubTunSeams(t, svc)

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), home, StartOptions{ApplySysProxy: true})
	}()
	// 轮询 state：TUN 分流已写入（root pid + Tun 标记 + applied 强制 false）
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("Run did not converge state within 15s")
		}
		st, err := store.LoadState(home)
		if err == nil && st.Core.Tun && st.Core.PID == 424242 && !st.SysProxy.Applied {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(svc.starts) != 1 || svc.starts[0] != 1 {
		t.Fatalf("service start gens = %v, want [1]", svc.starts)
	}
	// SIGINT → waitLoopTun 信号分支 → shutdownSequenceTun
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("self sigint: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after SIGINT")
	}
	if svc.stopCount() != 1 {
		t.Fatalf("service stops = %d, want exactly 1 (signal shutdown)", svc.stopCount())
	}
	st, _ := store.LoadState(home)
	if st.Core.Tun || st.Core.Desired != store.DesiredStopped || st.Core.PID != 0 {
		t.Fatalf("state after shutdown = %+v, want stopped + Tun cleared", st.Core)
	}
}

// TestCleanupResiduesTunBranchServiceRequest cleanupResidues TUN 分支（§2.1 分流 3）：
// root 孤儿 → 服务 stop 请求（桩），绝不进用户态 kill（孤儿进程不被触碰）。
func TestCleanupResiduesTunBranchServiceRequest(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	svc := &fakeTunService{startOK: true}
	stubTunSeams(t, svc)
	orphanPID := startCmdlineOrphan(t, home)

	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: orphanPID, Generation: 3, Tun: true}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	if err := cleanupResidues(home); err != nil {
		t.Fatalf("cleanupResidues: %v", err)
	}
	if svc.stopCount() != 1 {
		t.Fatalf("service stops = %d, want exactly 1 (orphan reclaim via service)", svc.stopCount())
	}
	st, _ = store.LoadState(home)
	if st.Core.PID != 0 {
		t.Fatalf("state PID = %d, want cleared", st.Core.PID)
	}
	if !processAlive(orphanPID) {
		t.Fatal("orphan process must not be touched by user-mode kill")
	}
}

// TestCleanupResiduesTunAnchorMissing 锚缺失（uninstall 半途角态）→ 报错指引，
// 不进注定 EPERM 的 kill（评审三轮 P3）。
func TestCleanupResiduesTunAnchorMissing(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	orphanPID := startCmdlineOrphan(t, home)
	old := tunServiceConfigPath
	tunServiceConfigPath = filepath.Join(stubSocketDir(t), "missing.json")
	t.Cleanup(func() { tunServiceConfigPath = old })

	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: orphanPID, Generation: 3, Tun: true}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	err := cleanupResidues(home)
	if err == nil || !strings.Contains(err.Error(), "tun status") {
		t.Fatalf("err = %v, want anchor-missing guidance", err)
	}
	if !processAlive(orphanPID) {
		t.Fatal("orphan must NOT be killed (no EPERM kill attempt in corner state)")
	}
}

// startCmdlineOrphan 起一个命令行含 "-d <home>" 的存活进程（孤儿身份桩；TUN 分支
// 只判身份/存活并走服务请求——桩客户端是 no-op，进程不被触碰）。
func startCmdlineOrphan(t *testing.T, home paths.Home) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while :; do sleep 5; done", "tw-orphan", "-d", home.String())
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn orphan stub: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// TestDegradeRestartTunBranch degrade 分流（§2.4）：root 内核不能用户态 SIGTERM →
// 服务 stop + 服务 start（generation+1）→ /version 就绪。
func TestDegradeRestartTunBranch(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	listenVersionStub(t, home)
	svc := &fakeTunService{startOK: true, startPID: 4343}
	stubTunSeams(t, svc)
	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: 4343, Generation: 5, Tun: true}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}

	var progress strings.Builder
	if err := degradeRestartW(home, "stub", &progress); err != nil {
		t.Fatalf("degradeRestartW tun branch: %v", err)
	}
	if svc.stopCount() != 1 {
		t.Fatalf("stops = %d, want 1", svc.stopCount())
	}
	if len(svc.starts) != 1 || svc.starts[0] != 6 {
		t.Fatalf("start gens = %v, want [6] (generation+1)", svc.starts)
	}
	if !strings.Contains(progress.String(), "core restarted with new config") {
		t.Fatalf("progress = %q", progress.String())
	}
}

// TestDegradeRestartTunUnreachable 服务不可达 → 显式报错指引（不静默半重启）。
func TestDegradeRestartTunUnreachable(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	svc := &fakeTunService{stopErr: errors.New("dial failed")}
	stubTunSeams(t, svc)
	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: 99, Generation: 5, Tun: true}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}

	err := degradeRestartW(home, "stub", os.Stdout)
	if err == nil || !strings.Contains(err.Error(), "tun status") {
		t.Fatalf("err = %v, want guidance", err)
	}
}

// TestWaitLoopTunHealthNotify TUN 健康轮询：失联连续 2 次 → runNotifier 通知一次/
// 失联周期。
func TestWaitLoopTunHealthNotify(t *testing.T) {
	notified := stubWatcherNotifier(t)
	home := paths.Home(stubSocketDir(t)) // 无 sock = 失联
	old := tunHealthInterval
	tunHealthInterval = 30 * time.Millisecond
	t.Cleanup(func() { tunHealthInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tunHealthLoop(ctx, home)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(*notified) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if len(*notified) != 1 {
		t.Fatalf("notified = %v, want exactly one per outage episode", *notified)
	}
	if !strings.Contains((*notified)[0], "tun status") {
		t.Fatalf("notification = %q", (*notified)[0])
	}
}

// TestWatcherTunBranchBeforeAppliedEarlyReturn watcher TUN 分支挂点（§2.3，F14）：
// applied=false 早退之前——tun_enabled + 失联连续 2 tick → 通知；同周期不重复。
func TestWatcherTunBranchBeforeAppliedEarlyReturn(t *testing.T) {
	resetLastApply(t)
	notified := stubWatcherNotifier(t)
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	settings := store.DefaultSettings()
	settings.TunEnabled = true
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	st := store.DefaultState()
	st.SysProxy.Applied = false // sysproxy off 正是 TUN 常见形态（早退分支）
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	w := newWatcher(home, &watcherFakeRunner{})

	if err := w.tick(); err != nil { // tick 1：失联计数 1，不通知
		t.Fatalf("tick 1: %v", err)
	}
	if len(*notified) != 0 {
		t.Fatalf("one failing tick must not notify: %v", *notified)
	}
	if err := w.tick(); err != nil { // tick 2：连续 2 → 通知
		t.Fatalf("tick 2: %v", err)
	}
	if len(*notified) != 1 {
		t.Fatalf("notified = %v, want exactly one", *notified)
	}
	if err := w.tick(); err != nil { // tick 3：同周期不重复通知
		t.Fatalf("tick 3: %v", err)
	}
	if len(*notified) != 1 {
		t.Fatalf("notified = %v, want still one per episode", *notified)
	}
}

// TestWatcherTunReachableResets core 恢复可达 → 失联计数复位（永不误报）。
func TestWatcherTunReachableResets(t *testing.T) {
	resetLastApply(t)
	notified := stubWatcherNotifier(t)
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	settings := store.DefaultSettings()
	settings.TunEnabled = true
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	st := store.DefaultState()
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	listenVersionStub(t, home)
	w := newWatcher(home, &watcherFakeRunner{})
	for i := 0; i < 3; i++ {
		if err := w.tick(); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
	if len(*notified) != 0 {
		t.Fatalf("reachable core must never notify: %v", *notified)
	}
}

// TestSettingsSetTunEnabledRejected settings set 双入口封死（§2.2）：报错指引
// `tradewind tun`，settings 不变。
func TestSettingsSetTunEnabledRejected(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	err := SettingsSet(context.Background(), home, "tun_enabled", "on")
	if err == nil || !strings.Contains(err.Error(), "tradewind tun") {
		t.Fatalf("err = %v, want dual-entry guard error", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must stay untouched by settings set")
	}
}

// TestStatusJSONTunTriState status --json tun 段（§4 三态数据源）：
// disabled / enabled+服务异常（sock 不可达）/ enabled+running。
func TestStatusJSONTunTriState(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	r, err := Status(home)
	if err != nil {
		t.Fatal(err)
	}
	out, err := StatusReport(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if out.Tun.Enabled || out.Tun.Running {
		t.Fatalf("tun = %+v, want disabled zero state", out.Tun)
	}
	// enabled + sock 不可达（服务异常态）
	settings := store.DefaultSettings()
	settings.TunEnabled = true
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	r, _ = Status(home)
	out, _ = StatusReport(home, r)
	if !out.Tun.Enabled || out.Tun.Running {
		t.Fatalf("tun = %+v, want enabled+not-running", out.Tun)
	}
	// enabled + 可达（stub core）
	listenVersionStub(t, home)
	r, _ = Status(home)
	out, _ = StatusReport(home, r)
	if !out.Tun.Enabled || !out.Tun.Running {
		t.Fatalf("tun = %+v, want enabled+running", out.Tun)
	}
}
