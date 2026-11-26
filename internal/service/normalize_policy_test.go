// normalize_policy_test.go：策略表的完备性断言（批⑥b §6-3）。
//
// 三条不变量：65 个顶层键恰好被策略集合划分（无遗漏、无重叠、无多余）；出站白名单
// == 源码 28 种 − {tailscale, zerotier, easytier}；嵌套键集合个数 27/11/2/4。
// 名单全部从 mihomo v1.19.34 源码逐个抄录（测试不得依赖 /tmp 下的克隆）。
package service

import (
	"testing"
)

// rawConfigYAMLKeys = v1.19.34 config/config.go RawConfig 的全部 yaml 键（65 个）。
var rawConfigYAMLKeys = []string{
	"port", "socks-port", "redir-port", "tproxy-port", "mixed-port", "ss-config",
	"vmess-config", "inbound-tfo", "inbound-mptcp", "authentication",
	"skip-auth-prefixes", "lan-allowed-ips", "lan-disallowed-ips", "allow-lan",
	"bind-address", "mode", "unified-delay", "log-level", "ipv6",
	"external-controller", "external-controller-routing-mark",
	"external-controller-pipe", "external-controller-unix", "external-controller-tls",
	"external-controller-cors", "external-ui", "external-ui-url", "external-ui-name",
	"external-doh-server", "secret", "interface-name", "routing-mark", "tunnels",
	"geo-auto-update", "geo-update-interval", "geodata-mode", "geodata-loader",
	"geosite-matcher", "tcp-concurrent", "find-process-mode",
	"global-client-fingerprint", "global-ua", "etag-support", "keep-alive-idle",
	"keep-alive-interval", "disable-keep-alive", "proxy-providers", "rule-providers",
	"proxies", "proxy-groups", "rules", "sub-rules", "listeners", "hosts", "dns",
	"ntp", "tun", "tuic-server", "iptables", "experimental", "profile", "geox-url",
	"sniffer", "tls", "clash-for-android",
}

// parseProxySourceTypes = v1.19.34 adapter/parser.go ParseProxy 的全部 28 种出站类型。
var parseProxySourceTypes = []string{
	"ss", "ssr", "socks5", "http", "vmess", "vless", "snell", "trojan", "hysteria",
	"hysteria2", "wireguard", "tuic", "shadowquic", "gost-relay", "direct", "dns",
	"reject", "rematch", "ssh", "mieru", "anytls", "sudoku", "masque", "trusttunnel",
	"openvpn", "tailscale", "zerotier", "easytier",
}

// policySets 顶层策略全集（钉死 + 各直通 + 结构校验 + 拒绝）。enum 集合取的是
// 枚举键的键名（mode/log-level/…），choice 值只进 Reason 文案，不参与划分。
func policySets() map[string]map[string]bool {
	return map[string]map[string]bool{
		"pinned":        pinnedKeys,
		"port":          portKeys,
		"enum":          setOf(enumKeys),
		"strMax":        setOf(strMaxKeys),
		"bool":          boolKeys,
		"tunDeniedBool": tunDeniedBoolKeys,
		"nonNegInt":     nonNegIntKeys,
		"seqStr":        seqStrKeys,
		"struct":        structKeys,
		"reject":        rejectKeys,
	}
}

func setOf[T any](m map[string]T) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// TestTopLevelKeysExactlyPartitioned 断言 65 个顶层键被策略集合划分。
func TestTopLevelKeysExactlyPartitioned(t *testing.T) {
	if len(rawConfigYAMLKeys) != 65 {
		t.Fatalf("rawConfigYAMLKeys has %d entries, want 65 (v1.19.34 RawConfig)", len(rawConfigYAMLKeys))
	}
	all := make(map[string]bool, len(rawConfigYAMLKeys))
	for _, k := range rawConfigYAMLKeys {
		if all[k] {
			t.Fatalf("duplicate key in rawConfigYAMLKeys: %s", k)
		}
		all[k] = true
	}
	sets := policySets()
	// 无重叠：任两集合交集为空
	names := make([]string, 0, len(sets))
	for name := range sets {
		names = append(names, name)
	}
	for i, a := range names {
		for _, b := range names[i+1:] {
			for key := range sets[a] {
				if sets[b][key] {
					t.Fatalf("key %q in both %s and %s", key, a, b)
				}
			}
		}
	}
	// 无遗漏、无多余：并集恰为 65 键全集
	union := make(map[string]bool)
	for _, name := range names {
		if len(sets[name]) == 0 {
			t.Fatalf("policy set %s is empty", name)
		}
		for key := range sets[name] {
			union[key] = true
		}
	}
	if len(union) != len(all) {
		t.Fatalf("policy union has %d keys, want %d", len(union), len(all))
	}
	for key := range all {
		if !union[key] {
			t.Fatalf("key %q in RawConfig but in no policy set", key)
		}
	}
	for key := range union {
		if !all[key] {
			t.Fatalf("key %q in a policy set but not in RawConfig", key)
		}
	}
}

// TestOutboundWhitelistMatchesSource 断言：白名单 25 个 == 源码 28 种 −
// {tailscale, zerotier, easytier}（后者是仅有的会落盘 state-dir 的出站，M11）。
func TestOutboundWhitelistMatchesSource(t *testing.T) {
	if len(parseProxySourceTypes) != 28 {
		t.Fatalf("parseProxySourceTypes has %d entries, want 28 (v1.19.34 ParseProxy)", len(parseProxySourceTypes))
	}
	seen := make(map[string]bool, len(parseProxySourceTypes))
	for _, typ := range parseProxySourceTypes {
		if seen[typ] {
			t.Fatalf("duplicate type in parseProxySourceTypes: %s", typ)
		}
		seen[typ] = true
	}
	// 等式：28 种 − 白名单 25 种 == {tailscale, zerotier, easytier}
	excluded := map[string]bool{
		"tailscale": true, "zerotier": true, "easytier": true,
	}
	if len(outboundTypeAllowed) != 25 {
		t.Fatalf("outboundTypeAllowed has %d entries, want 25", len(outboundTypeAllowed))
	}
	for typ := range outboundTypeAllowed {
		if !seen[typ] {
			t.Fatalf("whitelist type %q not in v1.19.34 ParseProxy", typ)
		}
		if excluded[typ] {
			t.Fatalf("state-dir type %q must not be whitelisted in TUN mode", typ)
		}
	}
	for typ := range seen {
		if !outboundTypeAllowed[typ] && !excluded[typ] {
			t.Fatalf("type %q in source but neither whitelisted nor explicitly excluded", typ)
		}
	}
}

// TestNestedKeySetSizes 防手抄漏项：RawDNS 27 / RawSniffer 11 / RawProfile 2 /
// RawExperimental 4。任务书写 experimental 为 5 键但列出的 4 个键名与源码一致——
// v1.19.34 config.go RawExperimental struct 实为 4 个字段，以源码为准（4）。
func TestNestedKeySetSizes(t *testing.T) {
	cases := []struct {
		name string
		got  map[string]bool
		want int
	}{
		{"dnsKeys", dnsKeys, 27},
		{"snifferKeys", snifferKeys, 11},
		{"profileKeys", profileKeys, 2},
		{"experimentalKeys", experimentalKeys, 4},
	}
	for _, tc := range cases {
		if len(tc.got) != tc.want {
			t.Fatalf("%s has %d keys, want %d", tc.name, len(tc.got), tc.want)
		}
	}
	// 组类型白名单非空（select/url-test/fallback/load-balance/relay）
	if len(proxyGroupTypes) != 5 {
		t.Fatalf("proxyGroupTypes has %d entries, want 5", len(proxyGroupTypes))
	}
}
