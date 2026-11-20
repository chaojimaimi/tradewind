package app

import (
	"errors"
	"fmt"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// errPACGatewayUnreachable 是 PAC 形态写入的前置守卫失败哨兵（applySysProxyRunner
// 同置守卫，批④ §3.1/§3.3）：CLI on 路径翻译成用户可读报错；start 路径据此跳过
// 系统代理写入并显著警告（不 fail start——F4 网关降级不拖垮 start 的哲学相容）。
var errPACGatewayUnreachable = errors.New("app: panel gateway unreachable for PAC")

// SysProxyOn 编排 `tradewind sysproxy on`（幂等语义 M2-plan §4.1）：
// mutation.lock 有界等待（批④评审二轮 P2-1：与 watcher/fullShutdown 互斥，消除
// SaveState(applied=true) 先于 Apply 的窗口假报；最坏等锁 35s 由壳侧 65s 超时档覆盖）；
// 前置守卫内核必须可达（否则系统代理指向死端口 = 断网陷阱）；
// applied=true → no-op 成功返回（零 networksetup 调用）；
// 否则 备份 → 落盘 applied=true → 按 settings.PacMode 决定的形态写入。
// WHY 幂等：重复 on 会把自身写入的 127.0.0.1 当"原值"快照，此后还原 =
// 系统代理指向已死端口——这是 M1 遗留语义的最大陷阱。
func SysProxyOn(home paths.Home) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	if !CoreReachable(home) {
		return errors.New("app: core is not reachable; run `tradewind start` first (refusing to point system proxy at a dead port)")
	}
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	return sysProxyOnWith(home, settings.MixedPort, sysproxy.SystemRunner(), formFromSettings(settings))
}

// SysProxyOff 编排 `tradewind sysproxy off`：无内核守卫（随时可关）；
// mutation.lock 有界等待（同 SysProxyOn——消除 Restore→SaveState 亚秒窗口与
// watcher tick 的竞速）；applied=false → no-op；否则按备份精确还原 → applied=false。
func SysProxyOff(home paths.Home) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	return sysProxyOffWith(home, sysproxy.SystemRunner())
}

// formFromSettings 由 settings 推导会话写入形态（PacMode 缺省 false → manual）。
func formFromSettings(s store.Settings) string {
	if s.PacMode {
		return sysproxy.FormPAC
	}
	return sysproxy.FormManual
}

// sysProxyOnWith 是 on 的可测核心（fake runner 单测，绝不触碰真实系统代理）。
// form 经由调用方传入（CLI 按 settings.PacMode；单测矩阵显式指定双形态）。
func sysProxyOnWith(home paths.Home, port int, runner sysproxy.Runner, form string) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if st.SysProxy.Applied {
		fmt.Println("system proxy already applied; nothing to do")
		return nil
	}
	if _, err := applySysProxyRunner(home, port, runner, form); err != nil {
		if errors.Is(err, errPACGatewayUnreachable) {
			return errors.New("面板网关不可达，PAC 无法生效")
		}
		return err
	}
	fmt.Printf("system proxy applied (form=%s, original values backed up)\n", form)
	return nil
}

// sysProxyOffWith 是 off 的可测核心：按备份精确还原（autoproxy 段按会话 Form 分流，
// Restore 决策表见 sysproxy.RestoreCommands）→ applied=false。
func sysProxyOffWith(home paths.Home, runner sysproxy.Runner) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		fmt.Println("system proxy already off; nothing to do")
		return nil
	}
	if err := sysproxy.Restore(runner, st.SysProxy.Services, st.SysProxy.Form); err != nil {
		return fmt.Errorf("app: restore sysproxy: %w", err)
	}
	st.SysProxy.Applied = false
	if err := store.SaveState(home, st); err != nil {
		return err
	}
	fmt.Println("system proxy restored from backup")
	return nil
}

// applySysProxyRunner 备份 → 落盘 applied=true（含会话 Form）→ 按 form 写入。
// CLI on（sysProxyOnWith）与 start（postHandshake→applySysProxy）的共用编排核心
// ——Form 写入与 PAC 分流钉在这里（批④评审二轮 P2-2：仅改 sysProxyOnWith 会
// "CLI on 走 PAC、start 重启走手工"形态分裂）。
// 崩溃安全顺序：备份先行落盘，apply 中途被 kill 也能在下次启动还原。
// （原 lifecycle.applySysProxy 的可注入形态；写入失败立即回滚备份。）
// PAC 形态前置 gatewayReachable 守卫：不可达返回 errPACGatewayUnreachable 哨兵，
// 不写系统代理（手工形态无此守卫——127.0.0.1:mixed_port 由内核可达性保证）。
func applySysProxyRunner(home paths.Home, port int, runner sysproxy.Runner, form string) ([]store.ServiceBackup, error) {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return nil, err
	}
	bypass := sysproxy.EffectiveBypass(settings.BypassDomains)
	if form == sysproxy.FormPAC && !gatewayReachable(settings.GatewayPort) {
		return nil, errPACGatewayUnreachable
	}
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
	st.SysProxy = store.SysProxyState{Applied: true, Services: backups, Form: form}
	if err := store.SaveState(home, st); err != nil {
		return nil, err
	}
	var applyErr error
	if form == sysproxy.FormPAC {
		pacURL := fmt.Sprintf("http://127.0.0.1:%d/pac", settings.GatewayPort)
		applyErr = sysproxy.ApplyPAC(runner, svcs, pacURL, bypass)
	} else {
		applyErr = sysproxy.Apply(runner, svcs, "127.0.0.1", port, bypass)
	}
	if applyErr != nil {
		// 写入失败立即回滚备份，绝不留下半应用状态（回滚按本会话 form 分流 autoproxy 段）
		_ = sysproxy.Restore(runner, backups, form)
		st.SysProxy.Applied = false
		_ = store.SaveState(home, st)
		return nil, fmt.Errorf("app: apply sysproxy: %w (networksetup 需要管理员权限)", applyErr)
	}
	return backups, nil
}
