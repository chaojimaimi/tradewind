// Package sysproxy 封装 macOS networksetup 的系统代理备份/写入/精确还原。
// 所有命令执行经 Runner 抽象注入，单测绝不触碰真实系统代理。
package sysproxy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// ParseEnabledServices 解析 -listallnetworkservices 输出：
// 首行是说明文字，星号前缀表示禁用服务——仅返回启用服务。
func ParseEnabledServices(out string) []string {
	var services []string
	for i, line := range nonEmptyLines(out) {
		if i == 0 && strings.HasPrefix(line, "An asterisk") {
			continue // 首行固定为说明文字
		}
		if strings.HasPrefix(line, "*") {
			continue // 禁用服务跳过
		}
		services = append(services, line)
	}
	return services
}

// ParseProxyState 解析 -getwebproxy / -getsecurewebproxy / -getsocksfirewallproxy 输出：
//
//	Enabled: Yes
//	Server: 127.0.0.1
//	Port: 7897
func ParseProxyState(out string) (store.ProxyState, error) {
	var st store.ProxyState
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "enabled":
			st.Enabled = strings.EqualFold(v, "yes") || v == "1"
		case "server":
			st.Server = v
		case "port":
			n, err := strconv.Atoi(v)
			if err != nil {
				return store.ProxyState{}, fmt.Errorf("sysproxy: parse port %q: %w", v, err)
			}
			st.Port = n
		}
	}
	return st, nil
}

// ParseAutoProxyState 解析 -getautoproxyurl 输出（批④ §3.3 评审 P2-6）：
//
//	URL: http://127.0.0.1:39097/pac
//	Enabled: Yes
//
// 未配置态输出 URL: (null)——哨兵归一为零值 URL。WHY 不复用 ParseProxyState：
// 三键解析（enabled/server/port）与两键（url/enabled）键名不同，"server 静默
// 丢 URL"不可接受。
func ParseAutoProxyState(out string) (url string, enabled bool, err error) {
	seen := false
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "url":
			seen = true
			if v == "(null)" {
				v = ""
			}
			url = v
		case "enabled":
			seen = true
			enabled = strings.EqualFold(v, "yes") || v == "1"
		}
	}
	if !seen {
		// 输出不含任何已知键：解析失败显式报错（"未配置"态恒有 URL/Enabled 两行，
		// URL 为 (null) 哨兵——全零键值仍是可识别输出）
		return "", false, fmt.Errorf("sysproxy: unrecognized -getautoproxyurl output: %q", strings.TrimSpace(out))
	}
	return url, enabled, nil
}

// ParseConnected 判断 -getinfo 输出是否表明服务已连接（有非空 IP）。
// WHY connected 过滤：对未连接的服务写代理无意义且会污染备份还原语义；
// 手动配置但无 IP 时该行形如 "*IP address:"，同样视为未连接。
func ParseConnected(info string) bool {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "IP address") {
			continue
		}
		_, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		if v != "" && !strings.HasPrefix(v, "*") {
			return true
		}
	}
	return false
}

// ParseBypassDomains 解析 -getproxybypassdomains 输出（每行一个域名；空输出 → nil）。
func ParseBypassDomains(out string) []string {
	var domains []string
	for _, line := range nonEmptyLines(out) {
		// 某些系统版本空列表输出说明文字而非空串；"Empty" 是设置端清空关键字，读回也可能出现
		if strings.Contains(line, "There aren't any bypass domains") || strings.EqualFold(line, "Empty") {
			continue
		}
		domains = append(domains, line)
	}
	return domains
}

func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
