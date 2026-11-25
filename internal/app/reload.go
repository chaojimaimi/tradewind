package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// reloadProbeTimeout 是 add/update 探测内核是否在跑的单次请求超时。
const reloadProbeTimeout = 2 * time.Second

// degradeWaitTimeout 降级重启后等待新内核就绪的窗口（覆盖 start 进程的 1s 首次退避）。
const degradeWaitTimeout = 15 * time.Second

// CoreReachable 探测内核是否在跑（sock 存在且 /version 可达）。只读。
// 按当前会话形态选 socket（批⑥b §4.3 F22）：TUN 形态拨服务的代理入口。
func CoreReachable(home paths.Home) bool {
	return coreReachableAt(coreSockPath(home), store.ReadSecret(home))
}

// ReloadIfRunning 内核运行中 → 重新合成 + PUT /configs?force=true 热重载；
// 未运行 → 仅落盘（调用方已完成），返回 nil。
// 热重载失败 → 降级重启：回收内核进程，由 start 进程的崩溃重启逻辑以新配置拉起
// （M0-plan §7.1 降级路径的最简形态）。
func ReloadIfRunning(ctx context.Context, home paths.Home) error {
	return ReloadIfRunningW(ctx, home, os.Stdout)
}

// ReloadIfRunningW 同 ReloadIfRunning，过程输出走注入 writer。WHY（批③ code-reviewer
// P1-1）：enhance set/clear --json 的单行 stdout 契约会被打进 reload.go 的既有
// fmt.Println 破坏（内核运行中 stdout 变两行，壳端解码失败=保存成功却报失败），
// enhance 编排传 progress（stderr）保持 stdout 纯净；其余调用点维持 stdout 不变。
func ReloadIfRunningW(ctx context.Context, home paths.Home, progress io.Writer) error {
	return reloadIfRunning(ctx, home, progress, true)
}

// ReloadIfRunningPreSynthesized 同 ReloadIfRunning，但跳过内部 Synthesize（仅 PUT/
// 降级）——调用方已显式合成（批④ P3-7 方向反转：bareCoreFallback/reloadAfterEnhance
// 的显式 Synthesize 是内核未运行分支唯一的 runtime.yaml 落盘点，F14；删显式合成=
// 未运行时 runtime.yaml 停留旧内容，故保留显式并收敛双实现到本变体）。
func ReloadIfRunningPreSynthesized(ctx context.Context, home paths.Home) error {
	return reloadIfRunning(ctx, home, os.Stdout, false)
}

// ReloadIfRunningPreSynthesizedW 同 ReloadIfRunningPreSynthesized，输出走注入 writer。
func ReloadIfRunningPreSynthesizedW(ctx context.Context, home paths.Home, progress io.Writer) error {
	return reloadIfRunning(ctx, home, progress, false)
}

// reloadIfRunning 内核运行中 → （可选合成）+ PUT /configs?force=true 热重载；
// 未运行 → 返回 nil（落盘由调用方负责）。热重载失败 → 降级重启：回收内核进程，
// 由 start 进程的崩溃重启逻辑以新配置拉起（M0-plan §7.1 降级路径的最简形态）。
func reloadIfRunning(ctx context.Context, home paths.Home, progress io.Writer, synthesize bool) error {
	// TUN 形态分流（批⑥b §4.4）：root 内核的原生 socket 用户域不可达，热重载改走
	// 服务 reload 动作（判据 state.Core.Tun + 锚存在性，与 degrade 分流同源）
	if coreInTunForm(home) && serviceConfigInstalled() {
		return reloadViaService(ctx, home, progress, synthesize)
	}
	if !CoreReachable(home) {
		return nil // 未运行：仅落盘
	}
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if synthesize {
		if err := Synthesize(home, secret); err != nil {
			return err
		}
	}
	client := core.NewClient(home.SockFile(), secret)
	reloadCtx, cancel := context.WithTimeout(ctx, reloadProbeTimeout)
	err = client.PutConfigs(reloadCtx, home.RuntimeFile())
	cancel()
	if err == nil {
		fmt.Fprintln(progress, "core hot-reloaded")
		return nil
	}
	fmt.Fprintf(progress, "hot reload failed (%v); degrading to core restart\n", err)
	return degradeRestartW(home, secret, progress)
}

// degradeRestart SIGTERM 内核并等待新内核就绪。
// 内核由持锁的 start 进程守护：它监测到退出后会以已更新的 runtime.yaml 重启。
// 若没有 start 进程（孤儿内核场景），等待会超时并明确报错建议手动 start。
func degradeRestart(home paths.Home, secret string) error {
	return degradeRestartW(home, secret, os.Stdout)
}

func degradeRestartW(home paths.Home, secret string, progress io.Writer) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if st.Core.Tun {
		// TUN 形态降级分流（批⑥ §2.4）：root 内核用户态 SIGTERM 无效（F9）→
		// 服务 stop + 服务 start（generation+1），killProcessGracefully 仅用户态保留
		return degradeRestartTun(home, st, secret, progress)
	}
	if st.Core.PID <= 0 || !pidCommandIsCore(st.Core.PID, home) {
		return fmt.Errorf("app: no manageable core process; run `tradewind start` to apply")
	}
	if err := killProcessGracefully(st.Core.PID); err != nil {
		return fmt.Errorf("app: degrade restart: %w", err)
	}
	if waitCoreReady(home.SockFile(), secret, degradeWaitTimeout) {
		fmt.Fprintln(progress, "core restarted with new config")
		return nil
	}
	return errors.New("app: core did not come back after degrade restart; run `tradewind start`")
}

// degradeRestartTun TUN 形态降级重启：服务 stop → 服务 start（generation+1——
// 同 socket 串行请求，stop 已停核、start 新代际准入，幂等仲裁不拦）→ /version
// 轮询就绪。服务不可达 → 显式报错指引（不静默半重启）。
func degradeRestartTun(home paths.Home, st store.State, secret string, progress io.Writer) error {
	if !serviceConfigInstalled() {
		return errors.New("app: TUN core degrade needs the privileged service; run `tradewind tun status` or `tradewind tun reinstall`")
	}
	cl, err := tunNewClient(tunStartRequestTimeout)
	if err != nil {
		return err
	}
	if resp, err := cl.Stop(); err != nil {
		return fmt.Errorf("app: tun degrade stop: %w; run `tradewind tun status`", err)
	} else if !resp.OK {
		return fmt.Errorf("app: tun degrade stop refused: %s", resp.Error)
	}
	resp, err := cl.Start(st.Core.Generation + 1)
	if err != nil {
		return fmt.Errorf("app: tun degrade start: %w; run `tradewind tun status`", err)
	}
	if !resp.OK {
		return fmt.Errorf("app: tun degrade start refused: %s", resp.Error)
	}
	if !waitCoreReady(formSockPath(true, home), secret, degradeWaitTimeout) {
		return errors.New("app: core did not come back after degrade restart; run `tradewind start`")
	}
	fmt.Fprintln(progress, "core restarted with new config")
	return nil
}

// waitCoreReady 轮询给定 socket 上的 /version 直到就绪或超时（批⑥b §4.3：路径由
// 调用方按会话形态给出——start 进程内按 settings、CLI 按 state）。
func waitCoreReady(sockPath, secret string, timeout time.Duration) bool {
	client := core.NewClient(sockPath, secret)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), reloadProbeTimeout)
		_, err := client.Version(ctx)
		cancel()
		if err == nil {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
