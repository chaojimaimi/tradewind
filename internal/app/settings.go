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
		fmt.Println("settings saved (restart Tradewind to apply this change)")
		return nil
	}
	if !CoreReachable(home) {
		fmt.Println("settings saved (core not running; takes effect on next start)")
		return nil // 内核未运行 → 不 retarget（系统代理不指向死端口）
	}
	// 先 reload 后 retarget：PUT 已重绑端口而 sysproxy 仍指旧端口 = 断网陷阱窗口
	if err := ReloadIfRunning(ctx, home); err != nil {
		return err // 热重载失败且降级重启也失败：不 retarget，显式报错
	}
	if settings.MixedPort != mixedPortBefore {
		return retargetSysProxy(home, settings.MixedPort, runner)
	}
	return nil
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
