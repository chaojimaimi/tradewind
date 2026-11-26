// normalize_policy.go：配置白名单归一化器的策略表（批⑥b §4.2，I3/R16）。
//
// 本文件只放数据（键集合/上限/枚举），判定逻辑在 normalize.go；
// normalize_policy_test.go 证明 65 个顶层键恰好被下列集合划分（无遗漏、无重叠、
// 无多余），出站白名单 == v1.19.34 源码 28 种 − {tailscale, zerotier, easytier}。
//
// 名单来源：mihomo tag v1.19.34 源码逐个抄录（RawConfig / RawDNS / RawSniffer /
// RawProfile / RawExperimental / adapter/parser.go ParseProxy）。R24：内核升级时
// 必须复核本文件与测试里硬抄的名单，内核新增键/类型默认不在表内（fail-closed）。
package service

// 出站类型白名单（25）：v1.19.34 ParseProxy 的 28 种去掉 tailscale/zerotier/easytier
// ——三者是全部出站里仅有的会落盘 state-dir 的（M11），TUN 模式下 state 文件必须
// 不存在，内核升级新增类型默认不进白名单（R24）。
var outboundTypeAllowed = map[string]bool{
	"ss": true, "ssr": true, "socks5": true, "http": true, "vmess": true,
	"vless": true, "snell": true, "trojan": true, "hysteria": true, "hysteria2": true,
	"wireguard": true, "tuic": true, "shadowquic": true, "gost-relay": true,
	"direct": true, "dns": true, "reject": true, "rematch": true, "ssh": true,
	"mieru": true, "anytls": true, "sudoku": true, "masque": true,
	"trusttunnel": true, "openvpn": true,
}

// 钉死键：值整体替换为固定节点，用户原值不参与任何白名单校验（无论写了什么形状）。
// pinnedAppendOrder 是"键缺省时追加到顶层映射末尾"的固定顺序——追加位置固定才有幂等性。
var (
	pinnedKeys        = map[string]bool{"tun": true, "external-controller": true}
	pinnedAppendOrder = []string{"external-controller", "tun"}
)

// 直通·int [0,65535]：入站端口。
var portKeys = map[string]bool{
	"mixed-port": true, "port": true, "socks-port": true, "redir-port": true, "tproxy-port": true,
}

// 直通·枚举：值必须属于固定集合（!!str）。
var enumKeys = map[string]map[string]bool{
	"mode":              {"rule": true, "global": true, "direct": true},
	"log-level":         {"silent": true, "error": true, "warning": true, "info": true, "debug": true},
	"find-process-mode": {"always": true, "strict": true, "off": true},
	"geodata-loader":    {"standard": true, "memconservative": true},
}

// 直通·str ≤ N 字节（len(Value) 按字节——多字节字符按字节计，不是字符数）。
var strMaxKeys = map[string]int{
	"geosite-matcher":           64,
	"bind-address":              64,
	"interface-name":            64,
	"secret":                    256,
	"global-client-fingerprint": 64,
	"global-ua":                 512,
}

// 直通·bool。
var boolKeys = map[string]bool{
	"unified-delay": true, "ipv6": true, "tcp-concurrent": true,
	"inbound-tfo": true, "inbound-mptcp": true, "etag-support": true,
	"disable-keep-alive": true, "geodata-mode": true,
}

// 值条件拒绝·bool：键保留在允许面里（synth 每次都按 settings 注入这个键——整键拒绝
// 会把合法的 false 一并打掉），但显式真值在 TUN 形态硬拒；false/缺省照旧放行。
// WHY（2026-11-26 用户拍板）：真机发现 allow-lan: true 能经 settings → reload 推进
// root 内核，局域网拿到一个无鉴权的代理端口——TUN 形态下面向局域网的开入站一律不收。
var tunDeniedBoolKeys = map[string]bool{
	"allow-lan": true,
}

// 直通·int ≥ 0。
var nonNegIntKeys = map[string]bool{
	"keep-alive-idle": true, "keep-alive-interval": true, "routing-mark": true,
}

// 直通·序列：≤64 项，每项 !!str 且 ≤256 字节。
var seqStrKeys = map[string]bool{
	"authentication": true, "skip-auth-prefixes": true,
	"lan-allowed-ips": true, "lan-disallowed-ips": true,
}

// 结构校验键：值形状由 normalize.go 的结构校验器逐键判定。
var structKeys = map[string]bool{
	"proxies": true, "proxy-groups": true, "rules": true, "sub-rules": true,
	"proxy-providers": true, "rule-providers": true, "hosts": true,
	"dns": true, "sniffer": true, "profile": true, "experimental": true,
}

// 拒绝键（一律拒绝，Reason 统一 "is not allowed in TUN mode"）：
//   - external-controller-* / external-ui* / external-doh-server：监听面与 UI/DoH
//     文件写入面（M4/M10/M18：/upgrade、/ui/*、DoH 挂载点全由这些键点亮）；
//   - ntp：write-to-system 可改系统时钟（M7）；
//   - listeners / tls / tuic-server / ss-config / vmess-config / tunnels / iptables /
//     clash-for-android：额外出入站与系统面，合成器不产出；
//   - geox-url / geo-auto-update / geo-update-interval：TUN 下 geo 由服务从用户目录
//     同步（§4.1），不接受未校验内容的自动下载。
var rejectKeys = map[string]bool{
	"external-controller-unix": true, "external-controller-tls": true,
	"external-controller-pipe": true, "external-controller-cors": true,
	"external-controller-routing-mark": true,
	"external-ui":                      true, "external-ui-url": true, "external-ui-name": true,
	"external-doh-server": true,
	"ntp":                 true, "listeners": true, "tls": true, "tuic-server": true,
	"ss-config": true, "vmess-config": true, "tunnels": true, "iptables": true,
	"clash-for-android": true,
	"geox-url":          true, "geo-auto-update": true, "geo-update-interval": true,
}

// 嵌套键集合（v1.19.34 源码核对：RawDNS 27 键 / RawSniffer 11 键 /
// RawProfile 2 键 / RawExperimental 4 键；个数断言在 normalize_policy_test.go）。
var (
	dnsKeys = map[string]bool{
		"enable": true, "prefer-h3": true, "ipv6": true, "ipv6-timeout": true,
		"use-hosts": true, "use-system-hosts": true, "respect-rules": true,
		"nameserver": true, "fallback": true, "fallback-filter": true,
		"fallback-lazy-query": true, "listen": true, "listen-routing-mark": true,
		"enhanced-mode": true, "fake-ip-range": true, "fake-ip-range6": true,
		"fake-ip-filter": true, "fake-ip-filter-mode": true, "fake-ip-ttl": true,
		"default-nameserver": true, "cache-algorithm": true, "cache-max-size": true,
		"nameserver-policy": true, "proxy-server-nameserver": true,
		"proxy-server-nameserver-policy": true, "direct-nameserver": true,
		"direct-nameserver-follow-policy": true,
	}
	snifferKeys = map[string]bool{
		"enable": true, "override-destination": true, "sniffing": true,
		"force-domain": true, "skip-src-address": true, "skip-dst-address": true,
		"skip-domain": true, "port-whitelist": true, "force-dns-mapping": true,
		"parse-pure-ip": true, "sniff": true,
	}
	profileKeys = map[string]bool{"store-selected": true, "store-fake-ip": true}
	// RawExperimental 实为 4 键（config.go RawExperimental struct）
	experimentalKeys = map[string]bool{
		"fingerprints": true, "quic-go-disable-gso": true,
		"quic-go-disable-ecn": true, "dialer-ip4p-convert": true,
	}
)

// proxyGroupTypes：策略组类型白名单（批⑥b §4.2 结构校验细则）。
var proxyGroupTypes = map[string]bool{
	"select": true, "url-test": true, "fallback": true, "load-balance": true, "relay": true,
}
