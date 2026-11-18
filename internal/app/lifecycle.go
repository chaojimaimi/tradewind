package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/synth"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// Synthesize 合成 runtime.yaml（模板 embed + 订阅 current + settings + secret 注入）。
func Synthesize(home paths.Home, secret string) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	// 无 current 订阅时合成纯模板（裸内核也可启动，便于先 start 后 add）
	var sub []byte
	if item, ok := idx.CurrentItem(); ok {
		sub, err = os.ReadFile(home.ProfileFile(item.File))
		if err != nil {
			return fmt.Errorf("app: read current profile %s: %w", item.File, err)
		}
	}
	out, err := synth.Synth(sub, synth.Injection{Settings: settings, Secret: secret})
	if err != nil {
		return err
	}
	return store.WriteFileRaw(home.RuntimeFile(), out)
}

// startCore 合成 + spawn + 握手（M0-plan §5.1 步骤 4-6）。
func startCore(home paths.Home, secret string) (*core.Manager, error) {
	if err := Synthesize(home, secret); err != nil {
		return nil, err
	}
	mgr := core.NewManager(core.Config{
		BinPath:     home.Bin(),
		SidecarPath: home.Sidecar(),
		DataDir:     home.String(),
		RuntimePath: home.RuntimeFile(),
		SockPath:    home.SockFile(),
		Secret:      secret,
		LogPath:     home.LogFile(),
	})
	if err := mgr.Spawn(); err != nil {
		return nil, err
	}
	fmt.Printf("core spawned (pid %d), handshaking...\n", mgr.PID())
	if err := mgr.WaitReady(context.Background()); err != nil {
		_ = mgr.Stop() // 握手失败也必须回收进程并清理 sock
		return nil, err
	}
	return mgr, nil
}

// fullShutdown 完整还原流程（M0-plan §5.1 步骤 10）：
// 还原系统代理 → 内核 SIGTERM（5s 后 SIGKILL）→ 移除 core.sock → desired=stopped。
// flock 释放由调用方 defer 完成。任何一步失败都不阻断后续步骤（合并报错）。
func fullShutdown(home paths.Home, mgr *core.Manager) error {
	var errs []error
	if err := restoreSysProxyIfApplied(home); err != nil {
		errs = append(errs, err)
	}
	if mgr != nil {
		if err := mgr.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := markStopped(home); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("app: shutdown: %w", err)
	}
	return nil
}

// restoreSysProxyIfApplied 按 state.json 备份精确还原系统代理（applied=false 即无操作）。
func restoreSysProxyIfApplied(home paths.Home) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		return nil
	}
	fmt.Println("restoring system proxy from backup...")
	if err := sysproxy.Restore(sysproxy.SystemRunner(), st.SysProxy.Services); err != nil {
		return fmt.Errorf("app: restore sysproxy: %w", err)
	}
	fmt.Println("system proxy restored")
	return nil
}

// markStopped 写回 desired=stopped、applied=false。
func markStopped(home paths.Home) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	st.Core = store.CoreState{Desired: store.DesiredStopped, PID: 0}
	st.SysProxy.Applied = false
	return store.SaveState(home, st)
}

// cleanupResidues 启动期双残留清理（M0-plan §5.1 步骤 2，防 kill -9 遗患）：
// a. 系统代理 applied==true → 按备份精确还原；
// b. 孤儿内核 pid 存活且身份匹配 → SIGTERM 回收（5s 后 SIGKILL）。
// 注：残留 core.sock 无需处理——mihomo 在 listen 前 unlink 自清理。
func cleanupResidues(home paths.Home) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if st.SysProxy.Applied {
		fmt.Println("cleanup: restoring system proxy left by previous run")
		if err := restoreSysProxyIfApplied(home); err != nil {
			return err
		}
		st.SysProxy.Applied = false
	}
	if st.Core.PID > 0 && processAlive(st.Core.PID) && pidCommandIsCore(st.Core.PID, home) {
		fmt.Printf("cleanup: killing orphan core (pid %d)\n", st.Core.PID)
		if err := killProcessGracefully(st.Core.PID); err != nil {
			return err
		}
		st.Core.PID = 0
	}
	if st.Core.Desired != store.DesiredStopped {
		st.Core.Desired = store.DesiredStopped
	}
	return store.SaveState(home, st)
}

// applySysProxy 备份 → 落盘 applied=true → 写入 127.0.0.1:port。
// 崩溃安全顺序：备份先行落盘，apply 中途被 kill 也能在下次启动还原。
func applySysProxy(home paths.Home, port int) ([]store.ServiceBackup, error) {
	runner := sysproxy.SystemRunner()
	svcs, err := sysproxy.ActiveServices(runner)
	if err != nil {
		return nil, err
	}
	if len(svcs) == 0 {
		return nil, errors.New("app: no active network services found")
	}
	backups, err := sysproxy.Snapshot(runner, svcs)
	if err != nil {
		return nil, err
	}
	st, err := store.LoadState(home)
	if err != nil {
		return nil, err
	}
	st.SysProxy = store.SysProxyState{Applied: true, Services: backups}
	if err := store.SaveState(home, st); err != nil {
		return nil, err
	}
	if err := sysproxy.Apply(runner, svcs, "127.0.0.1", port); err != nil {
		// 写入失败立即回滚备份，绝不留下半应用状态
		_ = sysproxy.Restore(runner, backups)
		st.SysProxy.Applied = false
		_ = store.SaveState(home, st)
		return nil, fmt.Errorf("app: apply sysproxy: %w (networksetup 需要管理员权限)", err)
	}
	return backups, nil
}
