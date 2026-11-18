package sysproxy

import (
	"errors"
	"strconv"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// appliedBypass 是写入系统代理时的绕过列表（localhost + 内网段，DESIGN §2.4 默认绕过语义的最简集）。
var appliedBypass = []string{"127.0.0.1", "localhost", "*.local", "169.254/16"}

// ApplyCommands 生成把服务代理写入 host:port + 默认 bypass 的命令序列（纯函数，便于单测）。
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

// RestoreCommands 生成把服务恢复到备份状态的命令序列（纯函数）。
// 语义（M0-plan §3.3）：逐 service 恢复 enabled/server/port/bypass 到备份值——不是简单全部关掉。
func RestoreCommands(b store.ServiceBackup) [][]string {
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
	return append(cmds, bypassCommands(b.Service, b.Bypass))
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

// Apply 对各服务写入 host:port 系统代理。调用方必须已完成 Snapshot 备份并落盘。
func Apply(r Runner, services []string, host string, port int) error {
	for _, svc := range services {
		for _, args := range ApplyCommands(svc, host, port, appliedBypass) {
			if _, err := r.Run(args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restore 按备份逐服务精确还原，只触碰备份中存在的服务。
// 单个服务还原失败不阻断其余服务（最终合并报错）。
func Restore(r Runner, backups []store.ServiceBackup) error {
	var errs []error
	for _, b := range backups {
		for _, args := range RestoreCommands(b) {
			if _, err := r.Run(args...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
