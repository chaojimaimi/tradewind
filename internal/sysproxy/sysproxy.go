package sysproxy

import (
	"errors"
	"strconv"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// appliedBypass 是绕过列表的缺省集（localhost + 内网段，DESIGN §2.4 最简集）。
// settings.yaml 未配置 bypass_domains 时的生效值（批④ §3.1）。
var appliedBypass = []string{"127.0.0.1", "localhost", "*.local", "169.254/16"}

// EffectiveBypass 返回绕过列表生效值：自定义非空用自定义，否则缺省集。
// 批④ bypass 参数化的统一入口——watcher 恢复/retarget/PAC 写入共用同一判据。
func EffectiveBypass(custom []string) []string {
	if len(custom) > 0 {
		return custom
	}
	return appliedBypass
}

// ApplyCommands 生成把服务代理写入 host:port + bypass 的命令序列（纯函数，便于单测）。
func ApplyCommands(svc, host string, port int, bypass []string) [][]string {
	portStr := strconv.Itoa(port)
	cmds := [][]string{
		{"-setwebproxy", svc, host, portStr},
		{"-setsecurewebproxy", svc, host, portStr},
		{"-setsocksfirewallproxy", svc, host, portStr},
	}
	cmds = append(cmds, bypassCommands(svc, bypass))
	return cmds
}

// ApplyPACCommands 生成把服务切到 PAC 自动代理的命令序列（纯函数）。
// WHY -setautoproxystate on 恒配对（评审 P1-1 联网核查）：-setautoproxyurl 只写
// URL 不保证启用自动代理开关（版本相关行为），缺 state 命令会恒静默不生效；
// bypass 同步写入（网络层例外列表对自动代理同样生效）。
func ApplyPACCommands(svc, pacURL string, bypass []string) [][]string {
	cmds := [][]string{
		{"-setautoproxyurl", svc, pacURL},
		{"-setautoproxystate", svc, "on"},
	}
	cmds = append(cmds, bypassCommands(svc, bypass))
	return cmds
}

// autoproxyRestoreCommands 按 Restore 决策表（批④ §3.3 终审定稿）生成 autoproxy
// 还原段：仅 form=pac 时有命令——
//   - 备份 AutoProxy 非零（URL 非空）→ 按备份精确恢复：URL 恒写回、开关跟随备份
//     Enabled（`URL: x / Enabled: No` 是真实常见 macOS 态，强行 on 会把用户已关的
//     autoproxy 打开指向旧 PAC，违背"按备份精确还原"不变量）；
//   - 备份零值 → setautoproxystate off（取舍声明保留：机器本无自动代理，写 off 幂等）。
//
// form=manual/空（legacy 备份）→ 恒零 autoproxy 命令（旧备份零回归锚）。
func autoproxyRestoreCommands(b store.ServiceBackup, form string) [][]string {
	if form != FormPAC {
		return nil
	}
	if b.AutoProxy.URL != "" {
		state := "off"
		if b.AutoProxy.Enabled {
			state = "on"
		}
		return [][]string{
			{"-setautoproxyurl", b.Service, b.AutoProxy.URL},
			{"-setautoproxystate", b.Service, state},
		}
	}
	return [][]string{{"-setautoproxystate", b.Service, "off"}}
}

// RestoreCommands 生成把服务恢复到备份状态的命令序列（纯函数）。
// 语义（M0-plan §3.3）：逐 service 恢复 enabled/server/port/bypass 到备份值——不是简单全部关掉。
// form 是会话写入形态（SysProxyState.Form；""=legacy 视为 manual），仅 pac 形态
// 追加 autoproxy 还原段（决策表见 autoproxyRestoreCommands）。
func RestoreCommands(b store.ServiceBackup, form string) [][]string {
	var cmds [][]string
	// 每项代理：先恢复 server/port（networksetup 的 set 会顺带启用），备份为禁用再关闭
	for _, item := range []struct {
		set, state string
		ps         store.ProxyState
	}{
		{"-setwebproxy", "-setwebproxystate", b.Web},
		{"-setsecurewebproxy", "-setsecurewebproxystate", b.Secure},
		{"-setsocksfirewallproxy", "-setsocksfirewallproxystate", b.Socks},
	} {
		if item.ps.Server != "" && item.ps.Port > 0 {
			cmds = append(cmds, []string{item.set, b.Service, item.ps.Server, strconv.Itoa(item.ps.Port)})
		}
		if !item.ps.Enabled {
			cmds = append(cmds, []string{item.state, b.Service, "off"})
		}
	}
	cmds = append(cmds, bypassCommands(b.Service, b.Bypass))
	return append(cmds, autoproxyRestoreCommands(b, form)...)
}

// bypassCommands 生成 bypass 列表写入命令。
// 边界（M0-plan §6）：空列表必须用 "Empty" 关键字清空——networksetup 传空串会报错。
func bypassCommands(svc string, bypass []string) []string {
	args := []string{"-setproxybypassdomains", svc}
	if len(bypass) == 0 {
		return append(args, "Empty")
	}
	return append(args, bypass...)
}

// Apply 对各服务写入 host:port 手工三项代理 + bypass（生效值由调用方传入——
// 批④参数化：watcher 恢复/retarget 用 settings 生效值，非写死常量）。
// 调用方必须已完成 Snapshot 备份并落盘。
func Apply(r Runner, services []string, host string, port int, bypass []string) error {
	for _, svc := range services {
		for _, args := range ApplyCommands(svc, host, port, bypass) {
			if _, err := r.Run(args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// ApplyPAC 对各服务写入 PAC 自动代理 + bypass。调用方必须已完成 Snapshot 备份并落盘。
func ApplyPAC(r Runner, services []string, pacURL string, bypass []string) error {
	for _, svc := range services {
		for _, args := range ApplyPACCommands(svc, pacURL, bypass) {
			if _, err := r.Run(args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// 会话写入形态常量（SysProxyState.Form 的合法值；空串=legacy 视为 manual）。
const (
	FormManual = "manual"
	FormPAC    = "pac"
)

// Restore 按备份逐服务精确还原，只触碰备份中存在的服务；form 决定 autoproxy
// 还原段（决策表见 RestoreCommands）。单个服务还原失败不阻断其余服务（最终合并报错）。
func Restore(r Runner, backups []store.ServiceBackup, form string) error {
	var errs []error
	for _, b := range backups {
		for _, args := range RestoreCommands(b, form) {
			if _, err := r.Run(args...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
