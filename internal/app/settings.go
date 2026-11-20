package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// settingsKeys 是 `settings set` 的白名单键（壳通用页可写集，M3-plan §11.2-5；
// 批④ §3.1 增 pac_mode/bypass_domains）。
var settingsKeys = []string{"mixed_port", "gateway_port", "panel_enabled", "allow_lan", "log_retention_days", "pac_mode", "bypass_domains"}

// SettingsGet 编排 `tradewind settings [get]`：--json 输出机器可读单行 JSON
// （壳通用页初始值数据源），否则打印人读表。
func SettingsGet(home paths.Home, asJSON bool) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	if asJSON {
		line, err := MarshalSettingsJSON(settings)
		if err != nil {
			return err
		}
		fmt.Println(line)
		return nil
	}
	printSettingsTable(settings)
	return nil
}

// MarshalSettingsJSON 序列化 store.Settings（json tag 与 yaml 键一致，壳按 snake_case 解码）。
func MarshalSettingsJSON(s store.Settings) (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("app: marshal settings json: %w", err)
	}
	return string(data), nil
}

// printSettingsTable 输出人读设置表（键序与 JSON 一致）。
func printSettingsTable(s store.Settings) {
	fmt.Printf("mixed_port:         %d\n", s.MixedPort)
	fmt.Printf("gateway_port:       %d\n", s.GatewayPort)
	fmt.Printf("panel_enabled:      %t\n", s.PanelEnabled)
	fmt.Printf("allow_lan:          %t\n", s.AllowLan)
	fmt.Printf("mode:               %s\n", s.Mode)
	fmt.Printf("log_level:          %s\n", s.LogLevel)
	fmt.Printf("log_retention_days: %d\n", s.LogRetentionDays)
	fmt.Printf("pac_mode:           %t\n", s.PacMode)
	fmt.Printf("bypass_domains:     %s\n", bypassDomainsDisplay(s.BypassDomains))
}

// bypassDomainsDisplay 人读 bypass 列表（空=未设置，生效值取 sysproxy 缺省集）。
func bypassDomainsDisplay(domains []string) string {
	if len(domains) == 0 {
		return "(default)"
	}
	return strings.Join(domains, ",")
}

// SettingsSet 编排 `tradewind settings set <key> <value>`：
// 白名单键 + 完整写编排（M3-plan §11.2-4，mode.go 序列范本）。
func SettingsSet(ctx context.Context, home paths.Home, key, value string) error {
	// gateway_port/panel_enabled 与内核无关且由 start 进程内监听（panel.go）：跳过
	// 内核 reload（评审 P2——全量 force 重载会白断连接，最坏路径引入 15s 降级重启），
	// 仅落盘+Synthesize+重启提示
	skipKernelReload := key == "gateway_port" || key == "panel_enabled"
	err := applySettingsMutationWith(ctx, home, func(s *store.Settings) error {
		return settingsSetKey(s, key, value)
	}, sysproxy.SystemRunner(), skipKernelReload)
	if err != nil {
		return err
	}
	if skipKernelReload {
		fmt.Println("note: restart Tradewind to apply this change")
	}
	return nil
}

// SettingsReset 编排 `tradewind settings reset`：恢复默认值，走同一写编排
// （mixed_port 被定制且 sysproxy applied 时同样触发 retarget 联动）。
func SettingsReset(ctx context.Context, home paths.Home) error {
	return applySettingsMutationWith(ctx, home, func(s *store.Settings) error {
		*s = store.DefaultSettings()
		return nil
	}, sysproxy.SystemRunner(), false)
}

// applySettingsMutation 是 settings 写编排可测核心（runner 可注入，绝不触碰真实系统代理）。
func applySettingsMutation(ctx context.Context, home paths.Home, mutate func(*store.Settings) error) error {
	return applySettingsMutationWith(ctx, home, mutate, sysproxy.SystemRunner(), false)
}

// skipKernelReload（评审 P2）＝ true 时仅落盘+Synthesize（gateway_port/panel_enabled
// 等非内核消费键），跳过 reload/retarget。
func applySettingsMutationWith(ctx context.Context, home paths.Home, mutate func(*store.Settings) error, runner sysproxy.Runner, skipKernelReload bool) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	mixedPortBefore := settings.MixedPort
	gatewayPortBefore := settings.GatewayPort
	pacModeBefore := settings.PacMode
	if err := mutate(&settings); err != nil {
		return err
	}
	// SaveSettings 内含 Validate（含 mixed_port==gateway_port 互斥），非法值不落盘
	if err := store.SaveSettings(home, settings); err != nil {
		return err
	}
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if err := Synthesize(home, secret); err != nil {
		return err
	}
	if skipKernelReload {
		// gateway_port 变更 × PAC 会话（批④ §3.2 评审二轮 P1-3）：网关仍监听旧端口
		// 直到 start 重启——不重放（重放=指向无人监听端口，制造死链）；仅警告，
		// 重启后 start 路径以新端口 Apply 自然收敛
		if msg, needed := gatewayPortChangeWarning(home, settings, gatewayPortBefore); needed {
			fmt.Println("warning: " + msg)
		}
		fmt.Println("settings saved (restart Tradewind to apply this change)")
		return nil
	}
	if !CoreReachable(home) {
		fmt.Println("settings saved (core not running; takes effect on next start)")
		// 内核未运行 → 不 retarget（系统代理不指向死端口）；但 sysproxy 已 applied 时
		// pac_mode 翻转仍立即生效（系统代理在场，与内核无关）→ 形态重放照走
		return switchSysProxyFormIfNeeded(home, runner, settings, pacModeBefore)
	}
	// 先 reload 后 retarget：PUT 已重绑端口而 sysproxy 仍指旧端口 = 断网陷阱窗口
	if err := ReloadIfRunning(ctx, home); err != nil {
		return err // 热重载失败且降级重启也失败：不 retarget，显式报错
	}
	if settings.MixedPort != mixedPortBefore {
		if err := retargetSysProxy(home, settings.MixedPort, runner); err != nil {
			return err
		}
	}
	return switchSysProxyFormIfNeeded(home, runner, settings, pacModeBefore)
}

// retargetSysProxy mixed_port 变更后的系统代理迁移。
// WHY 不复用 SysProxyOn：applied=true 时它是幂等 no-op，直接再调不生效；
// 这里直接按会话 Form（SysProxyState.Form）重放对应形态写入并保留既有备份态
// （备份仍指向用户原始网络配置，还原语义不变）；bypass 用 settings 生效值。
// 仅 mixed_port 变更触发——gateway_port 是 skipKernelReload 键不重放（批④ §3.2）。
// 失败显式报错不静默（系统代理仍指旧端口属用户可感知故障），不做自动回滚
// （回滚到旧端口同样指向已死端口，无意义）。
func retargetSysProxy(home paths.Home, port int, runner sysproxy.Runner) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		return nil // 未应用系统代理：无需迁移
	}
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	svcs, err := sysproxy.ActiveServices(runner)
	if err != nil {
		return fmt.Errorf("app: retarget sysproxy: %w; 系统代理迁移失败，请手动开关一次系统代理", err)
	}
	if len(svcs) == 0 {
		return errors.New("app: retarget sysproxy: no active network services; 系统代理迁移失败，请手动开关一次系统代理")
	}
	form := st.SysProxy.Form
	if form == sysproxy.FormPAC {
		pacURL := fmt.Sprintf("http://127.0.0.1:%d/pac", settings.GatewayPort)
		err = sysproxy.ApplyPAC(runner, svcs, pacURL, sysproxy.EffectiveBypass(settings.BypassDomains))
	} else {
		err = sysproxy.Apply(runner, svcs, "127.0.0.1", port, sysproxy.EffectiveBypass(settings.BypassDomains))
	}
	if err != nil {
		return fmt.Errorf("app: retarget sysproxy: %w; 系统代理迁移失败，请手动开关一次系统代理", err)
	}
	fmt.Printf("system proxy retargeted to 127.0.0.1:%d (backup preserved)\n", port)
	return nil
}

// gatewayPortChangeWarning gateway_port 变更 × PAC 会话（applied 且 Form=pac）的
// 仅警告判据：不重放期间 PAC 指向仍活着的旧端口，重启后 start 路径以新端口 Apply
// 自然收敛。未 applied（无系统代理在场）时警告是噪音，不提示。
func gatewayPortChangeWarning(home paths.Home, settings store.Settings, before int) (string, bool) {
	if settings.GatewayPort == before {
		return "", false
	}
	st, err := store.LoadState(home)
	if err != nil || !st.SysProxy.Applied || st.SysProxy.Form != sysproxy.FormPAC {
		return "", false
	}
	return "面板端口已改，重启后 PAC 自动指向新端口", true
}

// switchSysProxyFormIfNeeded pac_mode 翻转时的形态重放判定（批④ §3.3）：
// 仅 sysproxy applied 时重放（retarget 同款）；未 applied = 预配置（内核未运行允许，
// 下次 start 生效），写侧不探测网关。
// 守卫探测条件（批④ §3.1）：仅当变更将立即生效（applied）时探测 gatewayReachable；
// 新形态 PAC 且网关不可达 → 跳过重放 + 显著警告（设置已落盘，网关可用后
// 重启 Tradewind 或手动开关一次系统代理收敛）。
func switchSysProxyFormIfNeeded(home paths.Home, runner sysproxy.Runner, settings store.Settings, pacModeBefore bool) error {
	if settings.PacMode == pacModeBefore {
		return nil
	}
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		return nil
	}
	newForm := formFromSettings(settings)
	if newForm == sysproxy.FormPAC && !gatewayReachable(settings.GatewayPort) {
		fmt.Println("warning: PAC 未生效（面板网关不可达）——设置已保存；网关可用后重启 Tradewind 或手动开关一次系统代理")
		return nil
	}
	return switchSysProxyForm(home, runner, st, settings, newForm)
}

// switchSysProxyForm 形态重放本体（崩溃安全序，批④风险表"PAC 与手工态混写"对策）：
// 先按备份 Restore 全部（回到用户原始网络配置，含旧形态的 autoproxy 还原段）→
// 按新 Form Apply。备份先行，中途 kill 的残留由下次启动 cleanupResidues 收敛。
func switchSysProxyForm(home paths.Home, runner sysproxy.Runner, st store.State, settings store.Settings, newForm string) error {
	svcs, err := sysproxy.ActiveServices(runner)
	if err != nil {
		return fmt.Errorf("app: switch sysproxy form: %w; 系统代理迁移失败，请手动开关一次系统代理", err)
	}
	if len(svcs) == 0 {
		return errors.New("app: switch sysproxy form: no active network services; 系统代理迁移失败，请手动开关一次系统代理")
	}
	if err := sysproxy.Restore(runner, st.SysProxy.Services, st.SysProxy.Form); err != nil {
		return fmt.Errorf("app: switch sysproxy form: %w; 系统代理迁移失败，请手动开关一次系统代理", err)
	}
	bypass := sysproxy.EffectiveBypass(settings.BypassDomains)
	if newForm == sysproxy.FormPAC {
		pacURL := fmt.Sprintf("http://127.0.0.1:%d/pac", settings.GatewayPort)
		err = sysproxy.ApplyPAC(runner, svcs, pacURL, bypass)
	} else {
		err = sysproxy.Apply(runner, svcs, "127.0.0.1", settings.MixedPort, bypass)
	}
	if err != nil {
		return fmt.Errorf("app: switch sysproxy form: %w; 系统代理迁移失败，请手动开关一次系统代理", err)
	}
	// 会话形态换轨持久化（Restore 决策表与 watcher 检测以 state Form 为准）
	st.SysProxy.Form = newForm
	if err := store.SaveState(home, st); err != nil {
		return err
	}
	fmt.Printf("system proxy switched to %s form\n", newForm)
	return nil
}

// settingsSetKey 把白名单键值写入 settings（解析失败的显式报错面向 CLI 用户）。
func settingsSetKey(s *store.Settings, key, value string) error {
	switch key {
	case "mixed_port", "gateway_port", "log_retention_days":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("app: settings set: %s wants an integer, got %q", key, value)
		}
		switch key {
		case "mixed_port":
			s.MixedPort = n
		case "gateway_port":
			s.GatewayPort = n
		case "log_retention_days":
			s.LogRetentionDays = n
		}
	case "panel_enabled", "allow_lan", "pac_mode":
		b, err := parseSettingBool(value)
		if err != nil {
			return fmt.Errorf("app: settings set: %s wants on|off, got %q", key, value)
		}
		switch key {
		case "panel_enabled":
			s.PanelEnabled = b
		case "allow_lan":
			s.AllowLan = b
		case "pac_mode":
			s.PacMode = b
		}
	case "bypass_domains":
		// 逗号分隔（批④ §3.1）：空=恢复缺省（nil → 生效值取 sysproxy 缺省集）；
		// 逐条 trim、丢弃空段。白名单/条数校验在 Settings.Validate（SaveSettings 落盘
		// 前必经），CLI 层保留本函数的解析报错文案。
		s.BypassDomains = parseBypassDomains(value)
	default:
		return fmt.Errorf("app: settings set: unknown key %q (valid: %s)", key, strings.Join(settingsKeys, "|"))
	}
	return nil
}

// parseBypassDomains 解析逗号分隔的 bypass 列表（空串=恢复缺省 nil）。
func parseBypassDomains(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(trimmed, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseSettingBool 解析开关值（on|off 主形态，兼容 true/false/1/0）。
func parseSettingBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("app: invalid bool %q", value)
	}
}
