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
func CoreReachable(home paths.Home) bool {
	if _, err := os.Stat(home.SockFile()); err != nil {
		return false
	}
	client := core.NewClient(home.SockFile(), store.ReadSecret(home))
	ctx, cancel := context.WithTimeout(context.Background(), reloadProbeTimeout)
	defer cancel()
	_, err := client.Version(ctx)
	return err == nil
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
	if st.Core.PID <= 0 || !pidCommandIsCore(st.Core.PID, home) {
		return fmt.Errorf("app: no manageable core process; run `tradewind start` to apply")
	}
	if err := killProcessGracefully(st.Core.PID); err != nil {
		return fmt.Errorf("app: degrade restart: %w", err)
	}
	if waitCoreReady(home, secret, degradeWaitTimeout) {
		fmt.Fprintln(progress, "core restarted with new config")
		return nil
	}
	return errors.New("app: core did not come back after degrade restart; run `tradewind start`")
}

// waitCoreReady 轮询 /version 直到就绪或超时。
func waitCoreReady(home paths.Home, secret string, timeout time.Duration) bool {
	client := core.NewClient(home.SockFile(), secret)
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
