// normalize_test.go：归一化器的表驱动用例（批⑥b §6-3）——解析纪律、顶层值校验、
// 结构校验、钉死键、错误文本不含配置值。golden 与幂等在 normalize_golden_test.go，
// 策略完备性在 normalize_policy_test.go。
package service

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mustAccept 断言输入被接受。
func mustAccept(t *testing.T, in string) Result {
	t.Helper()
	res, rej := NormalizeRuntime([]byte(in))
	if rej != nil {
		t.Fatalf("want accept, got rejection at %s: %s\ninput:\n%s", rej.Key, rej.Reason, in)
	}
	return res
}

// mustReject 断言输入被拒并返回拒绝原因。
func mustReject(t *testing.T, in string) *Rejection {
	t.Helper()
	_, rej := NormalizeRuntime([]byte(in))
	if rej == nil {
		t.Fatalf("want rejection, got accept:\n%s", in)
	}
	return rej
}

// rejectAt 断言输入被拒且键路径恰为 wantKey。
func rejectAt(t *testing.T, in, wantKey string) *Rejection {
	t.Helper()
	rej := mustReject(t, in)
	if rej.Key != wantKey {
		t.Fatalf("rejection key = %q, want %q (reason: %s)", rej.Key, wantKey, rej.Reason)
	}
	return rej
}

// TestNormalizeParseDiscipline 解析纪律（单文档/根映射/别名/锚点/合并键/tag 白名单）。
func TestNormalizeParseDiscipline(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		// 合法别名必引用同文档中先出现的锚点——先序遍历下锚点检查先命中（这里命中
		// 在 "base"）；AliasNode 分支本身由 TestNormalizeWalkerAliasBranch 直接覆盖
		{"alias to anchor", "base: &a\n  x: 1\ntarget: *a\n", "base"},
		{"anchor", "a: &anchor1 1\n", "a"},
		// 锚点挂在集合节点上：Anchor 检查先于 Kind 分支，映射/序列同样拒
		{"anchor on mapping", "a: &m\n  x: 1\n", "a"},
		{"anchor on sequence", "a: &s [1, 2]\n", "a"},
		// %TAG 指令重定义句柄：!! 指向别处后 !!binary 不再是核心 tag → 拒；把 !e!
		// 指回官方前缀，!e!binary 解出来就是 !!binary → 同样拒。产物里不会有指令
		{"TAG handle redefined", "%TAG !! tag:example.com,2000:app/\n---\na: !!binary aGk=\n", "a"},
		{"TAG handle aliases official prefix", "%TAG !e! tag:yaml.org,2002:\n---\na: !e!binary aGk=\n", "a"},
		// merge 键：!!merge 的 << 键由"映射键必须是 !!str 标量"拦下（此形态无需
		// 先出现锚点；带别名取值的形态会被先序的锚点检查更早命中）
		{"merge key", "m:\n  <<: 1\n", "m"},
		{"binary tag", "b: !!binary aGVsbG8=\n", "b"},
		{"custom tag", "c: !mytag foo\n", "c"},
		{"multi document", "mode: rule\n---\nmode: global\n", "document"},
		{"empty input", "", "document"},
		{"comment only", "# nothing here\n", "document"},
		{"sequence root", "- a\n- b\n", "root"},
		{"scalar root", "just a string\n", "root"},
		{"null root", "null\n", "root"},
	}
	for _, tc := range cases {
		rej := rejectAt(t, tc.in, tc.wantKey)
		if rej.Reason == "" {
			t.Fatalf("%s: empty reason", tc.name)
		}
	}
}

// TestNormalizeWalkerAliasBranch：AliasNode 分支的直测（端到端输入里合法别名必先
// 经过其锚点，锚点检查按文档顺序先命中——见 TestNormalizeParseDiscipline）。
func TestNormalizeWalkerAliasBranch(t *testing.T) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "a"},
		&yaml.Node{Kind: yaml.AliasNode, Value: "x",
			Alias: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "x"}},
	)
	var w treeWalker
	if rej := w.visit(root, "", 1); rej == nil || rej.Key != "a" || !strings.Contains(rej.Reason, "aliases") {
		t.Fatalf("alias branch: got %v, want rejection at a mentioning aliases", rej)
	}
}

// TestNormalizeSyntaxErrorNoLeak：语法错误只回 "invalid YAML" + 行号，不透传 yaml.v3
// 错误文本（可能带出 token 内容）。
func TestNormalizeSyntaxErrorNoLeak(t *testing.T) {
	const s = "SENTINEL-LEAK-9f2b"
	rej := mustReject(t, "mode: ["+s+"\n")
	if !regexp.MustCompile(`^invalid YAML \(line \d+\)$`).MatchString(rej.Reason) {
		t.Fatalf("reason = %q, want invalid YAML with line number only", rej.Reason)
	}
	if strings.Contains(rej.Reason, s) || strings.Contains(rej.Key, s) {
		t.Fatalf("error text leaks token content: %q", rej.Reason)
	}
}

// TestNormalizeDepthLimit：深度 129 层拒绝、128 层通过。语义（实现取最保守读法）：
// 全树统一计层数——顶层映射记第 1 层，任一节点（含标量叶子）超过 128 层即拒。
// 深嵌套必须挂在合法顶层键下（未知顶层键会被策略先拒），选 proxies 项内 name/
// type/state-dir 之外的自由字段。
func deepUnderProxies(depth int) string {
	// 节点深度：根映射(1) → proxies 序列(2) → 项映射(3) → x 值映射(4) → k 层
	// a 嵌套 → 标量叶子（恰在第 depth 层，k = depth-5）。最内层也要花括号，否则
	// a: leaf: 1 在 flow 上下文里本身就不是合法 YAML。
	inner := "{leaf: 1}"
	for i := 0; i < depth-5; i++ {
		inner = "{a: " + inner + "}"
	}
	return "proxies:\n  - name: n\n    type: ss\n    server: 1.2.3.4\n    port: 1\n    x: " + inner + "\n"
}

func TestNormalizeDepthLimit(t *testing.T) {
	mustAccept(t, deepUnderProxies(128))
	rej := mustReject(t, deepUnderProxies(129))
	if !strings.Contains(rej.Reason, "128") {
		t.Fatalf("reason = %q, want it to name the 128 level limit", rej.Reason)
	}
}

// TestNormalizeNodeBudget：节点总数 > 1e6 拒绝。构造走"多而小"：flow 序列每项
// 2 字节，1.1e6 项约 2.2MB（字节粗闸 8MB 之内）。
func TestNormalizeNodeBudget(t *testing.T) {
	in := "a: [" + strings.Repeat("1,", 1_100_000) + "1]\n"
	rej := mustReject(t, in)
	if rej.Key != "document" || !strings.Contains(rej.Reason, "1000000") {
		t.Fatalf("key/reason = %s / %q, want document / node budget", rej.Key, rej.Reason)
	}
}

// TestNormalizeDuplicateKeys：重复键顶层与嵌套各一例（yaml.v3 对 Node 解码不报
// 重复——必须显式拦）。
func TestNormalizeDuplicateKeys(t *testing.T) {
	rejectAt(t, "mode: rule\nmode: global\n", "mode")
	rejectAt(t, "dns:\n  enable: true\n  enable: false\n", "dns.enable")
}

// TestNormalizeTopLevelUnknownKey：全集之外的键一律 unknown key；键名可回显但截断
// 64 字节。
func TestNormalizeTopLevelUnknownKey(t *testing.T) {
	rejectAt(t, "my-custom-key: 1\n", "my-custom-key")
	long := strings.Repeat("k", 300)
	rej := rejectAt(t, long+": 1\n", strings.Repeat("k", 64))
	if !strings.Contains(rej.Reason, "unknown key") {
		t.Fatalf("reason = %q, want unknown key", rej.Reason)
	}
}

// TestNormalizeRejectList：拒绝列表逐键一例（批⑥b §4.2 策略表：监听面/系统面/geo 下载面）。
func TestNormalizeRejectList(t *testing.T) {
	for key := range rejectKeys {
		in := "mode: rule\n" + key + ": whatever\n"
		rej := rejectAt(t, in, key)
		want := key + " is not allowed in TUN mode"
		if rej.Reason != want {
			t.Fatalf("key %s reason = %q, want %q", key, rej.Reason, want)
		}
	}
}

// TestNormalizeScalarValueChecks：直通键的每一类值校验（负数/越界/非整数/枚举外/
// bool 位放字符串/str 按字节超长/序列项数与单项字节）。
func TestNormalizeScalarValueChecks(t *testing.T) {
	twoByte := strings.Repeat("ß", 40) // 40 字符 = 80 字节：按字符数 ≤64 会误放行
	twoByteOK := strings.Repeat("ß", 32)
	cases := []struct {
		name    string
		in      string
		wantKey string // 空串 = 应接受
	}{
		{"port negative", "port: -1\n", "port"},
		{"port 65536", "socks-port: 65536\n", "socks-port"},
		{"port non-int", "mixed-port: \"7890\"\n", "mixed-port"},
		{"port float", "redir-port: 7890.5\n", "redir-port"},
		{"port 0 ok", "tproxy-port: 0\n", ""},
		{"port 65535 ok", "mixed-port: 65535\n", ""},
		{"mode outside enum", "mode: empire\n", "mode"},
		{"mode ok", "mode: rule\n", ""},
		{"log-level outside enum", "log-level: verbose\n", "log-level"},
		{"find-process-mode outside enum", "find-process-mode: maybe\n", "find-process-mode"},
		{"geodata-loader outside enum", "geodata-loader: fast\n", "geodata-loader"},
		{"bool as string", "allow-lan: \"true\"\n", "allow-lan"},
		{"bool as int", "unified-delay: 1\n", "unified-delay"},
		{"bools ok", "ipv6: true\ntcp-concurrent: false\netag-support: true\n", ""},
		{"nonneg negative", "routing-mark: -1\n", "routing-mark"},
		{"nonneg int as string", "keep-alive-idle: \"15\"\n", "keep-alive-idle"},
		{"nonneg ok", "keep-alive-idle: 15\nkeep-alive-interval: 0\n", ""},
		{"str 80 bytes for 64 cap", "interface-name: " + twoByte + "\n", "interface-name"},
		{"str 64 bytes ok", "interface-name: " + twoByteOK + "\n", ""},
		{"secret 257 bytes", "secret: " + strings.Repeat("s", 257) + "\n", "secret"},
		{"secret 256 bytes ok", "secret: " + strings.Repeat("s", 256) + "\n", ""},
		{"global-ua 513 bytes", "global-ua: " + strings.Repeat("u", 513) + "\n", "global-ua"},
		{"global-ua 512 bytes ok", "global-ua: " + strings.Repeat("u", 512) + "\n", ""},
		{"seq 65 items", "authentication:\n" + strings.Repeat("  - u:p\n", 65), "authentication"},
		{"seq 64 items ok", "authentication:\n" + strings.Repeat("  - u:p\n", 64), ""},
		{"seq item 257 bytes", "authentication:\n  - " + strings.Repeat("a", 257) + "\n", "authentication[0]"},
		{"seq item 256 bytes ok", "authentication:\n  - " + strings.Repeat("a", 256) + "\n", ""},
		{"seq not a sequence", "lan-allowed-ips: 0.0.0.0/0\n", "lan-allowed-ips"},
		{"str key given int", "bind-address: 12345\n", "bind-address"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
}
