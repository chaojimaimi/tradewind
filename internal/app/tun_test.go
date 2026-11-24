package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// fakeTunService 假特权服务客户端（记录 start/stop/status 调用——编排侧仅兜底
// stop + 就绪探针触达服务；绝不触真 /var/run socket）。
type fakeTunService struct {
	mu           sync.Mutex
	starts       []int
	stops        int
	statuses     int
	startOK      bool
	startPID     int
	startRefused string
	// stopErr 非空 = stop 的传输层失败（服务不可达）；stopRefused 非空 = 服务可达
	// 但拒绝（ok:false）——两条路径在 tun off 的处置不同（fold-in 2 逃生口）。
	stopErr     error
	stopRefused string
}

func (f *fakeTunService) Start(gen int) (service.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, gen)
	if f.startRefused != "" {
		return service.Response{OK: false, Error: f.startRefused}, nil
	}
	if !f.startOK {
		return service.Response{OK: true, PID: f.startPID, AlreadyRunning: true}, nil
	}
	return service.Response{OK: true, PID: f.startPID}, nil
}

func (f *fakeTunService) Stop() (service.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return service.Response{}, f.stopErr
	}
	if f.stopRefused != "" {
		return service.Response{OK: false, Error: f.stopRefused}, nil
	}
	return service.Response{OK: true}, nil
}

func (f *fakeTunService) Status() (service.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses++
	return service.Response{OK: true, PID: f.startPID, Version: "fake"}, nil
}

func (f *fakeTunService) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeTunService) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// newTunTestEnv 组装假编排环境（桩服务/spawn/stop/就绪/geoip/提权 + 假 runner——
// 绝不触真系统代理/真 launchd/真实提权）。返回环境、假服务、spawn 计数、stopStart
// 计数、sysproxy 假 runner。
func newTunTestEnv(t *testing.T) (tunEnv, *fakeTunService, *int, *int, *fakeNetRunner) {
	t.Helper()
	svc := &fakeTunService{startOK: true, startPID: 42000}
	spawns := 0
	stopStarts := 0
	fake := &fakeNetRunner{}
	env := tunEnv{
		serviceClient:        func(time.Duration) (tunServiceClientT, error) { return svc, nil },
		serviceAnchorExists:  func() bool { return true },
		legacyResidueExists:  func() bool { return false },
		helperMatchesCurrent: func() bool { return true },
		coreMatchesCurrent:   func(paths.Home) bool { return true },
		serviceReachable:     func() bool { return true },
		waitForServiceReady:  func() error { return nil },
		safeExecDirCandidates: func() ([]string, error) {
			return service.ExecDirCandidates(), nil // 默认全安全；过滤用例自行覆盖
		},
		elevate: func(string) error { return nil },
		stopStart: func(paths.Home) error {
			stopStarts++
			return nil
		},
		spawnStart: func() error {
			spawns++
			return nil
		},
		waitServiceCore: func(context.Context, paths.Home) (int, error) { return svc.startPID, nil },
		waitUserCore:    func(paths.Home, time.Duration) bool { return true },
		ensureGeoip:     func(context.Context, paths.Home) error { return nil },
		sysproxyRunner:  fake,
	}
	return env, svc, &spawns, &stopStarts, fake
}

// simulateGracefulStop 覆盖 env.stopStart：计数 + 模拟旧 start 的优雅关停语义
// （真实 StopW 后旧 start 经 fullShutdown 还原 sysproxy → applied=false——编排
// 步 8 意图恢复的前置时序；桩编排无真实旧 start，须显式仿真）。
func simulateGracefulStop(env *tunEnv, counter *int) {
	env.stopStart = func(home paths.Home) error {
		*counter++
		if st, err := store.LoadState(home); err == nil && st.SysProxy.Applied {
			st.SysProxy.Applied = false
			_ = store.SaveState(home, st)
		}
		return nil
	}
}

// seedTunOffHome 造 tun off 基线（默认 settings + secret）。
func seedTunOffHome(t *testing.T, home paths.Home) {
	t.Helper()
	seedRuntimeInputs(t, home)
}

// tunSettings 读 settings（测试断言用）。
func tunSettings(t *testing.T, home paths.Home) store.Settings {
	t.Helper()
	s, err := store.LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTunSpawnContractNoSysproxy --no-sysproxy spawn 契约（§2.2/sysproxy 语义钉死）：
// 编排 spawn 恒为 `start --no-sysproxy`（绝不隐式 apply 系统代理——两套机制不并存）。
func TestTunSpawnContractNoSysproxy(t *testing.T) {
	var got []string
	var gotExe string
	old := spawnDetachedArgs
	spawnDetachedArgs = func(exe string, args ...string) error {
		gotExe, got = exe, args
		return nil
	}
	t.Cleanup(func() { spawnDetachedArgs = old })
	if err := spawnTunStartDetached(); err != nil {
		t.Fatalf("spawnTunStartDetached: %v", err)
	}
	if gotExe == "" || len(got) != 2 || got[0] != "start" || got[1] != "--no-sysproxy" {
		t.Fatalf("spawn args = %q %v, want [start --no-sysproxy]", gotExe, got)
	}
}

// TestTunOnFlow 正向流：settings 翻转 + spawn 契约 + 兜底服务 stop + sysproxy 意图
// 保持（切换前 applied=false → 恢复阶段零 networksetup 调用）。
func TestTunOnFlow(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, stopStarts, fake := newTunTestEnv(t)

	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOnWith: %v", err)
	}
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be flipped on")
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want exactly 1 detached start", *spawns)
	}
	if svc.stopCount() < 1 {
		t.Fatalf("service stops = %d, want >=1 (forward convergence stop)", svc.stopCount())
	}
	if *stopStarts != 1 {
		t.Fatalf("stopStart count = %d, want 1 (kill old start)", *stopStarts)
	}
	// 切换前 applied=false → 意图恢复阶段零 networksetup 调用
	if len(fake.calls) != 0 {
		t.Fatalf("networksetup calls = %v, want zero (preApplied=false)", fake.calls)
	}
}

// TestTunOnIdempotent 已是目标态 → no-op（评审三轮 P3，防壳端异步双发）：零 spawn、
// 零服务调用、settings 不动。
func TestTunOnIdempotent(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, _, _ := newTunTestEnv(t)
	if err := saveTunEnabled(home, tunSettings(t, home), true); err != nil {
		t.Fatal(err)
	}
	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOnWith idempotent: %v", err)
	}
	if *spawns != 0 || svc.stopCount() != 0 || svc.startCount() != 0 {
		t.Fatalf("idempotent on must be zero-action: spawns=%d stops=%d starts=%d", *spawns, svc.stopCount(), svc.startCount())
	}
}

// TestTunOnRollbackOnReadyTimeout 失败回滚闭环（就绪判定超时）：settings 回写切换
// 前值 + 清理步 6 产物（stop 存活 start + 服务 stop）+ 前形态重拉（评审二轮 P1
// "新 start 存活"用例）+ 零 sysproxy 扰动。
func TestTunOnRollbackOnReadyTimeout(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, stopStarts, fake := newTunTestEnv(t)
	env.waitServiceCore = func(context.Context, paths.Home) (int, error) {
		return 0, errors.New("not ready")
	}
	old := tunReadyWindow
	tunReadyWindow = 200 * time.Millisecond
	t.Cleanup(func() { tunReadyWindow = old })

	// 回滚四步全部成功 = 闭环返回 nil（形态回到切换前）
	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("rollback closed loop should succeed: %v", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be rolled back to false")
	}
	// spawn 2 次（正向步 6 + 回滚步 3 重拉）；stopStart 2 次（步 5 杀旧 + 回滚清理
	// 存活的新 start——不终止则重拉撞 flock 幂等 exit 0 静默失效）
	if *spawns != 2 {
		t.Fatalf("spawn count = %d, want 2 (forward + rollback respawn)", *spawns)
	}
	if *stopStarts != 2 {
		t.Fatalf("stopStart count = %d, want 2 (old start + spawned start cleanup)", *stopStarts)
	}
	if svc.stopCount() < 1 {
		t.Fatalf("service stops = %d, want >=1 (rollback root core convergence)", svc.stopCount())
	}
	if len(fake.calls) != 0 {
		t.Fatalf("networksetup calls = %v, want zero", fake.calls)
	}
}

// TestTunOnRollbackWithSysproxyIntent 回滚 + sysproxy 意图恢复（切换前 Applied=true）：
// 重拉后按意图重放（fake runner 记录，绝不触真系统代理）。
func TestTunOnRollbackWithSysproxyIntent(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	writeAppliedState(t, home) // 切换前 applied=true（fake runner 环境，红线安全）
	env, _, spawns, stopStarts, fake := newTunTestEnv(t)
	env.waitServiceCore = func(context.Context, paths.Home) (int, error) {
		return 0, errors.New("not ready")
	}
	old := tunReadyWindow
	tunReadyWindow = 200 * time.Millisecond
	t.Cleanup(func() { tunReadyWindow = old })
	// 模拟旧 start 的优雅关停时序（真实 StopW 后旧 start 经 fullShutdown 还原
	// sysproxy → applied=false）：编排步 8 的意图恢复依赖该前置
	simulateGracefulStop(&env, stopStarts)

	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("rollback closed loop should succeed: %v", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must roll back to false")
	}
	if *spawns != 2 {
		t.Fatalf("spawn count = %d, want 2", *spawns)
	}
	var hasSetWeb bool
	for _, c := range fake.calls {
		if c[0] == "-setwebproxy" && c[2] == "127.0.0.1" {
			hasSetWeb = true
		}
	}
	if !hasSetWeb {
		t.Fatalf("rollback must restore sysproxy intent; calls: %v", fake.calls)
	}
	if st := appliedState(t, home); !st.Applied {
		t.Fatal("state must be applied=true after intent restore")
	}
}

// TestTunOnKillTimeoutAbandons kill 超时 + 垂死等待仍不退 → 放弃不 spawn（旧 start
// 仍持 flock；settings 已回写一致、无漂移，仅缺手动 start 收敛）。
func TestTunOnKillTimeoutAbandons(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, _, spawns, _, _ := newTunTestEnv(t)
	env.stopStart = func(paths.Home) error {
		return errors.New("did not exit within 20s")
	}
	if err := os.WriteFile(home.PIDFile(), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := tunDyingWindow
	tunDyingWindow = 80 * time.Millisecond
	t.Cleanup(func() { tunDyingWindow = old })

	err := tunOnWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "tradewind start") {
		t.Fatalf("err = %v, want abandon with manual-start guidance", err)
	}
	if *spawns != 0 {
		t.Fatalf("spawn count = %d, want 0 (abandon, no spawn while flock held)", *spawns)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be rolled back to false")
	}
}

// TestTunOnKillTimeoutThenDyingExits kill 超时但垂死期内退出 → 按回滚序重拉（二轮
// 复核点②细化：settings 已回写、正向 spawn 未执行，仅回滚重拉 1 次）。
func TestTunOnKillTimeoutThenDyingExits(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, _, spawns, _, _ := newTunTestEnv(t)
	env.stopStart = func(paths.Home) error {
		return errors.New("did not exit within 20s")
	}
	if err := os.WriteFile(home.PIDFile(), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := tunDyingWindow
	tunDyingWindow = 2 * time.Second
	t.Cleanup(func() { tunDyingWindow = old })
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.Remove(home.PIDFile()) // 模拟旧 start 优雅退出尾步（pidfile 删除）
	}()

	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("dying-exit rollback should close loop: %v", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must roll back to false")
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want 1 (rollback respawn only)", *spawns)
	}
}

// TestTunOnHelperStale 服务已装但 helper sha256 陈旧 → 报错指引 reinstall（不自动
// 重装，避免提权弹窗叠加）；settings 不翻转、零动作。
func TestTunOnHelperStale(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, _, _ := newTunTestEnv(t)
	env.helperMatchesCurrent = func() bool { return false }

	err := tunOnWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "reinstall") {
		t.Fatalf("err = %v, want stale-helper guidance", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must stay false on stale helper")
	}
	if *spawns != 0 || svc.stopCount() != 0 {
		t.Fatal("stale helper must abort before any action")
	}
}

// TestTunOnAutoInstallsWhenMissing 服务未安装 → 提权安装（桩 elevate）后继续。
func TestTunOnAutoInstallsWhenMissing(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, _, spawns, _, _ := newTunTestEnv(t)
	var installed bool
	env.serviceAnchorExists = func() bool { return installed }
	env.elevate = func(string) error {
		installed = true // 桩安装：helper install 语义（锚由 root helper 自写）
		return nil
	}
	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOnWith with auto-install: %v", err)
	}
	if !installed {
		t.Fatal("elevated install must run when anchor missing")
	}
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be on after install+converge")
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want 1", *spawns)
	}
}

// TestTunOnGeoipFailureAborts geoip.metadb 前置检查失败 → 报错终止（不半开 TUN）：
// settings 不翻转、零 spawn、零服务调用。
func TestTunOnGeoipFailureAborts(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, _, _ := newTunTestEnv(t)
	env.ensureGeoip = func(context.Context, paths.Home) error {
		return errors.New("geoip.metadb download failed")
	}

	err := tunOnWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "geoip") {
		t.Fatalf("err = %v, want geoip abort error", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must stay false (no half-open TUN)")
	}
	if *spawns != 0 || svc.stopCount() != 0 {
		t.Fatal("geoip failure must abort before any action")
	}
}

// TestTunOffFlow 对称流：settings 翻转 + 服务 stop（root core 收敛）+ 用户态重拉
// + 零 sysproxy 扰动（切换前 applied=false）。
func TestTunOffFlow(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	if err := saveTunEnabled(home, tunSettings(t, home), true); err != nil {
		t.Fatal(err)
	}
	env, svc, spawns, stopStarts, fake := newTunTestEnv(t)

	if err := tunOffWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOffWith: %v", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be flipped off")
	}
	if *spawns != 1 || *stopStarts != 1 || svc.stopCount() < 1 {
		t.Fatalf("off flow: spawns=%d stopStarts=%d stops=%d", *spawns, *stopStarts, svc.stopCount())
	}
	if len(fake.calls) != 0 {
		t.Fatalf("networksetup calls = %v, want zero (preApplied=false)", fake.calls)
	}
}

// TestTunOffRestoresSysproxyIntent 切换前 Applied=true → off 后意图恢复（§2.2 步 8，
// fake runner 记录——绝不触真系统代理）。
func TestTunOffRestoresSysproxyIntent(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	if err := saveTunEnabled(home, tunSettings(t, home), true); err != nil {
		t.Fatal(err)
	}
	writeAppliedState(t, home)
	env, _, _, stopStarts, fake := newTunTestEnv(t)
	simulateGracefulStop(&env, stopStarts)

	if err := tunOffWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOffWith: %v", err)
	}
	var hasSetWeb bool
	for _, c := range fake.calls {
		if c[0] == "-setwebproxy" {
			hasSetWeb = true
		}
	}
	if !hasSetWeb {
		t.Fatalf("off must restore sysproxy intent; calls: %v", fake.calls)
	}
	if st := appliedState(t, home); !st.Applied {
		t.Fatal("state must be applied=true after intent restore")
	}
}

// TestTunOffIdempotent 已 off → no-op（零动作）。
func TestTunOffIdempotent(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, _, _ := newTunTestEnv(t)
	if err := tunOffWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOffWith idempotent: %v", err)
	}
	if *spawns != 0 || svc.stopCount() != 0 {
		t.Fatal("idempotent off must be zero-action")
	}
}

// TestTunStatusLines 三方合并（未安装降级文案 / 服务态 / sock 健康）。
func TestTunStatusLines(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	lines, err := tunStatusLines(home)
	if err != nil {
		t.Fatalf("tunStatusLines: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "tun_enabled: false") || !strings.Contains(joined, "not installed") {
		t.Fatalf("status lines missing degraded service info:\n%s", joined)
	}
	// 已安装 + 桩服务：running 行（锚缝隙指向临时文件）
	svc := &fakeTunService{startOK: true, startPID: 4242}
	old := tunNewClient
	tunNewClient = func(time.Duration) (tunServiceClientT, error) { return svc, nil }
	t.Cleanup(func() { tunNewClient = old })
	oldCfg := tunServiceConfigPath
	anchor := filepath.Join(home.String(), "service.json")
	if err := os.WriteFile(anchor, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	tunServiceConfigPath = anchor
	t.Cleanup(func() { tunServiceConfigPath = oldCfg })

	lines, err = tunStatusLines(home)
	if err != nil {
		t.Fatalf("tunStatusLines installed: %v", err)
	}
	joined = strings.Join(lines, "\n")
	if !strings.Contains(joined, "core pid=4242") {
		t.Fatalf("status missing service core pid:\n%s", joined)
	}
	// `{}` 锚不可用 → 陈旧项如实报"unknown"并指引 reinstall（不假装最新）
	if !strings.Contains(joined, "helper:     unknown") || !strings.Contains(joined, "tun reinstall") {
		t.Fatalf("status must flag an unreadable anchor instead of pretending it is current:\n%s", joined)
	}
}

// TestTunInstallIdempotentNoOp 已装且 helper 最新 → no-op（零提权弹窗）。
func TestTunInstallIdempotentNoOp(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	env, _, _, _, _ := newTunTestEnv(t)
	elevated := 0
	env.elevate = func(string) error {
		elevated++
		return nil
	}
	if err := tunInstallWith(home, env, false); err != nil {
		t.Fatalf("tunInstallWith: %v", err)
	}
	if elevated != 0 {
		t.Fatal("idempotent install must not elevate")
	}
}

// TestTunUninstallStopsServiceFirst uninstall 前先服务 stop（停 root core 语义）。
func TestTunUninstallStopsServiceFirst(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	env, svc, _, _, _ := newTunTestEnv(t)
	if err := tunUninstallWith(home, env); err != nil {
		t.Fatalf("tunUninstallWith: %v", err)
	}
	if svc.stopCount() != 1 {
		t.Fatalf("service stops = %d, want exactly 1 (stop before uninstall)", svc.stopCount())
	}
}

// TestShQuoteEscapesSingleQuote 单引号转义（含单引号用户名的路径）。
func TestShQuoteEscapesSingleQuote(t *testing.T) {
	got := shQuote(`/Users/o'brien/data`)
	if got != `'/Users/o'\''brien/data'` {
		t.Fatalf("shQuote = %q", got)
	}
}

// TestEnsureGeoipMetadb 前置检查三态（§3.2）：缺失 → 下载+sha256 校验+落位；
// 摘要失配 → 报错且不落位（钉死来源轮换的失败出口，R11）；已存在 → 零下载。
func TestEnsureGeoipMetadb(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	fixture := []byte("fake-geoip-bytes")
	h := sha256.Sum256(fixture)
	sum := hex.EncodeToString(h[:])
	fetch := func(_ context.Context, _ *http.Client, _ string, dest string, _ int64) error {
		return os.WriteFile(dest, fixture, 0o600)
	}
	// 摘要失配：报错不落位
	err := ensureGeoipMetadbWith(context.Background(), home, "http://example/geoip.metadb", sum+"deadbeef", fetch)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha mismatch failure", err)
	}
	if _, err := os.Stat(home.GeoIP()); !os.IsNotExist(err) {
		t.Fatal("geoip must not be placed on mismatch")
	}
	// 匹配：落位
	if err := ensureGeoipMetadbWith(context.Background(), home, "http://example/geoip.metadb", sum, fetch); err != nil {
		t.Fatalf("ensure with matching sha: %v", err)
	}
	if _, err := os.Stat(home.GeoIP()); err != nil {
		t.Fatalf("geoip placed: %v", err)
	}
	// 已存在：fetch 不再被调用（零下载）
	called := false
	if err := ensureGeoipMetadbWith(context.Background(), home, "http://example/geoip.metadb", sum,
		func(_ context.Context, _ *http.Client, _ string, _ string, _ int64) error {
			called = true
			return nil
		}); err != nil {
		t.Fatalf("ensure when present: %v", err)
	}
	if called {
		t.Fatal("existing geoip.metadb must skip download")
	}
}

// TestTunOnIdempotentRequiresRunningCore 幂等假成功封堵（batch6 review P2-2）：
// settings 说开但 root core 未跑（kill 窗口残留）→ 落入 converge 补收敛（spawn），
// 绝不输出 "already enabled" 假成功。
func TestTunOnIdempotentRequiresRunningCore(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	if err := saveTunEnabled(home, tunSettings(t, home), true); err != nil {
		t.Fatal(err)
	}
	env, svc, spawns, _, _ := newTunTestEnv(t)
	// 两段式探针：幂等判定（首次）失败 = 触发 fall-through；converge 就绪判定
	//（后续）成功 = 正向流闭环，不触发回滚
	probes := 0
	env.waitServiceCore = func(context.Context, paths.Home) (int, error) {
		probes++
		if probes == 1 {
			return 0, errors.New("service reports no managed core")
		}
		return svc.startPID, nil
	}

	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOnWith fall-through: %v", err)
	}
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must stay on (fall-through keeps user intent)")
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want 1 (must converge, not fake no-op)", *spawns)
	}
}

// TestTunOffIdempotentRequiresCoreDown 幂等假成功封堵（对称侧）：settings 说关但
// core 仍可达（漂移态）→ 落入 converge 收敛（服务 stop + 用户态重拉）。
func TestTunOffIdempotentRequiresCoreDown(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)    // tun_enabled=false
	listenVersionStub(t, home) // core 可达 = 漂移态
	env, svc, spawns, _, _ := newTunTestEnv(t)

	if err := tunOffWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOffWith fall-through: %v", err)
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want 1 (drift must converge)", *spawns)
	}
	if svc.stopCount() != 1 {
		t.Fatalf("service stops = %d, want 1 (root core convergence)", svc.stopCount())
	}
}

// TestTunOnFallThroughRestoreFailureRollbackKeepsIntent 回滚矩阵（batch6 review
// 复评残留——第 5 处 tunRollback 调用点）：fall-through（settings 已开、core 未跑）
// × tunRestoreSysProxy 失败交集下，回滚必须保持 TunEnabled=true——旧的 "!on" 推导
// 在该点会把用户已开的 TUN 静默翻关重拉用户态（漂移态复活）。
// WHY 现有族未覆盖：正常路径 preTun==!on 死等价，全绿不证明该点正确。
func TestTunOnFallThroughRestoreFailureRollbackKeepsIntent(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	if err := saveTunEnabled(home, tunSettings(t, home), true); err != nil {
		t.Fatal(err)
	}
	writeAppliedState(t, home) // 切换前 applied=true（fake runner 环境，红线安全）
	env, svc, spawns, stopStarts, _ := newTunTestEnv(t)
	// 两段式探针：幂等判定（首次）失败 = fall-through；就绪判定（后续）成功
	probes := 0
	env.waitServiceCore = func(context.Context, paths.Home) (int, error) {
		probes++
		if probes == 1 {
			return 0, errors.New("service reports no managed core")
		}
		return svc.startPID, nil
	}
	// restore 必败（写入命令失败——applySysProxyRunner 回滚备份后显式报错）
	env.sysproxyRunner = &fakeNetRunner{applyFailure: "permission denied"}
	// 旧 start 优雅关停时序仿真（编排步 8 的前置：applied 清 false）
	simulateGracefulStop(&env, stopStarts)

	err := tunOnWith(context.Background(), home, env)
	// restore 在正向与回滚两阶段持续失败 → 回滚步 4 手工指引（不静默）
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("err = %v, want rollback guidance", err)
	}
	// THE 断言：回滚 settings 回写必须保持用户意图（旧 "!"on 推导在此写 false）
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must stay true after fall-through restore-failure rollback")
	}
	if *spawns != 2 {
		t.Fatalf("spawn count = %d, want 2 (forward + rollback respawn)", *spawns)
	}
}
