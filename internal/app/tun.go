// tun.go：TUN 模式切换编排（批⑥ §2.2）——`tradewind tun on|off|status`。
//
// 编排目标=翻转 settings.tun_enabled 且其余意图全部保持：切换前快照（sysproxy
// Applied）→ 杀旧 start（复用 stop 语义）→ detached spawn `start --no-sysproxy`
// （spawn 恒不带 sysproxy 语义，恢复由编排显式执行——两套机制不并存）→ 就绪判定
// → sysproxy 意图恢复。任一步失败 → 回滚四步（评审 P1-1 + 二轮 P1 闭环）。
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// 切换编排窗口（评审二轮 P2-b）：就绪 45s 覆盖服务端 15s 握手 + spawn/launchd
// 冷启动预算，与壳端 90s 档对齐防假阴性回滚；垂死等待 10s（二轮复核点②）。
// tunReadyWindow 用变量缝——单测缩短窗口（服务探针失败路径的 45s 轮询不可入测）。
var (
	tunReadyWindow         = 45 * time.Second
	tunStatusTimeout       = 5 * time.Second
	tunStopRequestTimeout  = 10 * time.Second // 服务 stop 含 5s StopGrace + SIGKILL
	tunStartRequestTimeout = 30 * time.Second // 服务 start 同步握手 15s + 余量
)

// tunDyingWindow kill 超时后的垂死等待上限（变量缝：测试缩短）。
var tunDyingWindow = 10 * time.Second

// spawnDetachedArgs detached spawn 底座缝隙（测试桩记录 argv——--no-sysproxy
// 契约断言；生产 Setpgid 脱管，父退出不牵连）。
var spawnDetachedArgs = func(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("app: detached start %s %v: %w", exe, args, err)
	}
	go func() { _ = cmd.Wait() }() // 回收子进程（不阻塞编排）
	return nil
}

// spawnTunStartDetached detached spawn `tradewind start --no-sysproxy`（§2.2 步 6，
// on/off 两侧统一——评审二轮 P3）。
func spawnTunStartDetached() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("app: resolve executable: %w", err)
	}
	return spawnDetachedArgs(exe, "start", "--no-sysproxy")
}

// tunServiceClientT 是特权服务客户端缝隙接口（测试注入假客户端——编排测试绝不
// 触真 /var/run socket；生产为 *service.Client）。
type tunServiceClientT interface {
	Start(generation int) (service.Response, error)
	Stop() (service.Response, error)
	Status() (service.Response, error)
}

// tunEnv 是 tun 编排家族的可注入环境（selfUpdateEnv 同款形态）：测试构造假编排，
// 绝不触真系统代理/真 launchd/真实提权。
type tunEnv struct {
	// serviceClient 特权服务客户端工厂（timeout 按动作给）。
	serviceClient func(time.Duration) (tunServiceClientT, error)
	// serviceAnchorExists service.json 锚存在性（安装判据/兜底 stop 判据）。
	serviceAnchorExists func() bool
	// helperMatchesCurrent 安装位 helper sha256 == 当前 CLI（R4 陈旧检测）。
	helperMatchesCurrent func() bool
	// elevate 提权脚本执行缝隙（测试桩——绝不真实提权）。
	elevate func(scriptPath string) error
	// stopStart 杀旧 start（复用 stop 编排语义 20s/StopW）。
	stopStart func(paths.Home) error
	// spawnStart detached spawn `start --no-sysproxy`。
	spawnStart func() error
	// waitServiceCore 单次服务就绪探针（status pid>0 + /version 双确认）。
	waitServiceCore func(context.Context, paths.Home) (int, error)
	// waitUserCore 用户态就绪轮询（回滚重拉/tun off 就绪判定）。
	waitUserCore func(paths.Home, time.Duration) bool
	// ensureGeoip geoip.metadb 前置检查+下载缝隙。
	ensureGeoip func(context.Context, paths.Home) error
	// sysproxyRunner sysproxy 意图恢复 runner（测试注入 fake——绝不触真系统代理）。
	sysproxyRunner sysproxy.Runner
}

// productionTunEnv 组装生产编排环境。
func productionTunEnv() tunEnv {
	return tunEnv{
		serviceClient:        func(timeout time.Duration) (tunServiceClientT, error) { return tunNewClient(timeout) },
		serviceAnchorExists:  serviceConfigInstalled,
		helperMatchesCurrent: helperBinaryMatches,
		elevate:              runElevatedScript,
		stopStart:            func(home paths.Home) error { return StopW(home, os.Stdout) },
		spawnStart:           spawnTunStartDetached,
		waitServiceCore:      tunProbeServiceCore,
		waitUserCore: func(home paths.Home, timeout time.Duration) bool {
			return waitCoreReady(home, store.ReadSecret(home), timeout)
		},
		ensureGeoip:    ensureGeoipMetadb,
		sysproxyRunner: sysproxy.SystemRunner(),
	}
}

// TunOn 编排 `tradewind tun on`：持 mutation lock → 快照/幂等判定 → 前置检查
// （服务安装 + geoip）→ settings 翻转 → 收敛（§2.2 步 5-8）。
func TunOn(ctx context.Context, home paths.Home) error {
	return tunOnWith(ctx, home, productionTunEnv())
}

// TunOff 编排 `tradewind tun off`：对称收敛（settings 翻转 → 服务 stop 兜底 →
// 用户态 core 回归 → sysproxy 意图按切换前 Applied 恢复）。
func TunOff(ctx context.Context, home paths.Home) error {
	return tunOffWith(ctx, home, productionTunEnv())
}

func tunOnWith(ctx context.Context, home paths.Home, env tunEnv) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	settings, preApplied, err := tunSnapshot(home)
	if err != nil {
		return err
	}
	preTun := settings.TunEnabled // 切换前值（幂等 fall-through 时 == true——回滚不得翻转）
	if settings.TunEnabled {
		// 幂等需实际形态探针（batch6 review P2-2）：仅 settings 说开不算——壳端
		// 90s SIGTERM 恰落在步 4 落盘后步 6 spawn 前的 kill 窗口时，重试会输出
		// "already enabled" 假成功而 TUN 全无。服务双探针（status pid>0 + /version）
		// 确认 root core 在跑才 no-op；探测失败 = 落入 converge 补收敛（绝不假成功）。
		if _, err := env.waitServiceCore(ctx, home); err == nil {
			fmt.Println("tun: already enabled; nothing to do") // 幂等（评审三轮 P3，防双发 bounce）
			return nil
		}
	}
	if err := tunPreflightOn(ctx, home, env); err != nil {
		return err
	}
	if err := saveTunEnabled(home, settings, true); err != nil {
		return err
	}
	return tunConverge(ctx, home, env, settings, preApplied, true, preTun)
}

func tunOffWith(ctx context.Context, home paths.Home, env tunEnv) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	settings, preApplied, err := tunSnapshot(home)
	if err != nil {
		return err
	}
	preTun := settings.TunEnabled // 切换前值（显式传参——同 P2-2 次生修正）
	if !settings.TunEnabled {
		// 幂等需实际形态探针（batch6 review P2-2，对称侧）：core 仍可达 = 漂移态
		// （kill 窗口/旧会话残留——配置说关但 core 在跑）→ 落入 converge 收敛；
		// 不可达才是目标态，no-op 返回（绝不无谓 spawn）。
		if !CoreReachable(home) {
			fmt.Println("tun: already disabled; nothing to do") // 幂等（评审三轮 P3，防双发 bounce）
			return nil
		}
	}
	if err := saveTunEnabled(home, settings, false); err != nil {
		return err
	}
	return tunConverge(ctx, home, env, settings, preApplied, false, preTun)
}

// tunSnapshot 读取切换前形态（settings.tun_enabled + state.SysProxy.Applied）——
// 编排目标="翻转 tun_enabled 且其余意图全部保持"的判定基线。
func tunSnapshot(home paths.Home) (store.Settings, bool, error) {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return store.Settings{}, false, err
	}
	st, err := store.LoadState(home)
	if err != nil {
		return store.Settings{}, false, err
	}
	return settings, st.SysProxy.Applied, nil
}

// saveTunEnabled 翻转 tun_enabled 落盘（Validate 经 SaveSettings；这是唯一写入
// 入口——settings set 白名单封死此键，防漂移态）。
func saveTunEnabled(home paths.Home, settings store.Settings, on bool) error {
	settings.TunEnabled = on
	return store.SaveSettings(home, settings)
}

// tunPreflightOn `tun on` 前置检查（§2.2 步 3 + §3.2）：服务未安装 → 提权安装；
// helper sha256 ≠ 当前 CLI → 提示走 reinstall（本命令不自动重装，避免提权弹窗
// 叠加）；geoip.metadb 缺失 → 钉死来源下载，失败报错不半开 TUN。
func tunPreflightOn(ctx context.Context, home paths.Home, env tunEnv) error {
	if !env.serviceAnchorExists() {
		return tunInstallWith(home, env, false)
	}
	if !env.helperMatchesCurrent() {
		return errors.New("app: 服务二进制已陈旧（self-update 后），请先执行 `tradewind tun reinstall`")
	}
	return env.ensureGeoip(ctx, home)
}

// tunConverge 切换收敛（§2.2 步 5-8）：杀旧 start（超时 → 垂死等待）→ 服务 stop
// 兜底收敛 root core（kill -9 场景旧 start 未及收敛；已起则停、未起 no-op）→
// detached spawn --no-sysproxy → 就绪判定 → sysproxy 意图恢复。任一步失败 → 回滚。
func tunConverge(ctx context.Context, home paths.Home, env tunEnv, settings store.Settings, preApplied, on, preTun bool) error {
	// preTun 显式传参（batch6 review P2-2 次生修正）：幂等 fall-through 时 preTun
	// == on（切换前即目标态、仅缺实际形态）——回滚必须按真实切换前值回写，旧的
	// "!on" 推导会把用户已开的 tun_enabled 翻成 false。
	if err := env.stopStart(home); err != nil {
		if !tunDyingWait(home) {
			// 旧 start 仍持 flock，继续 spawn 会撞幂等 exit 0——放弃并指引
			// （settings 回写一致、无漂移，仅缺手动 start 收敛）
			_ = saveTunEnabled(home, settings, preTun)
			return errors.New("app: 旧 start 进程未在超时+垂死窗口内退出，settings 已回写；请手动执行 `tradewind start` 收敛")
		}
		return tunRollback(ctx, home, env, settings, preApplied, preTun, false)
	}
	if err := tunStopCoreForward(env); err != nil {
		return tunRollback(ctx, home, env, settings, preApplied, preTun, false)
	}
	if err := env.spawnStart(); err != nil {
		return tunRollback(ctx, home, env, settings, preApplied, preTun, false)
	}
	pid, readyErr := tunWaitReady(ctx, home, env, on)
	if readyErr != nil {
		return tunRollback(ctx, home, env, settings, preApplied, preTun, true)
	}
	if err := tunRestoreSysProxy(home, env, settings, preApplied); err != nil {
		return tunRollback(ctx, home, env, settings, preApplied, !on, true)
	}
	tunSuccessPrint(on, pid)
	return nil
}

// tunWaitReady 就绪判定（§2.2 步 7，窗口 45s）：on=轮询服务 status（pid>0 即服务
// 托管 root core——评审 P3：不用 CoreReachable 单判，旧用户态 core 尚存时会假
// 阳性）+ core.sock /version 双确认；off=委派 waitUserCore（root core 已服务 stop
// 收敛，可达即用户态新核）。返回 root pid（on）或 0（off）。
func tunWaitReady(ctx context.Context, home paths.Home, env tunEnv, on bool) (int, error) {
	if !on {
		if !env.waitUserCore(home, tunReadyWindow) {
			return 0, errors.New("app: user-mode core not ready within 45s")
		}
		return 0, nil
	}
	deadline := time.Now().Add(tunReadyWindow)
	for time.Now().Before(deadline) {
		if pid, err := env.waitServiceCore(ctx, home); err == nil && pid > 0 {
			return pid, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0, errors.New("app: TUN core not ready within 45s")
}

// tunRollback 回滚四步（评审 P1-1 + 二轮 P1 闭环版）：① settings 回写切换前值；
// ② 清理已产出物（步 6 spawn 的 start 存活 → stop 语义终止；服务 stop 收敛 root
// core——不可达=no-op 继续，三轮 P3）；③ 确认清理后按切换前形态重拉 + sysproxy
// 意图恢复；④ 任一步失败 → 手工指引（不静默）。
func tunRollback(ctx context.Context, home paths.Home, env tunEnv, settings store.Settings, preApplied, preTun, spawned bool) error {
	fmt.Println("tun: rolling back to previous state...")
	var errs []error
	if err := saveTunEnabled(home, settings, preTun); err != nil {
		errs = append(errs, err)
	}
	if spawned {
		// 不终止则重拉撞 ErrInstanceRunning 幂等 exit 0 静默失效，留下
		// "settings 说关、core 在跑"的漂移态（正是 §2.2 封死双入口要防的态）
		if err := env.stopStart(home); err != nil {
			errs = append(errs, fmt.Errorf("rollback: stop spawned start: %w", err))
		}
	}
	if err := tunStopCoreNoOp(env); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return tunRollbackGuidance("cleanup", errors.Join(errs...))
	}
	if err := env.spawnStart(); err != nil {
		return tunRollbackGuidance("respawn", err)
	}
	if err := tunWaitRollbackReady(ctx, home, env, preTun); err != nil {
		return tunRollbackGuidance("readiness", err)
	}
	if err := tunRestoreSysProxy(home, env, settings, preApplied); err != nil {
		return tunRollbackGuidance("sysproxy restore", err)
	}
	fmt.Printf("tun: rolled back (tun_enabled=%t)\n", preTun)
	return nil
}

// tunWaitRollbackReady 回滚重拉后的就绪判定：按切换前形态分流（preTun=true 即
// 回滚 tun off → 服务探针；false → 用户态 core.sock 轮询）。
func tunWaitRollbackReady(ctx context.Context, home paths.Home, env tunEnv, preTun bool) error {
	if preTun {
		_, err := tunWaitReady(ctx, home, env, true)
		return err
	}
	if !env.waitUserCore(home, tunReadyWindow) {
		return errors.New("app: user-mode core not ready after rollback respawn")
	}
	return nil
}

func tunRollbackGuidance(step string, cause error) error {
	return fmt.Errorf("app: tun 回滚在 %s 阶段失败: %v；请执行 `tradewind tun status` 或手动 `tradewind start` 收敛", step, cause)
}

// tunRestoreSysProxy sysproxy 意图恢复（§2.2 步 8）：切换前 Applied=true → 既有
// sysProxyOnWith 恢复写入（备份链复用；form 按 settings.PacMode 推导——切换不
// 改变用户意图）；false → 保持 off（无动作，绝不触碰 networksetup）。
func tunRestoreSysProxy(home paths.Home, env tunEnv, settings store.Settings, preApplied bool) error {
	if !preApplied {
		return nil
	}
	return sysProxyOnWith(home, settings.MixedPort, env.sysproxyRunner, formFromSettings(settings))
}

func tunSuccessPrint(on bool, pid int) {
	if on {
		fmt.Printf("tun: enabled, core pid=%d (root, via service)\n", pid)
	} else {
		fmt.Println("tun: disabled, user-mode core restored")
	}
}

// tunDyingWait kill 超时后的垂死等待：旧 start 已收 SIGTERM，有界再等窗口让其
// 退出（pidfile 消失 = graceful 退出尾步已释放 flock）；仍存活 → false（放弃）。
func tunDyingWait(home paths.Home) bool {
	deadline := time.Now().Add(tunDyingWindow)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(home.PIDFile()); errors.Is(err, os.ErrNotExist) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
