package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
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
// 返回 nil 表示信号触发的优雅退出或幂等命中"已在运行"（v0.4.12）；返回错误表示失败
// （内核 3 次重启耗尽等），两种路径都已完成完整还原（系统代理/内核/状态文件）。
func Run(ctx context.Context, home paths.Home, opts StartOptions) error {
	if err := home.Ensure(); err != nil {
		return err
	}
	lock, err := LockPIDFile(home)
	if err != nil {
		// v0.4.12 幂等语义：活实例持 flock 锁 = "已在运行"这一目标态已达成，视为成功。
		// WHY launchd agent 是 KeepAlive={SuccessfulExit:false}（crash-restart）语义：壳侧
		// 主开关 detached spawn 抢先持锁后，agent 的重试撞锁 exit(1) 会触发无限 crash-
		// restart 节流循环（autostart.go 注释预言的坑，2026-11-19 真机日志实证：21ms
		// 生命周期每 10s 重试）。幂等 exit 0 让 launchd 视为成功、停止重试；壳侧
		// startCoreDetached 的有界轮询照常拿到 running=true，语义无损。
		// flock 是内核托管锁（进程死锁自动释放），冲突必然意味着活实例，无陈锁误判。
		// 审查 P2（2026-11-19）：仅对冲突类别幂等——pidfile 打开失败等必须照常报错，
		// 否则假报"已运行"会让 launchd 永久停止拉起（静默下线）。
		if errors.Is(err, ErrInstanceRunning) {
			fmt.Println("already running: another tradewind instance holds the lock; nothing to do")
			return nil
		}
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
	settings, err := store.LoadSettings(home)
	if err != nil {
		_ = fullShutdown(home, mgr, 0)
		return err
	}
	// 网关降级（disabled/ui 缺失/端口被占）→ (nil,"")：start 继续运行（M1-plan §5.2）
	gw, panelURL, err := startGateway(home, settings)
	if err != nil {
		_ = fullShutdown(home, mgr, 0)
		return err
	}
	generation, err := postHandshake(ctx, home, mgr, opts, settings, panelURL)
	if err != nil {
		if gw != nil {
			gw.close()
		}
		return err
	}
	// 日志清理（启动时一次，M2-plan §2.4）+ 后台调度器（30min tick，M2-plan §2.2）
	// + 代理守护（30s tick，批④ §2.1）；三者失败均不阻断 start（尽力而为面）。
	// watcher 收敛（评审二轮 P1-2）：waitLoop 两条关停路径的 fullShutdown 都发生在
	// waitLoop 返回之前——收敛函数（cancel+WaitGroup.Wait）前移进两个调用点，
	// 先收敛 watcher 再还原，杜绝守护与关停交错留 applied=false 死端口残留。
	if err := cleanupLogs(home); err != nil {
		fmt.Printf("log cleanup: %v\n", err)
	}
	startScheduler(ctx, home)
	watcherCtx, cancelWatcher := context.WithCancel(ctx)
	var watcherWG sync.WaitGroup
	startWatcher(watcherCtx, home, &watcherWG)
	convergeWatcher := func() {
		cancelWatcher()
		watcherWG.Wait()
	}
	err = waitLoop(ctx, home, mgr, generation, convergeWatcher)
	if gw != nil {
		gw.close()
	}
	return err
}

// postHandshake 完成握手后的步骤 7/8：系统代理写入 + desired=running + ready 输出
// （M1：ready 行追加 panel 入口 URL；M2：generation 自增并返回本实例代际，M2-plan §2.3）。
// PAC 形态 + 网关降级（批④ §3.3 评审二轮 P2-3）：跳过系统代理写入并显著警告
// "PAC 未生效"，不 fail start——fail-start 会把网关降级升级为整机失败，违反 F4
// 降级哲学；其余写入失败照旧 fullShutdown（写入半途失败不留半应用态）。
func postHandshake(ctx context.Context, home paths.Home, mgr *core.Manager, opts StartOptions, settings store.Settings, panelURL string) (int, error) {
	if opts.ApplySysProxy {
		if _, err := applySysProxy(home, settings.MixedPort, formFromSettings(settings)); err != nil {
			if errors.Is(err, errPACGatewayUnreachable) {
				fmt.Println("warning: PAC 未生效（网关降级）——系统代理未写入；检查 panel_enabled/ui 后重启 Tradewind")
			} else {
				_ = fullShutdown(home, mgr, 0)
				return 0, err
			}
		} else {
			noteSysProxyApply() // grace 起点：watcher 30s 内不判抢改（防自家 Apply 误报）
		}
	}
	st, err := store.LoadState(home)
	if err != nil {
		_ = fullShutdown(home, mgr, 0)
		return 0, err
	}
	generation := st.Core.Generation + 1
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: mgr.PID(), Generation: generation}
	if !opts.ApplySysProxy {
		st.SysProxy.Applied = false
	}
	if err := store.SaveState(home, st); err != nil {
		_ = fullShutdown(home, mgr, 0)
		return 0, err
	}
	version, _ := core.NewClient(home.SockFile(), store.ReadSecret(home)).Version(ctx)
	ready := fmt.Sprintf("ready: core=%s pid=%d mixed=127.0.0.1:%d sysproxy=%v",
		version, mgr.PID(), settings.MixedPort, opts.ApplySysProxy)
	if panelURL != "" {
		ready += " panel=" + panelURL
	}
	fmt.Println(ready)
	return generation, nil
}

// waitLoop 阻塞等待：SIGINT/SIGTERM → 优雅退出；内核 3 次重启耗尽 → 返回错误。
// giveup 回调内已完成完整还原（绝不把系统代理留在指向死端口的状态）。
// convergeWatcher（批④评审二轮 P1-2 挂点修正）：两条关停路径的 fullShutdown 都在
// waitLoop 返回之前执行——signal 分支（本 select 内）与 giveup 回调（Monitor 回调，
// 在监控 goroutine 上、waitLoop 仍阻塞、watcher 存活）都先收敛 watcher 再还原，
// 共用 shutdownSequence 同一收敛前置序。
func waitLoop(ctx context.Context, home paths.Home, mgr *core.Manager, generation int, convergeWatcher func()) error {
	monCh := make(chan error, 1)
	go func() {
		monCh <- mgr.Monitor(ctx, func(error) {
			_ = shutdownSequence(home, mgr, generation, convergeWatcher)
		})
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		fmt.Printf("\ncaught %s, shutting down...\n", sig)
		return shutdownSequence(home, mgr, generation, convergeWatcher)
	case err := <-monCh:
		return err
	}
}

// shutdownSequence 关停统一收敛前置序（批④评审二轮 P1-2）：先收敛 watcher
// （cancel+WaitGroup.Wait——在途 tick 完成后退出，此后零 tick）再 fullShutdown。
// 绝不把系统代理留在指向死端口的状态（M1 "最大陷阱"的守护侧收口）。
func shutdownSequence(home paths.Home, mgr *core.Manager, generation int, convergeWatcher func()) error {
	convergeWatcher()
	return fullShutdown(home, mgr, generation)
}
