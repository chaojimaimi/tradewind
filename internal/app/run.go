package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// StartOptions 是 start 子命令的行为开关。
type StartOptions struct {
	// ApplySysProxy 为 false 时不触碰系统代理（冒烟/开发场景的隔离手段）
	ApplySysProxy bool
}

// Run 执行 start 全流程（M0-plan §5.1），阻塞至信号或内核彻底失活。
// 返回 nil 表示信号触发的优雅退出；返回错误表示失败（内核 3 次重启耗尽等），
// 两种路径都已完成完整还原（系统代理/内核/状态文件）。
func Run(ctx context.Context, home paths.Home, opts StartOptions) error {
	if err := home.Ensure(); err != nil {
		return err
	}
	lock, err := LockPIDFile(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := cleanupResidues(home); err != nil {
		return err
	}
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if err := core.VerifyBinary(home.Bin(), home.Sidecar()); err != nil {
		return err
	}
	mgr, err := startCore(home, secret)
	if err != nil {
		return err
	}
	if err := postHandshake(ctx, home, mgr, opts); err != nil {
		return err
	}
	return waitLoop(ctx, home, mgr)
}

// postHandshake 完成握手后的步骤 7/8：系统代理写入 + desired=running + ready 输出。
func postHandshake(ctx context.Context, home paths.Home, mgr *core.Manager, opts StartOptions) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		_ = fullShutdown(home, mgr)
		return err
	}
	if opts.ApplySysProxy {
		if _, err := applySysProxy(home, settings.MixedPort); err != nil {
			_ = fullShutdown(home, mgr)
			return err
		}
	}
	st, err := store.LoadState(home)
	if err != nil {
		_ = fullShutdown(home, mgr)
		return err
	}
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: mgr.PID()}
	if !opts.ApplySysProxy {
		st.SysProxy.Applied = false
	}
	if err := store.SaveState(home, st); err != nil {
		_ = fullShutdown(home, mgr)
		return err
	}
	version, _ := core.NewClient(home.SockFile(), store.ReadSecret(home)).Version(ctx)
	fmt.Printf("ready: core=%s pid=%d mixed=127.0.0.1:%d sysproxy=%v\n",
		version, mgr.PID(), settings.MixedPort, opts.ApplySysProxy)
	return nil
}

// waitLoop 阻塞等待：SIGINT/SIGTERM → 优雅退出；内核 3 次重启耗尽 → 返回错误。
// giveup 回调内已完成完整还原（绝不把系统代理留在指向死端口的状态）。
func waitLoop(ctx context.Context, home paths.Home, mgr *core.Manager) error {
	monCh := make(chan error, 1)
	go func() {
		monCh <- mgr.Monitor(ctx, func(error) { _ = fullShutdown(home, mgr) })
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		fmt.Printf("\ncaught %s, shutting down...\n", sig)
		return fullShutdown(home, mgr)
	case err := <-monCh:
		return err
	}
}
