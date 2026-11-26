// tun_run.go：start 进程 TUN 分流（批⑥ §2.1 三处分流 + §2.4 degrade 分流）。
// 分流判据 = settings.TunEnabled（startCore/waitLoop）与 state.Core.Tun
// （cleanupResidues/degrade）+ service.json 锚存在性（角态不进 EPERM kill）。
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// 服务锚缝隙（测试指向桩路径——F19：macOS unix socket sun_path 104 字符上限，
// 服务 sock 必须短路径桩）。
var (
	tunServiceSockPath   = service.DefaultSockPath
	tunServiceConfigPath = service.DefaultConfigPath
)

// tunNewClient 服务客户端构造缝隙（默认直连生产锚；测试注入假客户端——绝不触真
// /var/run socket）。锚缺失 → errTunServiceNotInstalled（调用方分流：no-op/报错）。
var tunNewClient = func(timeout time.Duration) (tunServiceClientT, error) {
	if !serviceConfigInstalled() {
		return nil, errTunServiceNotInstalled
	}
	return service.NewClient(tunServiceSockPath, timeout), nil
}

var errTunServiceNotInstalled = errors.New("app: privileged service is not installed; run `tradewind tun install`")

// serviceConfigInstalled service.json 锚存在性（分流判据之一：uninstall 半途等
// 角态 → 不走服务请求，报错指引）。
func serviceConfigInstalled() bool {
	_, err := os.Stat(tunServiceConfigPath)
	return err == nil
}

// tunLegacyHelperPath 是遗留位置的 helper 路径（缝隙：单测指向临时文件，绝不触真
// 系统目录）。
var tunLegacyHelperPath = service.HelperInstallPath

// legacyHelperResidueExists 遗留位置残留检测。Lstat 而非 Stat：悬空链接也算残留
// （卸载脚本的 rm -f 能清）。
func legacyHelperResidueExists() bool {
	_, err := os.Lstat(tunLegacyHelperPath)
	return err == nil
}

// startCoreViaService startCore TUN 分流（§2.1 分流 1）：合成后向服务请求 start
// （携带 generation——postHandshake 同源自增，§3.1 携带方统一=start 进程）。
// 服务端同步握手完成才返回；再 /version 双确认收口。返回 root pid（postHandshake
// 写 state 用）。
func startCoreViaService(ctx context.Context, home paths.Home, secret string) (int, error) {
	if err := Synthesize(home, secret); err != nil {
		return 0, err
	}
	st, err := store.LoadState(home)
	if err != nil {
		return 0, err
	}
	cl, err := tunNewClient(tunStartRequestTimeout)
	if err != nil {
		return 0, err
	}
	resp, err := cl.Start(st.Core.Generation + 1)
	if err != nil {
		return 0, fmt.Errorf("app: tun service start: %w; run `tradewind tun status`", err)
	}
	if !resp.OK {
		return 0, fmt.Errorf("app: tun service start refused: %s", resp.Error)
	}
	// 就绪双确认（语义同 F10：spawn 主体换成服务，/version 轮询收口）；TUN 形态
	// 拨服务的代理入口（批⑥b §4.3）
	if !waitCoreReady(formSockPath(true, home), secret, 10*time.Second) {
		return 0, errors.New("app: core.sock not ready after service start")
	}
	// 跨形态接力（批⑥b §4.8）：必须在 postHandshake 写 last_form 之前判定；socket
	// 显式按本次形态拨代理入口（coreClient 照 state 取，此刻 state 还是上一会话
	// 的形态，会拨到用户态路径）。
	replayIfFormChanged(ctx, home, core.NewClient(formSockPath(true, home), secret), formNameTun)
	fmt.Printf("core spawned via service (pid %d, root)\n", resp.PID)
	return resp.PID, nil
}

// tunStopCoreForward 切换收敛路径的 root core 兜底 stop（tun on/off 共用）：未
// 安装=无核 no-op；请求失败 → 显式失败（走回滚——快失败优先于 45s 假等）。
func tunStopCoreForward(env tunEnv) error {
	if !env.serviceAnchorExists() {
		return nil
	}
	cl, err := env.serviceClient(tunStopRequestTimeout)
	if err != nil {
		return fmt.Errorf("app: tun service stop: %w", err)
	}
	resp, err := cl.Stop()
	if err != nil {
		return fmt.Errorf("app: tun service stop: %w", err)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// tunStopCoreNoOp 回滚步 2 的服务 stop：服务不可达/未安装 → 视为无 root core 可
// 收敛的 no-op（评审三轮 P3——服务不可达本身即状态自洽），不阻塞回滚。
func tunStopCoreNoOp(env tunEnv) error {
	if !env.serviceAnchorExists() {
		return nil
	}
	cl, err := env.serviceClient(tunStopRequestTimeout)
	if err != nil {
		return nil // 未安装 = 无 root core 可收敛
	}
	resp, err := cl.Stop()
	if err != nil {
		fmt.Printf("tun: service unreachable during rollback stop (%v); treated as no-op\n", err)
		return nil
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// tunStopCoreBestEffort `tun off` 路径的尽力而为 stop（逃生口，真机 fold-in 2）：
// 服务不可达（客户端构造失败/拨号失败）→ 没有可请求停下的 root core，打一行
// warning 继续收敛。否则 off 会走回滚（preTun=true）去重拉一个服务不可达时注定
// 拉不起的 TUN 会话，用户被困在"配置说 TUN 开、实际没有任何代理"的态。
// 服务可达但拒绝（!resp.OK）仍视为失败（调用方走回滚）。`tun on` 不走这里——
// 不可达照旧回滚，行为不变。
func tunStopCoreBestEffort(env tunEnv) error {
	if !env.serviceAnchorExists() {
		return nil
	}
	cl, err := env.serviceClient(tunStopRequestTimeout)
	if err != nil {
		fmt.Printf("warning: 特权服务不可达（%v），无法请求停止 root core；继续关闭 TUN\n", err)
		return nil
	}
	resp, err := cl.Stop()
	if err != nil {
		fmt.Printf("warning: 特权服务不可达（%v），无法请求停止 root core；继续关闭 TUN\n", err)
		return nil
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// tunStopServiceCore 关停/回滚收口路径的 root core 收敛：未安装（uninstall 半途
// 等角态）→ 无核 no-op；请求失败 → 显式报错（root core 可能仍被服务托管，不静默）。
func tunStopServiceCore() error {
	if !serviceConfigInstalled() {
		return nil
	}
	cl, err := tunNewClient(tunStopRequestTimeout)
	if err != nil {
		return err
	}
	resp, err := cl.Stop()
	if err != nil {
		return fmt.Errorf("app: tun service stop: %w", err)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// cleanupTunOrphan cleanupResidues TUN 分支（§2.1 分流 3）：root pid 用户态
// SIGTERM=EPERM（F21）→ 走服务 stop 请求收敛。锚缺失（uninstall 半途等角态）→
// 报错指引，不进注定 EPERM 的 kill 尝试（评审三轮 P3——否则 start 崩溃重拉一次
// 即 EPERM 死循环）。服务不可达 → 警告继续（后续 service start 幂等 no-op 收编）。
func cleanupTunOrphan(pid int) error {
	if !serviceConfigInstalled() {
		return fmt.Errorf("app: orphan root core (pid %d) but service anchor missing; run `tradewind tun status` or `tradewind tun reinstall`", pid)
	}
	cl, err := tunNewClient(tunStopRequestTimeout)
	if err != nil {
		return err
	}
	resp, err := cl.Stop()
	if err != nil {
		fmt.Printf("cleanup: service unreachable (%v); root core (pid %d) left for service-start adoption\n", err, pid)
		return nil
	}
	if !resp.OK {
		return fmt.Errorf("app: cleanup tun service stop: %s", resp.Error)
	}
	return nil
}

// tunHealthInterval 健康轮询周期（§2.1 分流 2：每 5s 探 core.sock；变量缝供测试缩短）。
var tunHealthInterval = 5 * time.Second

// tunHealthLoop 服务健康轮询：失联连续 2 次（10s，覆盖服务端 1s 首退避重启窗口）
// → osascript 通知一次/失联周期（恢复即复位）。WHY 只通知不自愈：自愈职责=服务端
// Monitor 复用 + launchd KeepAlive 双层兜底，start 进程不自动重启服务。
func tunHealthLoop(ctx context.Context, home paths.Home) {
	streak, notified := 0, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(tunHealthInterval):
		}
		// 只在 TUN 形态的等待循环里启动，恒探代理入口（批⑥b §4.3）
		if coreReachableAt(formSockPath(true, home), store.ReadSecret(home)) {
			streak, notified = 0, false
			continue
		}
		streak++
		if streak >= 2 && !notified {
			notified = true
			if err := runNotifier("Tradewind", "TUN 服务异常，请检查 tradewind tun status"); err != nil {
				fmt.Printf("watcher: notify: %v\n", err)
			}
		}
	}
}

// waitLoopTun TUN 形态等待循环（§2.1 分流 2）：无 mgr 子进程，Monitor 分支替换为
// 服务健康轮询（后台通知环）。SIGINT/SIGTERM：收敛 watcher/健康环 → 服务 stop →
// 完整还原 → 退出。
func waitLoopTun(ctx context.Context, home paths.Home, generation int, convergeWatcher func()) error {
	healthCtx, cancelHealth := context.WithCancel(ctx)
	go tunHealthLoop(healthCtx, home)
	converge := func() {
		cancelHealth()
		convergeWatcher()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		fmt.Printf("\ncaught %s, shutting down...\n", sig)
		return shutdownSequenceTun(home, generation, converge)
	case <-ctx.Done():
		return shutdownSequenceTun(home, generation, converge)
	}
}

// shutdownSequenceTun TUN 关停统一收敛前置序（对齐 shutdownSequence 同一收敛前置
// 序）：先收敛 watcher/健康环，再 generation 归属校验（F13——被新实例接管 → root
// core 归新会话所有，服务 core 跨代共享，绝不误杀）→ 快照 → 还原 sysproxy →
// 服务 stop → markStopped（Tun 清零=stopped 无形态残留，§3.4）。
func shutdownSequenceTun(home paths.Home, generation int, convergeWatcher func()) error {
	convergeWatcher()
	if generation > 0 && !generationIsMine(home, generation) {
		fmt.Printf("shutdown: state taken over by newer instance (generation %d); skip restore and service stop\n",
			stGeneration(home))
		return nil
	}
	var errs []error
	if err := snapshotBeforeStop(home); err != nil {
		errs = append(errs, err)
	}
	if err := restoreSysProxyIfApplied(home); err != nil {
		errs = append(errs, err)
	}
	if err := tunStopServiceCore(); err != nil {
		errs = append(errs, err)
	}
	if err := markStopped(home); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("app: shutdown: %w", err)
	}
	return nil
}

// tunProbeServiceCore 单次服务就绪探针：status pid>0 + core.sock /version 双确认。
func tunProbeServiceCore(ctx context.Context, home paths.Home) (int, error) {
	cl, err := tunNewClient(tunStatusTimeout)
	if err != nil {
		return 0, err
	}
	resp, err := cl.Status()
	if err != nil {
		return 0, fmt.Errorf("app: tun service status: %w", err)
	}
	if !resp.OK || resp.PID <= 0 {
		return 0, errors.New("app: service reports no managed core")
	}
	// 单次服务就绪探针：/version 双确认恒拨代理入口（本探针只服务 TUN 形态，
	// 批⑥b §4.3）
	if !coreReachableAt(formSockPath(true, home), store.ReadSecret(home)) {
		return 0, errors.New("app: core.sock not reachable")
	}
	return resp.PID, nil
}

// TunStatus 编排 `tradewind tun status`（§2.2）：合并——settings.tun_enabled /
// 服务 status（未安装=清晰降级文案）/ helper 与 root 内核副本两项陈旧状态（R4）/
// core.sock 健康 / 联锁状态。诊断面，不受联锁约束。
func TunStatus(home paths.Home) error {
	lines, err := tunStatusLines(home)
	if err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

// tunStatusLines 状态行组装（人读输出与未来 --json 共用一份数据源）。
func tunStatusLines(home paths.Home) ([]string, error) {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return nil, err
	}
	lines := []string{fmt.Sprintf("tun_enabled: %t", settings.TunEnabled)}
	if !serviceConfigInstalled() {
		lines = append(lines, tunNotInstalledLine())
	} else {
		lines = append(lines, tunServiceStatusLine()...)
		lines = append(lines, tunBinaryStatusLines(home)...)
	}
	// core.sock 健康按当前会话形态探（批⑥b §4.3：TUN 形态即服务的代理入口）
	if coreReachableAt(coreSockPath(home), store.ReadSecret(home)) {
		lines = append(lines, "core.sock:  healthy")
	} else {
		lines = append(lines, "core.sock:  unreachable")
	}
	// 联锁状态（tun_interlock.go）：受控命令默认拒绝这一事实要在诊断面如实可见
	return append(lines, tunInterlockStatusLine()), nil
}

// tunNotInstalledLine 未安装行按联锁状态分两态（批⑥b §4.7 终审 P3-③）：联锁开着
// → 既有安装指引不变；关着 → 点明受控命令默认拒绝与本机自测的确认出口（环境变量
// 名与值复用联锁常量，不写第二份字面量）。
func tunNotInstalledLine() string {
	if tunInterlockOpen() {
		return "service:    not installed（`tradewind tun install` 安装特权服务；`tun on` 会自动安装）"
	}
	return fmt.Sprintf("service:    not installed（联锁关闭：`tradewind tun install` 与 `tun on` 默认拒绝；本机自测需显式确认 %s=%s）",
		tunInterlockAllowEnv, tunInterlockAllowVal)
}

func tunServiceStatusLine() []string {
	cl, err := tunNewClient(tunStatusTimeout)
	if err != nil {
		return []string{"service:    installed (status query failed)"}
	}
	resp, err := cl.Status()
	if err != nil {
		return []string{"service:    installed (unreachable)"}
	}
	if resp.PID > 0 {
		return []string{fmt.Sprintf("service:    running, core pid=%d version=%s", resp.PID, resp.Version)}
	}
	return []string{"service:    running, no managed core"}
}
