package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// tunCallLog 记录 tunEnv 会话类缝隙的调用序列——逃生口/重收敛的用例据此断言
// "走了哪条路、先后次序如何"，而不只看终态。
type tunCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *tunCallLog) record(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, step)
}

func (l *tunCallLog) sequence() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.calls, ",")
}

// newLoggedTunEnv 在 newTunTestEnv 之上把 stopStart/spawnStart/waitServiceCore/
// waitUserCore 换成带记录的实现（默认全部成功）；spawns 与 spawnStart 同步计数。
func newLoggedTunEnv(t *testing.T) (tunEnv, *fakeTunService, *tunCallLog, *int, *fakeNetRunner) {
	t.Helper()
	env, svc, _, _, fake := newTunTestEnv(t)
	log := &tunCallLog{}
	spawns := 0
	env.stopStart = func(paths.Home) error { log.record("stopStart"); return nil }
	env.spawnStart = func() error { log.record("spawnStart"); spawns++; return nil }
	env.waitServiceCore = func(context.Context, paths.Home) (int, error) {
		log.record("waitServiceCore")
		return svc.startPID, nil
	}
	env.waitUserCore = func(paths.Home, time.Duration) bool { log.record("waitUserCore"); return true }
	return env, svc, log, &spawns, fake
}

func seedTunOnHome(t *testing.T, home paths.Home) {
	t.Helper()
	seedTunOffHome(t, home)
	if err := saveTunEnabled(home, tunSettings(t, home), true); err != nil {
		t.Fatal(err)
	}
}

// --- a. tun off 逃生口：服务不可达时仍能成功 ---

// TestTunOffToleratesUnreachableService off × 服务不可达（客户端构造失败 / stop 请求
// 传输失败）→ 不回滚、settings=false、spawn 一次用户态 start、就绪判定走
// waitUserCore。此前这里会回滚去重拉 TUN 会话并把用户困住。
func TestTunOffToleratesUnreachableService(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*tunEnv, *fakeTunService)
	}{
		{"client unavailable", func(env *tunEnv, _ *fakeTunService) {
			env.serviceClient = func(time.Duration) (tunServiceClientT, error) {
				return nil, errors.New("service: dial /var/run/x.sock: no such file or directory")
			}
		}},
		{"stop request fails in transit", func(_ *tunEnv, svc *fakeTunService) {
			svc.stopErr = errors.New("service: read response: i/o timeout")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := paths.Home(stubSocketDir(t))
			seedTunOnHome(t, home)
			env, svc, log, spawns, fake := newLoggedTunEnv(t)
			c.arrange(&env, svc)

			if err := tunOffWith(context.Background(), home, env); err != nil {
				t.Fatalf("tun off with an unreachable service must succeed (escape hatch): %v", err)
			}
			if tunSettings(t, home).TunEnabled {
				t.Fatal("tun_enabled must end up false")
			}
			// 不回滚：回滚会在正向 spawn 之后再拉一次 TUN start
			if *spawns != 1 {
				t.Fatalf("spawn count = %d, want 1 (no rollback respawn)", *spawns)
			}
			if seq := log.sequence(); seq != "stopStart,spawnStart,waitUserCore" {
				t.Fatalf("call sequence = %q, want stopStart,spawnStart,waitUserCore (readiness via the user-mode probe)", seq)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("networksetup calls = %v, want zero (applied=false before the switch)", fake.calls)
			}
		})
	}
}

// TestTunOffStillRollsBackWhenServiceRefuses 服务可达但拒绝（ok:false）不是"不可达"
// ——仍视为失败走回滚：settings 回到 true，正向 spawn 都不发生（拒绝发生在它之前），
// 更不会进入用户态就绪判定。
func TestTunOffStillRollsBackWhenServiceRefuses(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOnHome(t, home)
	env, svc, log, spawns, _ := newLoggedTunEnv(t)
	svc.stopRefused = "internal error"

	err := tunOffWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("err = %v, want a rollback failure with guidance", err)
	}
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("a refused stop must roll tun_enabled back to true")
	}
	if *spawns != 0 {
		t.Fatalf("spawn count = %d, want 0 (the refusal precedes the forward spawn)", *spawns)
	}
	if seq := log.sequence(); seq != "stopStart" {
		t.Fatalf("call sequence = %q, want the flow to stop right after stopStart", seq)
	}
}

// TestTunOnStillRollsBackWhenServiceUnreachable on 路径行为不变：服务不可达 → 回滚，
// 绝不以 TUN 开启收场。
func TestTunOnStillRollsBackWhenServiceUnreachable(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, _, log, spawns, _ := newLoggedTunEnv(t)
	env.serviceClient = func(time.Duration) (tunServiceClientT, error) {
		return nil, errors.New("service: dial: connection refused")
	}

	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("rollback closed loop should return nil: %v", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun on must not end with TUN enabled when the service is unreachable")
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want 1 (rollback respawn of the user-mode start)", *spawns)
	}
	if seq := log.sequence(); seq != "stopStart,spawnStart,waitUserCore" {
		t.Fatalf("call sequence = %q, want the rollback to converge on the user-mode probe", seq)
	}
}

// --- b. tun reinstall 在 TUN 开启期间重收敛会话 ---

// TestTunReinstallReconvergesTunSession TUN 开启 × reinstall → 安装验证通过后
// stopStart → spawnStart → waitServiceCore；不写 settings、不碰 sysproxy。
func TestTunReinstallReconvergesTunSession(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOnHome(t, home)
	seedFakeCore(t, home)      // 生成脚本要算内核摘要（批⑥b §4.5）
	writeAppliedState(t, home) // 切换前 applied=true：重收敛不得触碰系统代理
	env, svc, log, _, fake := newLoggedTunEnv(t)
	settingsBefore, err := os.ReadFile(home.SettingsFile())
	if err != nil {
		t.Fatal(err)
	}

	if err := tunInstallWith(home, env, true); err != nil {
		t.Fatalf("tunInstallWith(reinstall): %v", err)
	}
	if seq := log.sequence(); seq != "stopStart,spawnStart,waitServiceCore" {
		t.Fatalf("call sequence = %q, want stopStart,spawnStart,waitServiceCore", seq)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("networksetup calls = %v, want zero (re-convergence never touches sysproxy)", fake.calls)
	}
	settingsAfter, err := os.ReadFile(home.SettingsFile())
	if err != nil || string(settingsAfter) != string(settingsBefore) {
		t.Fatalf("settings.yaml must not be rewritten (err = %v)", err)
	}
	if svc.stopCount() < 1 {
		t.Fatal("reinstall must still stop the managed root core before replacing the service")
	}
}

// TestTunReinstallNoReconvergeWhenTunOff 未开启 TUN → 行为不变：不触碰会话。
func TestTunReinstallNoReconvergeWhenTunOff(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	seedFakeCore(t, home) // 生成脚本要算内核摘要（批⑥b §4.5）
	env, _, log, spawns, _ := newLoggedTunEnv(t)

	if err := tunInstallWith(home, env, true); err != nil {
		t.Fatalf("tunInstallWith(reinstall): %v", err)
	}
	if seq := log.sequence(); seq != "" || *spawns != 0 {
		t.Fatalf("no session call expected with TUN off: sequence = %q spawns = %d", seq, *spawns)
	}
}

// TestTunReinstallReconvergeFailureGivesGuidance 重收敛任一步失败 → 报错并给出
// `tradewind tun off && tradewind tun on` 指引；settings 不被改写。
func TestTunReinstallReconvergeFailureGivesGuidance(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOnHome(t, home)
	seedFakeCore(t, home) // 生成脚本要算内核摘要（批⑥b §4.5）
	env, _, log, _, _ := newLoggedTunEnv(t)
	env.waitServiceCore = func(context.Context, paths.Home) (int, error) {
		log.record("waitServiceCore")
		return 0, errors.New("service reports no managed core")
	}
	old := tunReadyWindow
	tunReadyWindow = 100 * time.Millisecond
	t.Cleanup(func() { tunReadyWindow = old })

	err := tunInstallWith(home, env, true)
	if err == nil || !strings.Contains(err.Error(), "tradewind tun off && tradewind tun on") {
		t.Fatalf("err = %v, want the recovery guidance", err)
	}
	if seq := log.sequence(); seq != "stopStart,spawnStart,waitServiceCore" {
		t.Fatalf("call sequence = %q", seq)
	}
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("a failed re-convergence must not rewrite settings")
	}
}

// TestTunReinstallSkipsReconvergeWhenInstallFails 提权失败或安装验证失败 → 不重收敛。
func TestTunReinstallSkipsReconvergeWhenInstallFails(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*tunEnv)
		want    string
	}{
		{"elevation fails", func(env *tunEnv) {
			env.elevate = func(string) error { return errors.New("app: 提权脚本执行失败（exit status 1）") }
		}, "提权脚本执行失败"},
		{"install verification fails", func(env *tunEnv) {
			env.waitForServiceReady = func() error { return errors.New("app: 特权服务安装后 12s 内不可达") }
		}, "不可达"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := paths.Home(stubSocketDir(t))
			seedTunOnHome(t, home)
			seedFakeCore(t, home) // 生成脚本要算内核摘要（批⑥b §4.5）
			env, _, log, spawns, _ := newLoggedTunEnv(t)
			c.arrange(&env)

			err := tunInstallWith(home, env, true)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to carry %q", err, c.want)
			}
			if seq := log.sequence(); seq != "" || *spawns != 0 {
				t.Fatalf("no session call expected when the install itself failed: sequence = %q spawns = %d", seq, *spawns)
			}
		})
	}
}

// TestTunInstallNonForceDoesNotReconverge 重收敛只属于 reinstall：非 force 的
// `tun install`（含 `tun on` 的前置安装——它随后自己走 converge）不重收敛。
func TestTunInstallNonForceDoesNotReconverge(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOnHome(t, home)
	seedFakeCore(t, home) // 生成脚本要算内核摘要（批⑥b §4.5）
	env, _, log, spawns, _ := newLoggedTunEnv(t)
	var installed bool
	env.serviceAnchorExists = func() bool { return installed }
	env.elevate = func(string) error { installed = true; return nil }

	if err := tunInstallWith(home, env, false); err != nil {
		t.Fatalf("tunInstallWith(install): %v", err)
	}
	if seq := log.sequence(); seq != "" || *spawns != 0 {
		t.Fatalf("non-force install must not re-converge the session: sequence = %q spawns = %d", seq, *spawns)
	}
}

// --- c. 卸载识别「只有遗留残留」 ---

// TestTunUninstallRemovesLegacyResidueOnly 没有 service.json、只有遗留位置的 root
// 属主 helper（真机失败安装的现状）→ 仍走提权清理，且不发服务 stop 请求。
func TestTunUninstallRemovesLegacyResidueOnly(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	env, svc, _, _, _ := newTunTestEnv(t)
	env.serviceAnchorExists = func() bool { return false }
	env.legacyResidueExists = func() bool { return true }
	elevated := 0
	env.elevate = func(string) error { elevated++; return nil }

	if err := tunUninstallWith(home, env); err != nil {
		t.Fatalf("tunUninstallWith(legacy only): %v", err)
	}
	if elevated != 1 {
		t.Fatalf("elevated uninstall runs = %d, want 1 (the residue is root-owned)", elevated)
	}
	if svc.stopCount() != 0 {
		t.Fatalf("service stops = %d, want 0 (no anchor → nothing to stop)", svc.stopCount())
	}
}

// TestTunUninstallNoopWhenNothingInstalled 锚与遗留残留都不在 → no-op，零提权。
func TestTunUninstallNoopWhenNothingInstalled(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	env, svc, _, _, _ := newTunTestEnv(t)
	env.serviceAnchorExists = func() bool { return false }
	env.legacyResidueExists = func() bool { return false }
	elevated := 0
	env.elevate = func(string) error { elevated++; return nil }

	if err := tunUninstallWith(home, env); err != nil {
		t.Fatalf("tunUninstallWith(nothing installed): %v", err)
	}
	if elevated != 0 || svc.stopCount() != 0 {
		t.Fatalf("nothing-installed uninstall must be a zero-action no-op: elevated=%d stops=%d", elevated, svc.stopCount())
	}
}

// TestLegacyHelperResidueExistsUsesLstat 遗留残留检测用 Lstat：普通文件算残留，
// 悬空链接也算（rm -f 能清），不存在才算干净。
func TestLegacyHelperResidueExistsUsesLstat(t *testing.T) {
	dir := stubSocketDir(t)
	residue := filepath.Join(dir, "legacy-helper")
	old := tunLegacyHelperPath
	tunLegacyHelperPath = residue
	t.Cleanup(func() { tunLegacyHelperPath = old })

	if legacyHelperResidueExists() {
		t.Fatal("absent path must read as no residue")
	}
	if err := os.WriteFile(residue, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !legacyHelperResidueExists() {
		t.Fatal("a regular file at the legacy path must read as residue")
	}
	if err := os.Remove(residue); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing-target"), residue); err != nil {
		t.Fatal(err)
	}
	if !legacyHelperResidueExists() {
		t.Fatal("a dangling link must count as residue (Stat would miss it)")
	}
}
