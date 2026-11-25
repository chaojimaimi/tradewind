// normalize_struct_test.go：结构校验器与钉死键的表驱动用例（批⑥b §6-3 第 5-10 项）。
package service

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func TestNormalizeProxiesChecks(t *testing.T) {
	proxy := func(name, typ, extra string) string {
		return "proxies:\n  - name: " + name + "\n    type: " + typ + "\n    server: 1.2.3.4\n    port: 1\n" + extra
	}
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		{"ss with credentials ok", proxy("n1", "ss", "    password: test-pw\n    cipher: aes-128-gcm\n"), ""},
		{"tailscale stateful", proxy("n1", "tailscale", ""), "proxies[0].type"},
		{"zerotier stateful", proxy("n1", "zerotier", ""), "proxies[0].type"},
		{"easytier stateful", proxy("n1", "easytier", ""), "proxies[0].type"},
		{"unknown type", proxy("n1", "sso", ""), "proxies[0].type"},
		{"missing name", "proxies:\n  - type: ss\n    server: 1.2.3.4\n    port: 1\n", "proxies[0].name"},
		{"missing type", "proxies:\n  - name: n1\n    server: 1.2.3.4\n    port: 1\n", "proxies[0].type"},
		{"empty name", proxy("\"\"", "ss", ""), "proxies[0].name"},
		{"257 byte name", proxy(strings.Repeat("n", 257), "ss", ""), "proxies[0].name"},
		{"256 byte name ok", proxy(strings.Repeat("n", 256), "ss", ""), ""},
		{"state-dir present", proxy("n1", "ss", "    state-dir: ./ts\n"), "proxies[0].state-dir"},
		{"4097 items", "proxies:\n" + strings.Repeat("  - {name: n, type: ss, server: 1.1.1.1, port: 1}\n", 4097), "proxies"},
		{"4096 items ok", "proxies:\n" + strings.Repeat("  - {name: n, type: ss, server: 1.1.1.1, port: 1}\n", 4096), ""},
		{"non mapping item", "proxies:\n  - just-a-string\n", "proxies[0]"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
}

func TestNormalizeProxyGroupChecks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		{"select ok", "proxy-groups:\n  - name: g1\n    type: select\n    proxies: [a]\n", ""},
		{"url-test ok", "proxy-groups:\n  - name: g1\n    type: url-test\n    proxies: [a]\n", ""},
		{"bad type", "proxy-groups:\n  - name: g1\n    type: smart\n    proxies: [a]\n", "proxy-groups[0].type"},
		{"missing name", "proxy-groups:\n  - type: select\n    proxies: [a]\n", "proxy-groups[0].name"},
		{"empty name", "proxy-groups:\n  - name: \"\"\n    type: select\n    proxies: [a]\n", "proxy-groups[0].name"},
		// 组名与节点名同限 [1,256]：组名进内核 selector 状态与缓存，没有理由更宽松
		{"257 byte name", "proxy-groups:\n  - name: " + strings.Repeat("g", 257) + "\n    type: select\n    proxies: [a]\n", "proxy-groups[0].name"},
		{"256 byte name ok", "proxy-groups:\n  - name: " + strings.Repeat("g", 256) + "\n    type: select\n    proxies: [a]\n", ""},
		{"missing type", "proxy-groups:\n  - name: g1\n    proxies: [a]\n", "proxy-groups[0].type"},
		{"non mapping item", "proxy-groups:\n  - g1\n", "proxy-groups[0]"},
		{"not a sequence", "proxy-groups:\n  g1: select\n", "proxy-groups"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
}

func TestNormalizeRulesChecks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		{"rules ok", "rules:\n  - MATCH,DIRECT\n", ""},
		{"non string item", "rules:\n  - MATCH,DIRECT\n  - 3\n", "rules[1]"},
		{"item 4097 bytes", "rules:\n  - " + strings.Repeat("D", 4097) + "\n", "rules[0]"},
		{"item 4096 bytes ok", "rules:\n  - " + strings.Repeat("D", 4096) + "\n", ""},
		{"65537 items", "rules:\n" + strings.Repeat("  - MATCH,DIRECT\n", 65537), "rules"},
		{"not a sequence", "rules: MATCH,DIRECT\n", "rules"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
}

func TestNormalizeSubRulesChecks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		{"ok", "sub-rules:\n  r1:\n    - MATCH,DIRECT\n", ""},
		{"value not a sequence", "sub-rules:\n  r1: MATCH,DIRECT\n", "sub-rules.r1"},
		{"item too long", "sub-rules:\n  r1:\n    - " + strings.Repeat("D", 4097) + "\n", "sub-rules.r1[0]"},
		{"non string item", "sub-rules:\n  r1:\n    - 3\n", "sub-rules.r1[0]"},
		{"total over limit", "sub-rules:\n  r1:\n" + strings.Repeat("    - MATCH,DIRECT\n", 32769) +
			"  r2:\n" + strings.Repeat("    - MATCH,DIRECT\n", 32769), "sub-rules"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
	// 恰好 65536 条（两半分摊）应通过
	total := strings.Repeat("    - MATCH,DIRECT\n", 32768)
	mustAccept(t, "sub-rules:\n  r1:\n"+total+"  r2:\n"+total)
}

func TestNormalizeProviderChecks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		{"file rejected", "proxy-providers:\n  sub:\n    type: file\n    path: ./p.yaml\n", "proxy-providers.sub.type"},
		{"rule file rejected", "rule-providers:\n  rp:\n    type: file\n", "rule-providers.rp.type"},
		{"http ok", "proxy-providers:\n  sub:\n    type: http\n    url: https://p.example.invalid/s\n", ""},
		{"inline ok", "proxy-providers:\n  sub:\n    type: inline\n    payload: []\n", ""},
		{"missing type", "proxy-providers:\n  sub:\n    url: https://p.example.invalid/s\n", "proxy-providers.sub.type"},
		{"unknown type", "proxy-providers:\n  sub:\n    type: sftp\n", "proxy-providers.sub.type"},
		{"name 129 bytes", "proxy-providers:\n  " + strings.Repeat("n", 129) + ":\n    type: http\n",
			"proxy-providers." + strings.Repeat("n", 64)},
		{"empty name", "proxy-providers:\n  \"\":\n    type: http\n", "proxy-providers."},
		{"body not a mapping", "proxy-providers:\n  sub: http\n", "proxy-providers.sub"},
		{"not a mapping", "proxy-providers: []\n", "proxy-providers"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
	// 条目数：256 个（键名互异）通过、257 个拒绝；file 的 Reason 必须指引 enhance 槽
	entries := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("  p" + strconv.Itoa(i) + ":\n    type: inline\n")
		}
		return "proxy-providers:\n" + b.String()
	}
	mustAccept(t, entries(256))
	rejectAt(t, entries(257), "proxy-providers")
	rej := rejectAt(t, "rule-providers:\n  rp:\n    type: file\n", "rule-providers.rp.type")
	if !strings.Contains(rej.Reason, "enhance") {
		t.Fatalf("file reason = %q, want it to mention the enhance slot", rej.Reason)
	}
}

func TestNormalizeProviderPathStripped(t *testing.T) {
	// http 带 path：剥离并入 Ignored（恰一条、路径前缀正确、输出无 path 键）
	res := mustAccept(t, "proxy-providers:\n  sub:\n    type: http\n    url: https://p.example.invalid/s\n    path: ./sub.yaml\n")
	if got, want := res.Ignored, "proxy-providers.sub.path"; len(got) != 1 || got[0] != want {
		t.Fatalf("Ignored = %v, want [%s]", got, want)
	}
	if strings.Contains(string(res.Bytes), "path") {
		t.Fatalf("output still contains path:\n%s", res.Bytes)
	}
	// rule-providers 同理
	res = mustAccept(t, "rule-providers:\n  rp:\n    type: http\n    url: https://p.example.invalid/r\n    path: ./rp.yaml\n")
	if got, want := res.Ignored, "rule-providers.rp.path"; len(got) != 1 || got[0] != want {
		t.Fatalf("Ignored = %v, want [%s]", got, want)
	}
	// 无 path：Ignored 为空
	res = mustAccept(t, "proxy-providers:\n  sub:\n    type: http\n    url: https://p.example.invalid/s\n")
	if len(res.Ignored) != 0 {
		t.Fatalf("Ignored = %v, want empty", res.Ignored)
	}
	// 按出现顺序：rule-providers 在文档里先出现，其剥离记录排在前
	res = mustAccept(t, "rule-providers:\n  rp:\n    type: http\n    path: ./a\nproxy-providers:\n  sub:\n    type: http\n    path: ./b\n")
	want := []string{"rule-providers.rp.path", "proxy-providers.sub.path"}
	if len(res.Ignored) != 2 || res.Ignored[0] != want[0] || res.Ignored[1] != want[1] {
		t.Fatalf("Ignored = %v, want %v (document order)", res.Ignored, want)
	}
}

func TestNormalizeDNSAndNestedBlocks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKey string
	}{
		{"dns listen non-empty", "dns:\n  listen: 0.0.0.0:1053\n", "dns.listen"},
		{"dns listen empty ok", "dns:\n  listen: \"\"\n", ""},
		{"dns listen null ok", "dns:\n  listen:\n", ""},
		{"dns listen absent ok", "dns:\n  enable: true\n", ""},
		// listen 的非标量形态：不是 "缺省/!!null/空 !!str" 三者之一，一律拒
		{"dns listen sequence", "dns:\n  listen: [0.0.0.0:1053]\n", "dns.listen"},
		{"dns listen mapping", "dns:\n  listen: {host: 0.0.0.0}\n", "dns.listen"},
		{"dns listen int", "dns:\n  listen: 53\n", "dns.listen"},
		{"dns unknown key", "dns:\n  bogus: true\n", "dns.bogus"},
		{"dns not a mapping", "dns: 1\n", "dns"},
		{"sniffer unknown key", "sniffer:\n  bogus: true\n", "sniffer.bogus"},
		{"sniffer ok", "sniffer:\n  enable: true\n  sniff: {HTTP: []}\n", ""},
		{"profile unknown key", "profile:\n  bogus: true\n", "profile.bogus"},
		{"profile non bool", "profile:\n  store-selected: \"true\"\n", "profile.store-selected"},
		{"profile ok", "profile:\n  store-selected: true\n  store-fake-ip: false\n", ""},
		{"experimental unknown key", "experimental:\n  bogus: true\n", "experimental.bogus"},
		{"experimental ok", "experimental:\n  quic-go-disable-gso: true\n", ""},
		{"hosts scalar ok", "hosts:\n  a.example.invalid: 1.2.3.4\n", ""},
		{"hosts seq of scalars ok", "hosts:\n  a.example.invalid:\n    - 1.2.3.4\n    - 1.2.3.5\n", ""},
		{"hosts mapping value", "hosts:\n  a.example.invalid:\n    b: 1\n", "hosts.a.example.invalid"},
		{"hosts seq of maps", "hosts:\n  a.example.invalid:\n    - {b: 1}\n", "hosts.a.example.invalid"},
		{"hosts not a mapping", "hosts: 1\n", "hosts"},
	}
	for _, tc := range cases {
		if tc.wantKey == "" {
			mustAccept(t, tc.in)
			continue
		}
		rejectAt(t, tc.in, tc.wantKey)
	}
}

// TestNormalizePinnedKeys：两个钉死键——用户的值无论什么形状（标量、带额外键的映射、
// 恶意键）都被整体替换；缺省时按固定顺序追加到末尾。
func TestNormalizePinnedKeys(t *testing.T) {
	assertTunBlock := func(t *testing.T, out []byte, where string) {
		t.Helper()
		var doc yaml.Node
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("%s: output not valid yaml: %v", where, err)
		}
		root := doc.Content[0]
		tun, ok := mappingGet(root, "tun")
		if !ok {
			t.Fatalf("%s: tun missing from output:\n%s", where, out)
		}
		if tun.Kind != yaml.MappingNode {
			t.Fatalf("%s: tun must be replaced by a mapping, got kind %d", where, tun.Kind)
		}
		enable, _ := mappingGet(tun, "enable")
		stack, _ := mappingGet(tun, "stack")
		if enable.Value != "true" || stack.Value != "system" {
			t.Fatalf("%s: tun.enable/stack = %s/%s, want true/system", where, enable.Value, stack.Value)
		}
		for _, evil := range []string{"device", "external-ui", "route-exclude-address"} {
			if _, present := mappingGet(tun, evil); present {
				t.Fatalf("%s: malicious tun key %s survived", where, evil)
			}
		}
		hijack, ok := mappingGet(tun, "dns-hijack")
		if !ok || len(hijack.Content) != 1 || hijack.Content[0].Value != "any:53" {
			t.Fatalf("%s: tun.dns-hijack = %v, want [any:53]", where, hijack)
		}
	}

	// 任意形状都被整体替换
	scalar := mustAccept(t, "mode: rule\ntun: garbage\n")
	assertTunBlock(t, scalar.Bytes, "scalar")
	malicious := mustAccept(t, "tun:\n  enable: false\n  device: evil0\n  external-ui: /tmp/evil\n  route-exclude-address: [0.0.0.0/0]\n")
	assertTunBlock(t, malicious.Bytes, "malicious")

	// external-controller 有值 → 替换为 ""
	ec := mustAccept(t, "external-controller: 1.2.3.4:9090\n")
	if !strings.Contains(string(ec.Bytes), "external-controller: \"\"\n") {
		t.Fatalf("external-controller not pinned to empty string:\n%s", ec.Bytes)
	}

	// 两键缺省 → 追加在末尾，顺序固定为先 external-controller 后 tun
	appended := mustAccept(t, "mode: rule\nlog-level: info\n")
	out := string(appended.Bytes)
	ecIdx := strings.Index(out, "external-controller:")
	tunIdx := strings.Index(out, "\ntun:")
	if ecIdx < 0 || tunIdx < 0 || ecIdx > tunIdx {
		t.Fatalf("append order wrong (ec@%d tun@%d):\n%s", ecIdx, tunIdx, out)
	}
	assertTunBlock(t, appended.Bytes, "appended")

	// 已存在的键原位替换，不挪到末尾（输入含两钉死键，替换不改序）
	inPlace := mustAccept(t, "mode: rule\ntun: garbage\nlog-level: info\nexternal-controller: 1.2.3.4:9090\n")
	var doc yaml.Node
	if err := yaml.Unmarshal(inPlace.Bytes, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i < len(doc.Content[0].Content); i += 2 {
		keys = append(keys, doc.Content[0].Content[i].Value)
	}
	if len(keys) != 4 || keys[0] != "mode" || keys[1] != "tun" || keys[2] != "log-level" || keys[3] != "external-controller" {
		t.Fatalf("in-place replacement changed key order: %v\n%s", keys, inPlace.Bytes)
	}
	assertTunBlock(t, inPlace.Bytes, "in-place")
}

// TestNormalizeNoValueLeak：错误文本不含任何配置值。哨兵串只放在"值"的位置
// （密码/uuid/服务器/路径/枚举位/规则条目），逐例断言 Key/Reason/Error 都不含它。
// 例外（有注释说明）：哨兵作为映射"键名"出现时允许回显——键名回显是让用户知道
// 删哪个键，回显前已截断 64 字节并抹掉控制字符。
func TestNormalizeNoValueLeak(t *testing.T) {
	const s = "SENTINEL-SECRET-7c1e"
	cases := []struct {
		name string
		in   string
	}{
		{"enum position", "mode: " + s + "\n"},
		{"int position", "mixed-port: " + s + "\n"},
		{"bool position", "allow-lan: " + s + "\n"},
		{"secret too long", "secret: " + strings.Repeat(s, 20) + "\n"},
		{"unknown key value", "unknown-key: " + s + "\n"},
		// 同一映射里哨兵在 server/uuid/password/ws-opts.path 等值位置，让 type 落在
		// 白名单外触发拒绝——拒绝只报 type 位，兄弟键的值绝不回显
		{"proxy server/uuid/password", "proxies:\n  - name: p1\n    type: " + s + "\n    server: " + s +
			"\n    port: 1\n    uuid: " + s + "\n    password: " + s + "\n    ws-opts:\n      path: /" + s + "\n"},
		{"proxy type whitelist", "proxies:\n  - name: p1\n    type: " + s + "\n    server: 1.2.3.4\n    port: 1\n"},
		{"proxy state-dir value", "proxies:\n  - name: p1\n    type: ss\n    server: 1.2.3.4\n    port: 1\n    state-dir: /tmp/" + s + "\n"},
		{"rules item", "rules:\n  - " + strings.Repeat(s+"|", 300) + "\n"},
		{"provider file with secret path", "proxy-providers:\n  prov:\n    type: file\n    path: /tmp/" + s + "\n"},
		{"provider unknown type", "proxy-providers:\n  prov:\n    type: " + s + "\n"},
		{"dns unknown key value", "dns:\n  listen: \"\"\n  mystery-key: " + s + "\n"},
		{"hosts bad value", "hosts:\n  a.example.invalid:\n    b: " + s + "\n"},
		{"syntax error line", "mode: [" + s + "\n"},
		{"second document", "mode: rule\n---\nmode: " + s + "\n"},
		{"anchor value", "a: &x " + s + "\n"},
	}
	for _, tc := range cases {
		rej := mustReject(t, tc.in)
		if strings.Contains(rej.Key, s) {
			t.Fatalf("%s: Key leaks sentinel: %q", tc.name, rej.Key)
		}
		if strings.Contains(rej.Reason, s) {
			t.Fatalf("%s: Reason leaks sentinel: %q", tc.name, rej.Reason)
		}
		if strings.Contains(rej.Error(), s) {
			t.Fatalf("%s: Error() leaks sentinel: %q", tc.name, rej.Error())
		}
	}

	// 键名回显例外：哨兵作为 provider 名称（映射键）出现——Key 允许包含它，
	// Reason 仍不得出现（注释即本测试的裁定记录）
	rej := mustReject(t, "proxy-providers:\n  "+s+":\n    type: file\n")
	if !strings.Contains(rej.Key, s) {
		t.Fatalf("provider name (mapping key) should be echoed in Key, got %q", rej.Key)
	}
	if strings.Contains(rej.Reason, s) {
		t.Fatalf("Reason leaks sentinel: %q", rej.Reason)
	}

	// 接受路径的产物包含值是设计使然（内核要用），此处只确认 Ignored 不进值：
	res := mustAccept(t, "proxy-providers:\n  prov:\n    type: http\n    url: https://u.example.invalid/x\n    path: /tmp/"+s+"\n")
	for _, ig := range res.Ignored {
		if strings.Contains(ig, s) {
			t.Fatalf("Ignored leaks path value: %q", ig)
		}
	}
}

// TestNormalizeSanitizeKeyEcho：键名回显截断到 64 字节；非可打印字符替换为 '?'。
// 不只是控制字符——双向覆盖符、零宽字符、行/段分隔符都不是控制字符，但都能让
// root 属主日志里的文本"看起来不是它实际的样子"（批⑥b I6）。
func TestNormalizeSanitizeKeyEcho(t *testing.T) {
	multibyte := strings.Repeat("节", 100) // 300 字节、100 字符
	rej := mustReject(t, "dns:\n  listen: \"\"\n  "+multibyte+": 1\n")
	want := strings.Repeat("节", 21) // 63 字节——第 22 个字符会超出 64
	if rej.Key != "dns."+want {
		t.Fatalf("Key = %q (%d bytes tail), want dns.%s", rej.Key, len(rej.Key)-4, want)
	}
	// 控制字符键名须写成带转义的双引号标量——裸控制字节 YAML 本身就解析不过
	ctrl := "bad\\x01key\\nname"
	rej = mustReject(t, "dns:\n  \""+ctrl+"\": 1\n")
	if strings.ContainsAny(rej.Key, "\x01\n") || !strings.Contains(rej.Key, "bad?key?name") {
		t.Fatalf("Key = %q, want control chars replaced: bad?key?name", rej.Key)
	}
	// 非控制类的"不可见"字符：双向覆盖（Cf）、零宽空格（Cf）、行/段分隔符（Zl/Zp）、
	// BOM（Cf）、不换行空格（Zs）。写成 \u 转义——裸的 U+2028/U+2029 会被 YAML
	// 扫描器当成换行
	hostile := "a\\u202Egnp.iru\\u200Bb\\u2028c\\u2029d\\uFEFFe\\u00A0f"
	rej = mustReject(t, "dns:\n  listen: \"\"\n  \""+hostile+"\": 1\n")
	if rej.Key != "dns.a?gnp.iru?b?c?d?e?f" {
		t.Fatalf("Key = %q, want every non-printable rune replaced", rej.Key)
	}
	for _, r := range rej.Key {
		if !isEchoable(r) && r != '.' {
			t.Fatalf("Key = %q still carries a non-printable rune %U", rej.Key, r)
		}
	}
	// 普通可打印字符（含中文、空格）原样保留
	rej = mustReject(t, "dns:\n  listen: \"\"\n  节点 one: 1\n")
	if rej.Key != "dns.节点 one" {
		t.Fatalf("Key = %q, want printable runes kept", rej.Key)
	}
}

// TestNormalizeRejectionKeyBounded：键路径整体封顶 256 字节。每段键名至多回显
// 64 字节，但深嵌套会把路径叠到约 8KB——错误文本进日志与错误响应，整条路径必须有界。
func TestNormalizeRejectionKeyBounded(t *testing.T) {
	// 6 层 × 65 字节即越过 256：挂在 proxies 项的自由字段下
	inner := "{leaf: 1}"
	for i := 0; i < 140; i++ { // 同时越过 128 层深度上限
		inner = "{" + strings.Repeat("k", 64) + ": " + inner + "}"
	}
	rej := mustReject(t, "proxies:\n  - name: n\n    type: ss\n    server: 1.2.3.4\n    port: 1\n    x: "+inner+"\n")
	if len(rej.Key) > keyPathMaxBytes {
		t.Fatalf("Key is %d bytes, want at most %d", len(rej.Key), keyPathMaxBytes)
	}
	if !utf8.ValidString(rej.Key) || !strings.HasSuffix(rej.Key, "…") {
		t.Fatalf("Key = %q, want valid UTF-8 ending with the truncation mark", rej.Key)
	}
	if len(rej.Error()) > keyPathMaxBytes+len(rej.Reason)+64 {
		t.Fatalf("Error() is %d bytes: %q", len(rej.Error()), rej.Error())
	}
	// 截断不切坏多字节字符：每段用 3 字节的汉字，边界必须落在字符之间。深度没超，
	// 在最内层放一个违规 tag 让拒绝发生在长路径的末端
	inner = "{leaf: !!binary aGk=}"
	for i := 0; i < 6; i++ {
		inner = "{" + strings.Repeat("节", 21) + ": " + inner + "}"
	}
	rej = mustReject(t, "proxies:\n  - name: n\n    type: ss\n    server: 1.2.3.4\n    port: 1\n    x: "+inner+"\n")
	if !utf8.ValidString(rej.Key) || strings.ContainsRune(rej.Key, utf8.RuneError) {
		t.Fatalf("Key = %q, truncation split a multi-byte rune", rej.Key)
	}
	// 短路径原样返回，不带截断标记
	rej = mustReject(t, "mode: empire\n")
	if rej.Key != "mode" {
		t.Fatalf("Key = %q, want the untouched short path", rej.Key)
	}
}
