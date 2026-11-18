package app

import (
	"context"
	"errors"
	"fmt"
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
	if !CoreReachable(home) {
		return nil // 未运行：仅落盘
	}
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if err := Synthesize(home, secret); err != nil {
		return err
	}
	client := core.NewClient(home.SockFile(), secret)
	reloadCtx, cancel := context.WithTimeout(ctx, reloadProbeTimeout)
	err = client.PutConfigs(reloadCtx, home.RuntimeFile())
	cancel()
	if err == nil {
		fmt.Println("core hot-reloaded")
		return nil
	}
	fmt.Printf("hot reload failed (%v); degrading to core restart\n", err)
	return degradeRestart(home, secret)
}

// degradeRestart SIGTERM 内核并等待新内核就绪。
// 内核由持锁的 start 进程守护：它监测到退出后会以已更新的 runtime.yaml 重启。
// 若没有 start 进程（孤儿内核场景），等待会超时并明确报错建议手动 start。
func degradeRestart(home paths.Home, secret string) error {
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
		fmt.Println("core restarted with new config")
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
