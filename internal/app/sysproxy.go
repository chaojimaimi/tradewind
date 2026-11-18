package app

import (
	"errors"
	"fmt"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// SysProxyOn 编排 `tradewind sysproxy on`（幂等语义 M2-plan §4.1）：
// 前置守卫内核必须可达（否则系统代理指向死端口 = 断网陷阱）；
// applied=true → no-op 成功返回（零 networksetup 调用）；
// 否则 备份 → 落盘 applied=true → 写入（applySysProxy 内保证崩溃安全顺序）。
// WHY 幂等：重复 on 会把自身写入的 127.0.0.1 当"原值"快照，此后还原 =
// 系统代理指向已死端口——这是 M1 遗留语义的最大陷阱。
func SysProxyOn(home paths.Home) error {
	if !CoreReachable(home) {
		return errors.New("app: core is not reachable; run `tradewind start` first (refusing to point system proxy at a dead port)")
	}
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	return sysProxyOnWith(home, settings.MixedPort, sysproxy.SystemRunner())
}

// SysProxyOff 编排 `tradewind sysproxy off`：无内核守卫（随时可关）；
// applied=false → no-op；否则按备份精确还原 → applied=false。
func SysProxyOff(home paths.Home) error {
	return sysProxyOffWith(home, sysproxy.SystemRunner())
}

// sysProxyOnWith 是 on 的可测核心（fake runner 单测，绝不触碰真实系统代理）。
func sysProxyOnWith(home paths.Home, port int, runner sysproxy.Runner) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if st.SysProxy.Applied {
		fmt.Println("system proxy already applied; nothing to do")
		return nil
	}
	if _, err := applySysProxyRunner(home, port, runner); err != nil {
		return err
	}
	fmt.Printf("system proxy applied (127.0.0.1:%d, original values backed up)\n", port)
	return nil
}

// sysProxyOffWith 是 off 的可测核心。
func sysProxyOffWith(home paths.Home, runner sysproxy.Runner) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		fmt.Println("system proxy already off; nothing to do")
		return nil
	}
	if err := sysproxy.Restore(runner, st.SysProxy.Services); err != nil {
		return fmt.Errorf("app: restore sysproxy: %w", err)
	}
	st.SysProxy.Applied = false
	if err := store.SaveState(home, st); err != nil {
		return err
	}
	fmt.Println("system proxy restored from backup")
	return nil
}

// applySysProxyRunner 备份 → 落盘 applied=true → 写入 127.0.0.1:port。
// 崩溃安全顺序：备份先行落盘，apply 中途被 kill 也能在下次启动还原。
// （原 lifecycle.applySysProxy 的可注入形态；写入失败立即回滚备份。）
func applySysProxyRunner(home paths.Home, port int, runner sysproxy.Runner) ([]store.ServiceBackup, error) {
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
