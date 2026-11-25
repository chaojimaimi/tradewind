// synth 包内 dns.go：TUN 模式（批⑥ §3.2）注入的 dns/tun 块常量与 yaml 节点构造。
//
// 键值内置不暴露设置键（DNS 调参 UI 留观察期，YAGNI）；fake-ip-filter 纯域名通配
// （评审 P2：geosite.dat 供给链路本项目未实现，geosite: 前缀条目一律不用）。
// 键序固定追加，输出确定性（golden 测试前提）。
package synth

import "gopkg.in/yaml.v3"

// fakeIPFilter 是 fake-ip 白名单（mihomo fake-ip-filter）：命中条目走真实解析，
// 其余域名落 198.18.0.1/16 假地址进 TUN。黑名单语义（F3）：局域网/NTP/STUN/各系统
// 连通性检测必须拿到真实 IP——系统时钟同步、WebRTC 打洞、captive portal 判定被
// fake-ip 干扰即功能损坏。合计 27 条（纯域名/通配，测试逐条断言）。
var fakeIPFilter = []string{
	// 局域网/本地域：mDNS、路由器面板等本地解析不走假地址
	"*.lan",
	"*.local",
	"*.localhost",
	"*.localdomain",
	"*.internal",
	// NTP：时钟同步必须真实 IP（fake-ip 会让时间同步进 TUN 栈出错）
	"time.*.apple.com",
	"time.windows.com",
	"time.apple.com",
	"time.cloudflare.com",
	"time.android.com",
	"ntp.*.com",
	"ntp.ubuntu.com",
	// Apple captive portal 连通性检测
	"captive.apple.com",
	// Windows NCSI 连通性检测
	"dns.msftncsi.com",
	"www.msftncsi.com",
	"www.msftconnecttest.com",
	// Android/Chromium 连通性检测
	"connectivitycheck.gstatic.com",
	// 国产 ROM 连通性检测/路由器域
	"connect.rom.miui.com",
	"connectivitycheck.platform.hicloud.com",
	"wifi.vivo.com.cn",
	"*.miwifi.com",
	// STUN：WebRTC/P2P 打洞需要真实地址（fake-ip 会破坏 ICE 候选）
	"stun.*",
	"stun.*.*",
	"stun.*.*.*",
	"*.stun.*",
	"*.stun.*.*",
	"*.stun.*.*.*",
}

// dnsBootstrapServers 对齐模板最小 dns 段的 nameserver（F5）；dnsProxyServers 让
// 代理服务器域名走加密 DNS 防鸡生蛋（F3）；fallback 为加密 DoT（评审 P3：fallback
// 分流范式官方趋势为 nameserver-policy，常量块观察期再评估切换）。
var (
	dnsDefaultServers  = []string{"223.5.5.5", "119.29.29.29"}
	dnsProxyServers    = []string{"tls://223.5.5.5:853"}
	dnsFallbackServers = []string{"tls://8.8.8.8:853", "tls://1.1.1.1:853"}
)

// TunBlockNode 构造 tun 块：system 栈 + auto-route + dns-hijack any:53（DNS 劫持
// 覆盖全部明文查询——系统 DNS 接管（DESIGN #17）因此不进本批）。
// 导出供 service 归一化器共用（批⑥b §4.2）：TUN 期间内核 tun 块的唯一来源，
// 合成注入与归一化钉死读同一份定义，杜绝两处常量漂移。
func TunBlockNode() *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	setMappingValue(m, "enable", scalarNode(true))
	setMappingValue(m, "stack", scalarNode("system"))
	setMappingValue(m, "auto-route", scalarNode(true))
	setMappingValue(m, "auto-detect-interface", scalarNode(true))
	setMappingValue(m, "dns-hijack", seqNode([]string{"any:53"}))
	return m
}

// dnsBlockNode 构造 dns 白名单块（fake-ip 模式完整键域）。fallback-filter 依赖
// 数据目录 geoip.metadb——其就位由 tun on 编排前置检查保证（批⑥ §3.2），不写
// 无条件环境断言（评审二轮 P2-d）。
func dnsBlockNode() *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	setMappingValue(m, "enable", scalarNode(true))
	setMappingValue(m, "ipv6", scalarNode(false))
	setMappingValue(m, "enhanced-mode", scalarNode("fake-ip"))
	setMappingValue(m, "fake-ip-range", scalarNode("198.18.0.1/16"))
	setMappingValue(m, "fake-ip-filter", seqNode(fakeIPFilter))
	setMappingValue(m, "default-nameserver", seqNode(dnsDefaultServers))
	setMappingValue(m, "proxy-server-nameserver", seqNode(dnsProxyServers))
	setMappingValue(m, "nameserver", seqNode(dnsDefaultServers))
	setMappingValue(m, "fallback", seqNode(dnsFallbackServers))
	filter := &yaml.Node{Kind: yaml.MappingNode}
	setMappingValue(filter, "geoip", scalarNode(true))
	setMappingValue(filter, "geoip-code", scalarNode("CN"))
	setMappingValue(m, "fallback-filter", filter)
	return m
}

// seqNode 构造字符串序列节点（块风格；yaml.Encoder 统一缩进 2，键序确定）。
func seqNode(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, item := range items {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: item})
	}
	return n
}
