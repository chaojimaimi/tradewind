package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// watcherFakeRunner 可配置读回态的 networksetup 假执行器（模拟真实系统当前值——
// 干净 applied / 被抢改 / PAC 态），记录全部命令；绝不触碰真实系统代理。
type watcherFakeRunner struct {
	mu          sync.Mutex
	calls       [][]string
	web         store.ProxyState
	secure      store.ProxyState
	socks       store.ProxyState
	bypass      []string
	autoURL     string
	autoEnabled bool
}

func (f *watcherFakeRunner) Run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	switch args[0] {
	case "-getwebproxy":
		return proxyStateOut(f.web), nil
	case "-getsecurewebproxy":
		return proxyStateOut(f.secure), nil
	case "-getsocksfirewallproxy":
		return proxyStateOut(f.socks), nil
	case "-getproxybypassdomains":
		if len(f.bypass) == 0 {
			return "There aren't any bypass domains", nil
		}
		return strings.Join(f.bypass, "\n"), nil
	case "-getautoproxyurl":
		url := f.autoURL
		if url == "" {
			url = "(null)"
		}
		enabled := "No"
		if f.autoEnabled {
			enabled = "Yes"
		}
		return fmt.Sprintf("URL: %s\nEnabled: %s", url, enabled), nil
	case "-setwebproxy":
		// 写命令同步改内部"系统态"（真实 networksetup 语义）——恢复后期望读回干净值
		port, _ := strconv.Atoi(args[3])
		f.web = store.ProxyState{Enabled: true, Server: args[2], Port: port}
	case "-setsecurewebproxy":
		port, _ := strconv.Atoi(args[3])
		f.secure = store.ProxyState{Enabled: true, Server: args[2], Port: port}
	case "-setsocksfirewallproxy":
		port, _ := strconv.Atoi(args[3])
		f.socks = store.ProxyState{Enabled: true, Server: args[2], Port: port}
	case "-setwebproxystate", "-setsecurewebproxystate", "-setsocksfirewallproxystate":
		off := args[2] == "off"
		p := &f.web
		switch args[0] {
		case "-setsecurewebproxystate":
			p = &f.secure
		case "-setsocksfirewallproxystate":
			p = &f.socks
		}
		p.Enabled = !off
	case "-setproxybypassdomains":
		if len(args) == 3 && args[2] == "Empty" {
			f.bypass = nil
		} else {
			f.bypass = append([]string(nil), args[2:]...)
		}
	case "-setautoproxyurl":
		f.autoURL = args[2]
	case "-setautoproxystate":
		f.autoEnabled = args[2] == "on"
	}
	return "", nil
}

func proxyStateOut(p store.ProxyState) string {
	return fmt.Sprintf("Enabled: %s\nServer: %s\nPort: %d", boolYesNo(p.Enabled), p.Server, p.Port)
}

// writeCalls 统计写入命令数（-set*；巡检探测的 -get* 不算）。
func writeCalls(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && strings.HasPrefix(c[0], "-set") {
			n++
		}
	}
	return n
}

func boolYesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

func (f *watcherFakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// hijackWeb 把 web 代理改为外部值（模拟其他工具抢改）。
func (f *watcherFakeRunner) hijackWeb() {
	f.web = store.ProxyState{Enabled: true, Server: "9.9.9.9", Port: 8888}
}

// watcherHome 造 applied=true 的守护测试目录（默认 manual 会话 + 干净期望态）。
func watcherHome(t *testing.T, form string) (paths.Home, *watcherFakeRunner) {
	t.Helper()
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	backup := []store.ServiceBackup{{
		Service: "Wi-Fi",
		Web:     store.ProxyState{Enabled: true, Server: "10.0.0.1", Port: 8888},
		Secure:  store.ProxyState{Enabled: false},
		Socks:   store.ProxyState{Enabled: false},
		Bypass:  []string{"*.local"},
	}}
	st := store.DefaultState()
	st.SysProxy = store.SysProxyState{Applied: true, Services: backup, Form: form}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	// 干净 applied 态：三项指向 127.0.0.1:mixed + 缺省 bypass
	fake := &watcherFakeRunner{
		web:    store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort},
		secure: store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort},
		socks:  store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort},
		bypass: []string{"127.0.0.1", "localhost", "*.local", "169.254/16"},
	}
	return home, fake
}

// notifierRecorder 记录通知缝隙收到的调用。WHY 要锁：通知可能来自后台协程
// （tunHealthLoop 等），测试主协程同时在轮询读取——无锁切片一写一读是数据竞争
// （`go test -race` 实报于 TestWaitLoopTunHealthNotify，批⑥遗留的测试卫生问题）。
type notifierRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *notifierRecorder) add(title, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, title+"/"+body)
	return nil
}

// List 返回到目前为止的调用记录（副本，可安全地在通知协程仍在跑时轮询）。
func (r *notifierRecorder) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// stubWatcherNotifier 替换通知缝隙并记录调用；t.Cleanup 还原。
func stubWatcherNotifier(t *testing.T) *notifierRecorder {
	t.Helper()
	rec := &notifierRecorder{}
	old := runNotifier
	runNotifier = rec.add
	t.Cleanup(func() { runNotifier = old })
	return rec
}

// resetLastApply 归零进程内 grace 变量（包级状态跨测试污染防御）。
func resetLastApply(t *testing.T) {
	t.Helper()
	sysProxyLastApplyUnix.Store(0)
	t.Cleanup(func() { sysProxyLastApplyUnix.Store(0) })
}

// TestWatcherManualHijackMatrix 抢改矩阵（manual）：web 被抢改 → 检测命中 →
// 恢复按期望重放（三项 + 生效 bypass）→ 事件计数/时间戳/通知各一次。
func TestWatcherManualHijackMatrix(t *testing.T) {
	resetLastApply(t)
	notified := stubWatcherNotifier(t)
	home, fake := watcherHome(t, sysproxy.FormManual)
	fake.hijackWeb()
	w := newWatcher(home, fake)

	if err := w.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	var hasSetWeb, hasBypass bool
	for _, c := range fake.calls {
		if c[0] == "-setwebproxy" && c[2] == "127.0.0.1" && c[3] == fmt.Sprint(store.DefaultMixedPort) {
			hasSetWeb = true
		}
		if c[0] == "-setproxybypassdomains" {
			hasBypass = true
		}
	}
	if !hasSetWeb || !hasBypass {
		t.Fatalf("restore replay incomplete; calls %v", fake.calls)
	}
	st, _ := store.LoadState(home)
	if st.SysProxy.HijackEvents != 1 || st.SysProxy.LastRestore <= 0 {
		t.Fatalf("events = %+v, want HijackEvents=1 LastRestore>0", st.SysProxy)
	}
	if got := notified.List(); len(got) != 1 {
		t.Fatalf("notified = %v, want exactly one", got)
	}
	// 干净态复检：恢复后期望命中，零新增写入命令（巡检探测照常）
	before := fake.callCount()
	if err := w.tick(); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if writeCalls(fake.calls[before:]) != 0 {
		t.Fatalf("clean state must not trigger write commands: %v", fake.calls[before:])
	}
	if st2, _ := store.LoadState(home); st2.SysProxy.HijackEvents != 1 {
		t.Fatalf("clean tick must not increment events: %+v", st2.SysProxy)
	}
}

// TestWatcherPACHijackMatrix 抢改矩阵（PAC）：autoproxy URL 被改/开关被关 → 命中 →
// 按 PAC 重放（url+state on+bypass）；期望 URL 以当前 settings gateway_port 为准。
func TestWatcherPACHijackMatrix(t *testing.T) {
	resetLastApply(t)
	stubWatcherNotifier(t)
	home, fake := watcherHome(t, sysproxy.FormPAC)
	fake.autoURL = "http://127.0.0.1:9999/pac" // 期望 39097
	fake.autoEnabled = true
	w := newWatcher(home, fake)
	if err := w.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	var hasURL, hasStateOn bool
	for _, c := range fake.calls {
		if c[0] == "-setautoproxyurl" && c[2] == "http://127.0.0.1:39097/pac" {
			hasURL = true
		}
		if c[0] == "-setautoproxystate" && c[2] == "on" {
			hasStateOn = true
		}
	}
	if !hasURL || !hasStateOn {
		t.Fatalf("PAC restore replay incomplete; calls %v", fake.calls)
	}
	// URL 对但开关被关 → 同样命中
	fake2 := &watcherFakeRunner{
		web:         store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort},
		secure:      store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort},
		socks:       store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort},
		bypass:      []string{"127.0.0.1", "localhost", "*.local", "169.254/16"},
		autoURL:     "http://127.0.0.1:39097/pac",
		autoEnabled: false,
	}
	w2 := newWatcher(home, fake2)
	if err := w2.tick(); err != nil {
		t.Fatalf("tick disabled: %v", err)
	}
	// 干净 PAC 态：零写入命令（巡检探测照常）
	fake3 := &watcherFakeRunner{
		autoURL:     "http://127.0.0.1:39097/pac",
		autoEnabled: true,
	}
	w3 := newWatcher(home, fake3)
	if err := w3.tick(); err != nil {
		t.Fatalf("clean PAC tick: %v", err)
	}
	if writeCalls(fake3.calls) != 0 {
		t.Fatalf("clean PAC state must not trigger write commands: %v", fake3.calls)
	}
}

// TestWatcherLegacyFormTreatedAsManual legacy 备份（Form 空）按 manual 检测/恢复
// ——恢复写手工三项，绝不写 autoproxy 命令（旧备份零扰动锚）。
func TestWatcherLegacyFormTreatedAsManual(t *testing.T) {
	resetLastApply(t)
	stubWatcherNotifier(t)
	home, fake := watcherHome(t, "") // Form 空 = legacy
	fake.hijackWeb()
	w := newWatcher(home, fake)
	if err := w.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, c := range fake.calls {
		if c[0] == "-setautoproxyurl" || c[0] == "-setautoproxystate" {
			t.Fatalf("legacy form restore must not touch autoproxy: %v", c)
		}
	}
	var hasSetWeb bool
	for _, c := range fake.calls {
		if c[0] == "-setwebproxy" && c[2] == "127.0.0.1" {
			hasSetWeb = true
		}
	}
	if !hasSetWeb {
		t.Fatalf("legacy restore must replay manual commands; calls %v", fake.calls)
	}
}

// TestWatcherGraceSkipsDetection grace：本进程最近写入窗口内不判抢改（防自家
// Apply 误报——tick 零调用零事件）。
func TestWatcherGraceSkipsDetection(t *testing.T) {
	resetLastApply(t)
	home, fake := watcherHome(t, sysproxy.FormManual)
	fake.hijackWeb()
	sysProxyLastApplyUnix.Store(time.Now().Unix())
	w := newWatcher(home, fake)
	if err := w.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if fake.callCount() != 0 {
		t.Fatalf("grace window must make zero calls; got %v", fake.calls)
	}
	st, _ := store.LoadState(home)
	if st.SysProxy.HijackEvents != 0 {
		t.Fatalf("grace window must not record events: %+v", st.SysProxy)
	}
}

// TestWatcherLockBusySkipsTick LOCK_NB 忙跳过：mutation.lock 被持（CLI 编排在途）
// → tick 立即返回 nil 零调用（绝不等待）。
func TestWatcherLockBusySkipsTick(t *testing.T) {
	resetLastApply(t)
	home, fake := watcherHome(t, sysproxy.FormManual)
	fake.hijackWeb()
	lock, err := acquireMutationLockFor(home, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer lock.Release()
	w := newWatcher(home, fake)
	start := time.Now()
	if err := w.tick(); err != nil {
		t.Fatalf("busy tick: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("busy tick waited %s, want immediate skip", elapsed)
	}
	if fake.callCount() != 0 {
		t.Fatalf("busy tick must make zero calls; got %v", fake.calls)
	}
}

// TestWatcherAppliedFalseZeroAction Applied=false 时零动作（用户主动 off/裸跑不被
// 打扰——即便系统态与任何期望不符）。
func TestWatcherAppliedFalseZeroAction(t *testing.T) {
	resetLastApply(t)
	home, fake := watcherHome(t, sysproxy.FormManual)
	st, _ := store.LoadState(home)
	st.SysProxy.Applied = false
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	fake.hijackWeb()
	fake.autoURL = "whatever"
	w := newWatcher(home, fake)
	if err := w.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if fake.callCount() != 0 {
		t.Fatalf("applied=false must make zero calls; got %v", fake.calls)
	}
}

// TestWatcherAndCLIOffConcurrent watcher × CLI off 并发（锁互斥收口）：off 持锁
// 期间 tick 忙跳过；off 完成后 applied=false → tick 零动作。终态一致无交错残留。
func TestWatcherAndCLIOffConcurrent(t *testing.T) {
	resetLastApply(t)
	stubWatcherNotifier(t)
	home, fake := watcherHome(t, sysproxy.FormManual)
	fake.hijackWeb()

	var offFake watcherFakeRunner
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// CLI SysProxyOff 纳锁语义（SysProxyOff 包装层同构：35s 有界等待）
		var lock *MutationLock
		var err error
		for i := 0; i < 100 && lock == nil; i++ {
			if lock, err = TryAcquireMutationLock(home); err != nil {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if lock == nil {
			return
		}
		defer lock.Release()
		_ = sysProxyOffWith(home, &offFake)
	}()
	// 并发 ticks：持锁者要么 watcher 要么 off，互斥串行
	for i := 0; i < 5; i++ {
		w := newWatcher(home, fake)
		if err := w.tick(); err != nil {
			t.Fatalf("concurrent tick: %v", err)
		}
	}
	wg.Wait()
	st, err := store.LoadState(home)
	if err != nil {
		t.Fatal(err)
	}
	if st.SysProxy.Applied {
		t.Fatalf("final state must be applied=false (off completed); got %+v", st.SysProxy)
	}
}

// TestTryAcquireMutationLockBusy LOCK_NB 语义：已持锁时非阻塞获取返回 ErrMutationBusy。
func TestTryAcquireMutationLockBusy(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	first, err := TryAcquireMutationLock(home)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	start := time.Now()
	_, err = TryAcquireMutationLock(home)
	if err == nil || !strings.Contains(err.Error(), "another mutation") {
		t.Fatalf("err = %v, want busy sentinel", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("non-blocking acquire waited %s", elapsed)
	}
}

// --- 退出序收敛（评审二轮 P1-2：signal 分支 + giveup 回调两调用点先收敛再 fullShutdown）---

// TestShutdownSequenceConvergesWatcherBeforeFullShutdown 真实 watcher 起跑 + 收敛：
// shutdownSequence 返回时 watcher goroutine 必已退出（此后零 tick 与还原交错）。
// 两个关停调用点共用本序，覆盖 signal 与 giveup 双路径。
func TestShutdownSequenceConvergesWatcherBeforeFullShutdown(t *testing.T) {
	home, fake := watcherHome(t, sysproxy.FormManual)
	// 纪律 6（绝不触真系统代理）：本测试经 shutdownSequence→fullShutdown→
	// restoreSysProxyIfApplied（生产实现恒 SystemRunner，不可注入）——state 必须
	// applied=false 使还原分支早退；本测试断言的是收敛序，applied 态无关。
	st, err := store.LoadState(home)
	if err != nil {
		t.Fatal(err)
	}
	st.SysProxy.Applied = false
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	fake.web = store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort}
	fake.secure = store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort}
	fake.socks = store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: store.DefaultMixedPort}

	var order []string
	var mu sync.Mutex
	record := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	startWatcher(ctx, home, &wg)
	converge := func() {
		record("converge-start")
		cancel()
		wg.Wait()
		record("converge-done")
	}
	mgr := core.NewManager(core.Config{SockPath: home.SockFile()})
	if err := shutdownSequence(home, mgr, 0, converge); err != nil {
		t.Fatalf("shutdownSequence: %v", err)
	}
	record("shutdown-done")
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(order, ",")
	if !strings.HasPrefix(joined, "converge-start,converge-done") || !strings.HasSuffix(joined, "shutdown-done") {
		t.Fatalf("order = %q, want converge before shutdown-done", joined)
	}
	if st, _ := store.LoadState(home); st.SysProxy.Applied {
		t.Fatalf("state must be applied=false after shutdown; got %+v", st.SysProxy)
	}
}

// writeExecutableCore 写可验证的假内核（bin + sidecar 64hex 摘要），脚本立即退出
// ——giveup 路径测试用（重启预算耗尽触发 Monitor giveup 回调）。
func writeExecutableCore(t *testing.T, home paths.Home) {
	t.Helper()
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexit 7\n"
	if err := os.WriteFile(home.Bin(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(script)))
	if err := os.WriteFile(home.Sidecar(), []byte(sum+"  "+home.Bin()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestWaitLoopGiveupPathConvergesWatcher 给弃路径端到端：内核立即崩溃 → 重启预算
// 耗尽 → Monitor giveup 回调先收敛 watcher 再 fullShutdown → waitLoop 返回错误。
func TestWaitLoopGiveupPathConvergesWatcher(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	seedRuntimeInputs(t, home)
	writeExecutableCore(t, home)

	mgr := core.NewManager(core.Config{
		BinPath:        home.Bin(),
		SidecarPath:    home.Sidecar(),
		DataDir:        home.String(),
		RuntimePath:    home.RuntimeFile(),
		SockPath:       home.SockFile(),
		Secret:         "stub",
		LogPath:        home.LogFile(),
		StopGrace:      time.Second,
		RestartBackoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
	})
	if err := mgr.Spawn(); err != nil {
		t.Fatalf("spawn fake core: %v", err)
	}

	var convergeCount int
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	startWatcher(ctx, home, &wg)
	converge := func() {
		mu.Lock()
		convergeCount++
		mu.Unlock()
		cancel()
		wg.Wait()
	}

	err := waitLoop(ctx, home, mgr, 1, false, converge)
	if err == nil || !strings.Contains(err.Error(), "restart failed") {
		t.Fatalf("waitLoop = %v, want giveup error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if convergeCount != 1 {
		t.Fatalf("converge count = %d, want exactly 1", convergeCount)
	}
	st, err := store.LoadState(home)
	if err != nil {
		t.Fatal(err)
	}
	if st.Core.Desired != store.DesiredStopped || st.SysProxy.Applied {
		t.Fatalf("state after giveup = %+v, want stopped & unapplied", st)
	}
}

// TestWaitLoopSignalPathConvergesWatcher 信号路径端到端：SIGINT → 收敛 → 完整还原
// → waitLoop 返回 nil。
func TestWaitLoopSignalPathConvergesWatcher(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	seedRuntimeInputs(t, home)
	mgr := core.NewManager(core.Config{SockPath: home.SockFile()})

	var convergeCount int
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	startWatcher(ctx, home, &wg)
	converge := func() {
		mu.Lock()
		convergeCount++
		mu.Unlock()
		cancel()
		wg.Wait()
	}

	done := make(chan error, 1)
	go func() { done <- waitLoop(ctx, home, mgr, 1, false, converge) }()
	time.Sleep(100 * time.Millisecond) // 让 waitLoop 进入 select
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("self sigint: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitLoop signal path = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waitLoop did not return after SIGINT")
	}
	mu.Lock()
	defer mu.Unlock()
	if convergeCount != 1 {
		t.Fatalf("converge count = %d, want exactly 1", convergeCount)
	}
	if st, _ := store.LoadState(home); st.Core.Desired != store.DesiredStopped {
		t.Fatalf("desired = %q, want stopped", st.Core.Desired)
	}
}

// --- P3-7 双合成收敛：运行/未运行两分支行为等价（runtime.yaml 字节不变）---

// TestPreSynthesizedEquivalenceNotRunning 未运行分支：显式 Synthesize 落盘 +
// PreSynthesized 仅早退——runtime.yaml 与显式合成结果字节一致（未运行分支唯一
// 落盘点保留）。
func TestPreSynthesizedEquivalenceNotRunning(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)

	// 分支一：Synthesize + PreSynthesized（bareCoreFallback/reloadAfterEnhance 现序）
	if err := bareCoreFallback(context.Background(), home); err != nil {
		t.Fatalf("bareCoreFallback: %v", err)
	}
	a, err := os.ReadFile(home.RuntimeFile())
	if err != nil {
		t.Fatalf("runtime A: %v", err)
	}
	// 分支二：删除 current（等价场景）→ 对照 Synthesize+ReloadIfRunning（旧实现）
	if err := Synthesize(home, mustSecret(t, home)); err != nil {
		t.Fatalf("synthesize B: %v", err)
	}
	if err := ReloadIfRunning(context.Background(), home); err != nil {
		t.Fatalf("reload B: %v", err)
	}
	b, err := os.ReadFile(home.RuntimeFile())
	if err != nil {
		t.Fatalf("runtime B: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("runtime.yaml bytes differ between pre-synthesized and legacy paths")
	}
}

// TestPreSynthesizedEquivalenceRunning 运行分支：PreSynthesized 照常 PUT 热重载
// （一次），runtime.yaml 字节不变。
func TestPreSynthesizedEquivalenceRunning(t *testing.T) {
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedTwoProfiles(t, home)

	if err := Synthesize(home, mustSecret(t, home)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(home.RuntimeFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReloadIfRunningPreSynthesized(context.Background(), home); err != nil {
		t.Fatalf("pre-synthesized reload: %v", err)
	}
	if len(reloads) != 1 || reloads[0] != "PUT" {
		t.Fatalf("reloads = %v, want exactly one PUT", reloads)
	}
	after, err := os.ReadFile(home.RuntimeFile())
	if err != nil || string(after) != string(before) {
		t.Fatalf("runtime.yaml must be untouched by pre-synthesized reload: %v", err)
	}
}

// mustSecret EnsureSecret 的测试形态（测试体内显式失败）。
func mustSecret(t *testing.T, home paths.Home) string {
	t.Helper()
	secret, err := store.EnsureSecret(home)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// reloadAfterEnhance 收敛后路径回归：显式合成 + PreSynthesized，未运行仅落盘。
func TestReloadAfterEnhanceUsesPreSynthesized(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	withMihomoCheckStub(t, false)
	var progress strings.Builder
	content := []byte("- DOMAIN-SUFFIX,example.com,DIRECT\n")
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", content, &progress); err != nil {
		t.Fatalf("EnhanceSet: %v", err)
	}
	runtime, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !strings.Contains(string(runtime), "DOMAIN-SUFFIX,example.com,DIRECT") {
		t.Fatalf("runtime missing slot rule: %v", err)
	}
	if !strings.Contains(progress.String(), "config reloaded") {
		t.Fatalf("progress = %q, want reload receipt", progress.String())
	}
}
