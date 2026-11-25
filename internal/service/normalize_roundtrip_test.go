// normalize_roundtrip_test.go：重新编码保真（批⑥b §4.2）。
//
// 归一化器校验的是"解析出的树"，内核拿到的是"重新编码后的字节"——两者必须同义，
// 否则白名单校验的对象与内核解码的对象可以不是同一个。本文件断言两件事：
//  1. 对每个被接受的输入，产物重新解析后的树 == 输入的树（剥掉归一化器有权改动的
//     仅有的两处：顶层钉死键、http provider 的 path），逐节点比较 Kind/Tag/Value。
//  2. 在校验 tag 的位置上，被校验为 !!str 的值编码后仍是 !!str 且文本不变——这是
//     唯一要紧的方向：字符串不得在重新编码后变成别的类型（端口位、枚举位、
//     出站 type/name、规则条目都靠 tag 把关）。
//
// 已知的一处不对称（非缺陷，c3a 审查时实测）：流式上下文里带时间的 !!timestamp
// 标量会被 yaml.v3 编码器加引号，重新解析为 !!str（文本不变）。方向是收紧；凡校验
// tag 的位置 !!timestamp 本来就过不了，自由位置（节点自定义字段）内核按文本读。
//
// WHY 单独成文件：这是 go-yaml 升级时最先坏的东西（编码器引号策略变了，这里先红）。
// 本文件只守服务侧。内核解码用的是另一个分支 go.yaml.in/yaml/v3 v3.0.5（本项目钉
// gopkg.in/yaml.v3 v3.0.1；c3a 审查时逐文件比对：resolve/scanner/parser/reader 语义
// 一致，decode 仅差合并键的 panic 恢复——归一化产物不含合并键）。内核或依赖升级时
// 须重做这次比对（风险表 R24）。
package service

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// roundTripFree 是自由字段的挂载点：proxies 项里 name/type 之外的字段不做值校验，
// 想塞什么构造都行。
const roundTripFree = "proxies:\n  - name: n\n    type: ss\n    server: 1.2.3.4\n    port: 1\n"

// roundTripCorpus 被接受刁钻构造全集。域名一律 .invalid，凭据为测试假值。
func roundTripCorpus() map[string]string {
	return map[string]string{
		// 各种"看起来像别的类型"的字符串值
		"quoted lookalikes as str values": `hosts: {"null": "null", "~": "~", "true": "True", "yes": "yes", "on": "on", "1": "01", "1.5": ".inf", "2020-01-01": "2020-01-01", "0x1F": "0x1F", "1e3": "1_000", "": ""}`,
		"explicit non-specific tag !":     "hosts: {a: ! 123, b: ! true, k: ! foo}\n",
		"tagged key":                      "!!str mode: rule\nhosts: {!!str a: 1, !!str \"\": 2}\n",
		// 多行与转义
		"multiline free strings":     roundTripFree + "    notes: \"line1\\nline2\"\n    l: |\n      a\n      b\n\n    f: >-\n      x\n      y\n    keep: |+\n      k\n\n    ind: \"  spaced  \"\n    tab: \"a\\tb\"\n",
		"free strings with specials": roundTripFree + "    hash: \"a #b\"\n    colon: \"a: b\"\n    dash: \"- a\"\n    quote: \"it's \\\"quoted\\\"\"\n    back: \"C:\\\\path\\\\n\"\n    uni: 中文 émoji 😀\n    nul: \"\\0\"\n    ctrl: \"\\x01\\x1f\\x7f\"\n    cr: \"a\\r\\nb\"\n    bs: \"a\\bb\"\n    esc: \"\\e\\a\\v\\f\"\n    nel: \"a\\u0085b\"\n    ls: \"a\\u2028b\"\n    high: \"\\U0001F600\\u00e9\\u0301\\uFEFF\"\n    slash: \"a\\\\\"\n    quote_end: \"abc\\\"\"\n    empty: \"\"\n    tilde: \"~\"\n    amp: \"&a *b\"\n    flow: \"[a]{b},c\"\n    question: \"? a : b\"\n    pct: \"%YAML 1.1\"\n    at: \"@a `b\"\n    pipe: \"| >\"\n    longdash: \"--- a ...\"\n",
		// 键名：映射键只能是 !!str，但键名文本可以是任何东西
		"tricky keys":               "hosts: {\"a b\": 1, \"a: b\": 1, \"a#b\": 1, \"- a\": 1, \"? a\": 1, \"@a\": 1, \"`a\": 1, \"[a]\": 1, \"{a}\": 1, \"a,b\": 1, \"*a\": 1, \"&a\": 1, \"!a\": 1, \"%a\": 1, \"\": 1, \" leading\": 1, \"trailing \": 1, \"multi\\nline\": 1, \"tab\\there\": 1, \"'single'\": 1, \"\\\"double\\\"\": 1, \"中文\": 1, \"\\u2028\": 1, \"\\u2029\": 1, \"\\u0085\": 1, \"\\0\": 1, \"\\x7f\": 1, \"a\\u0085b\": 1, \"a:  b\": 1, \"#c\": 1, \"a #c\": 1, \"|\": 1, \"-\": 1, \"?\": 1, \":\": 1, \"...\": 1, \"---\": 1, \"a\\t\": 1, \"\\uFEFF\": 1, \"\\u200B\": 1, \"\\u202E\": 1}\n",
		"rules with tricky strings": "rules:\n  - MATCH,DIRECT\n  - 'DOMAIN,a#b,b'\n  - \"a\\tb\"\n  - \"x\\ny\"\n  - \"\"\n  - \"null\"\n  - \"2020-01-01\"\n",
		"flow nesting":              "proxies: [{name: n, type: ss, server: 1.2.3.4, port: 1, x: {y: [1, {z: \"q\"}, [true, null]]}, alpn: [h2, http/1.1]}]\n",
		"scalar types free":         roundTripFree + "    a: [1, -1, +1, 0x1F, 0o17, 017, 0b101, 1_000, 1.5, .inf, -.inf, .nan, 1e3, 1_0.5, true, false, True, FALSE, on, off, yes, no, ~, null, Null, NULL, 2020-01-01, 2001-12-14t21:59:43.10-05:00, 2001-12-14 21:59:43.10 -5, 12:30, \"\"]\n",
		"int forms at int keys":     "mixed-port: 0x1F\nrouting-mark: 0o17\nkeep-alive-idle: 1_000\nkeep-alive-interval: +5\nport: 65535\n",
		"enum quoted":               "mode: \"rule\"\nlog-level: 'info'\nfind-process-mode: \"off\"\ngeodata-loader: 'memconservative'\n",
		"str keys quoted":           "bind-address: \"*\"\ninterface-name: 'en0'\nsecret: ''\nglobal-ua: \"Clash\\nv1\"\nglobal-client-fingerprint: \"random\"\n",
		"seq of str quoted":         "authentication: [\"user:pass\", 'a b', \"\", \"null\", \"#x\", \"a,b\", \"[x]\", \"- x\"]\nlan-allowed-ips: ['::1/128', \"0.0.0.0/0\"]\n",
		// 注释：随节点走，不得改变结构；钉死键值节点上的注释随节点一起被替换掉
		"comments everywhere":     "# head of doc\n# second head line\nmode: rule # line comment\nallow-lan: true\n# head of dns\ndns:\n  enable: true # inline\n  # comment between\n  listen: \"\"\n  nameserver:\n    # comment in seq\n    - 223.5.5.5 # trailing\n    - 1.1.1.1\n  # foot of nameserver\nhosts:\n  a: 1 # c1\n  b: 2\n# standalone with: colon and - dash and \"quotes\"\n# --- looks like doc start\n# ...\n#\t tab indented comment\nprofile:\n  store-selected: true\n# tail comment without newline",
		"comment on pinned value": "tun: # user comment on tun\n  enable: false\nmode: rule\nexternal-controller: 1.2.3.4:9090 # ec comment\nhosts: {a: 1}\n",
		// 文档级形态
		"doc markers":               "---\nmode: rule\nallow-lan: true\n...\n",
		"doc markers with comments": "# before\n--- # after start\nmode: rule\n... # after end\n",
		// %TAG 把 !! 指向别处：普通标量仍解析为核心 tag，接受；产物不得携带指令
		"tag directive redefining !!": "%TAG !! tag:example.com,2000:app/\n---\nmode: rule\nhosts: {a: 1, b: x}\n",
		"empty mapping":               "{}\n",
		"empty flow spaced":           "{ }\n",
		"only pinned keys":            "tun: x\nexternal-controller: y\n",
		"crlf":                        "mode: rule\r\nallow-lan: true\r\nhosts:\r\n  a: 1\r\n",
		"bom":                         "\xef\xbb\xbfmode: rule\n",
		"no trailing newline":         "mode: rule",
		"indent 4":                    "dns:\n    enable: true\n    nameserver:\n        - 223.5.5.5\n    listen: \"\"\n",
		"null values everywhere":      "hosts:\n  a:\nproxies: []\nrules: []\ndns:\n  enable:\n",
		"ec empty forms":              "external-controller:\n",
		"ec explicit null":            "external-controller: null\n",
		"ec quoted empty":             "external-controller: ''\n",
		"deep ok":                     deepUnderProxies(128),
		// provider：path 剥离连同其注释；inline 的 path 不剥离（内核不读）
		"provider path with comments": "proxy-providers:\n  p:\n    type: http\n    url: \"https://x.invalid/a\" # url comment\n    path: /tmp/x # path comment\n    interval: 3600\nrule-providers:\n  q:\n    type: http\n    url: https://x.invalid/b\n    path: ./q.yaml\n",
		"inline provider keeps path":  "rule-providers:\n  q:\n    type: inline\n    behavior: classical\n    payload: [MATCH,DIRECT]\n    path: ./unused.yaml\n",
		"unicode escapes keys":        "hosts: {\"\\u4e2d\": \"\\u00e9\\u0301\", \"e\\u0301\": x, \"\\u00e9\": y}\n",
		"hosts values typed":          "hosts: {a: 1, b: 1.5, c: true, d: ~, e: [], f: [1, two, ~, .inf], g: 2020-01-01}\n",
		"all group types":             "proxy-groups: [{name: a, type: select}, {name: b, type: url-test}, {name: c, type: fallback}, {name: d, type: load-balance}, {name: e, type: relay}]\n",
		"long string":                 "hosts: {a: \"" + strings.Repeat("0123456789abcdef", 4096) + "\"}\n",
	}
}

// stripNormalizedAway 去掉归一化器有权改动的两处：顶层钉死键（值被整体替换）与
// http provider 的 path（被剥离）。两侧树都做同样的剥离，剩下的部分必须同义。
func stripNormalizedAway(root *yaml.Node) {
	var keep []*yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if pinnedKeys[k.Value] {
			continue
		}
		if v.Kind == yaml.MappingNode && (k.Value == "proxy-providers" || k.Value == "rule-providers") {
			for j := 1; j < len(v.Content); j += 2 {
				if typ, ok := mappingGet(v.Content[j], "type"); ok && typ.Value == "http" {
					removePair(v.Content[j], "path")
				}
			}
		}
		keep = append(keep, k, v)
	}
	root.Content = keep
}

// assertSameShape 逐节点比较（忽略样式/注释/行列）。唯一允许的差异是文件头说明的
// !!timestamp → !!str；反方向（!!str 变成别的类型）一律失败。
func assertSameShape(t *testing.T, want, got *yaml.Node, path string) {
	t.Helper()
	if want.Kind != got.Kind {
		t.Fatalf("%s: kind %v became %v", path, want.Kind, got.Kind)
	}
	if want.Kind == yaml.ScalarNode {
		sameTag := want.Tag == got.Tag || (want.Tag == "!!timestamp" && got.Tag == "!!str")
		if want.Value != got.Value || !sameTag {
			t.Fatalf("%s: scalar %s(%q) became %s(%q)", path, want.Tag, want.Value, got.Tag, got.Value)
		}
	}
	if len(want.Content) != len(got.Content) {
		t.Fatalf("%s: %d children became %d", path, len(want.Content), len(got.Content))
	}
	for i := range want.Content {
		assertSameShape(t, want.Content[i], got.Content[i], path+"/"+childName(want, i))
	}
}

// childName 给第 i 个子节点取一个只含键名/下标的路径片段（测试内比较用，输入全是
// 测试样例，不存在泄漏问题）。
func childName(parent *yaml.Node, i int) string {
	if parent.Kind == yaml.SequenceNode {
		return "[" + itoaTest(i) + "]"
	}
	if i%2 == 1 {
		return parent.Content[i-1].Value
	}
	return "<key>"
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// TestNormalizeRoundTripPreservesTree 每个被接受的输入：产物重新解析后与输入的树同义。
func TestNormalizeRoundTripPreservesTree(t *testing.T) {
	for name, in := range roundTripCorpus() {
		inb := []byte(in)
		res, rej := NormalizeRuntime(inb)
		if rej != nil {
			t.Fatalf("%s: want accept, got rejection at %s: %s", name, rej.Key, rej.Reason)
		}
		want := parseTestTree(t, inb)
		got := parseTestTree(t, res.Bytes)
		stripNormalizedAway(want)
		stripNormalizedAway(got)
		assertSameShape(t, want, got, name)
	}
}

// TestNormalizeOutputCarriesNoAliasOrAnchor 产物里不得残留别名/锚点——输入里有
// 会被解析纪律拒绝；这里守的是"编码器不会自己造出锚点"（节点 Anchor 字段非空时
// yaml.v3 会输出 &anchor / *alias）。
func TestNormalizeOutputCarriesNoAliasOrAnchor(t *testing.T) {
	for name, in := range roundTripCorpus() {
		res, _ := NormalizeRuntime([]byte(in))
		var count int
		var walk func(*yaml.Node)
		walk = func(n *yaml.Node) {
			if n.Kind == yaml.AliasNode || n.Anchor != "" {
				count++
			}
			for _, c := range n.Content {
				walk(c)
			}
		}
		walk(parseTestTree(t, res.Bytes))
		if count != 0 {
			t.Fatalf("%s: output carries %d alias/anchor nodes", name, count)
		}
	}
}

// TestNormalizeStrPositionsStayStr：被校验为 !!str 的值编码后仍是 !!str 且文本不变。
// 样例全是"裸写会被解析成别的类型"的字符串——编码器一旦漏加引号，它们就会以
// !!int/!!bool/!!null/!!timestamp 的身份到达内核，而白名单放行的是字符串。
func TestNormalizeStrPositionsStayStr(t *testing.T) {
	lookalikes := []string{
		`"2020-01-01"`, `"2001-12-14t21:59:43.10-05:00"`, `"null"`, `"~"`, `"true"`, `"yes"`, `"on"`,
		`"1"`, `"0x1F"`, `"1.5"`, `".inf"`, `".nan"`, `"12:30"`, `"1_000"`, `!!str 2020-01-01`,
		`!!str null`, `!!str 1`, `'2020-01-01'`, `"01"`, `"017"`, `"1e3"`, `"+1"`, `"-1"`,
		`"a\nb"`, `"中文"`, `" "`,
	}
	for _, lk := range lookalikes {
		want := scalarInTestDoc(t, lk)
		if want == nil || want.Tag != "!!str" {
			t.Fatalf("fixture %s is not a !!str scalar (tag %s); fix the test input", lk, tagOfTest(want))
		}
		in := "proxies:\n  - name: " + lk + "\n    type: ss\n    server: 1.2.3.4\n    port: 1\n" +
			"proxy-groups:\n  - name: " + lk + "\n    type: select\n" +
			"rules:\n  - " + lk + "\nauthentication:\n  - " + lk + "\nbind-address: " + lk + "\n"
		res, rej := NormalizeRuntime([]byte(in))
		if rej != nil {
			t.Fatalf("%s: want accept, got rejection at %s: %s", lk, rej.Key, rej.Reason)
		}
		root := parseTestTree(t, res.Bytes)
		px, _ := mappingGet(root, "proxies")
		pg, _ := mappingGet(root, "proxy-groups")
		ru, _ := mappingGet(root, "rules")
		au, _ := mappingGet(root, "authentication")
		for _, pos := range []struct {
			name string
			node *yaml.Node
		}{
			{"proxies[0].name", mustGetTest(t, px.Content[0], "name")},
			{"proxy-groups[0].name", mustGetTest(t, pg.Content[0], "name")},
			{"rules[0]", ru.Content[0]},
			{"authentication[0]", au.Content[0]},
			{"bind-address", mustGetTest(t, root, "bind-address")},
		} {
			if pos.node.Tag != "!!str" || pos.node.Value != want.Value {
				t.Fatalf("%s at %s: became %s(%q), want !!str(%q)", lk, pos.name, pos.node.Tag, pos.node.Value, want.Value)
			}
		}
	}
}

// TestNormalizeIdempotentCorpus：roundTripCorpus 全体幂等（逐字节相同、二次 Ignored 为空）。
func TestNormalizeIdempotentCorpus(t *testing.T) {
	for name, in := range roundTripCorpus() {
		first, rej := NormalizeRuntime([]byte(in))
		if rej != nil {
			t.Fatalf("%s: want accept, got rejection at %s: %s", name, rej.Key, rej.Reason)
		}
		second, rej := NormalizeRuntime(first.Bytes)
		if rej != nil {
			t.Fatalf("%s: second pass rejected at %s: %s", name, rej.Key, rej.Reason)
		}
		if string(first.Bytes) != string(second.Bytes) || len(second.Ignored) != 0 {
			t.Fatalf("%s: not a fixed point (second Ignored = %v)", name, second.Ignored)
		}
	}
}

func parseTestTree(t *testing.T, in []byte) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		t.Fatalf("parse test yaml: %v", err)
	}
	return doc.Content[0]
}

func mustGetTest(t *testing.T, m *yaml.Node, key string) *yaml.Node {
	t.Helper()
	n, ok := mappingGet(m, key)
	if !ok {
		t.Fatalf("key %s missing from test output", key)
	}
	return n
}

func scalarInTestDoc(t *testing.T, scalar string) *yaml.Node {
	t.Helper()
	return parseTestTree(t, []byte("k: "+scalar+"\n")).Content[1]
}

func tagOfTest(n *yaml.Node) string {
	if n == nil {
		return "<nil>"
	}
	return n.Tag
}
