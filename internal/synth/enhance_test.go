package synth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// enhanceInjection 与 testInjection 一致（告警收集槽可注入）。
func enhanceInjection(warn *[]string) Injection {
	inj := testInjection()
	inj.Warnings = warn
	return inj
}

// decodeTop 把合成输出解码为顶层映射（便于断言键级语义）。
func decodeTop(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out)
	}
	return m
}

// rulesList 取输出 rules 序列（字符串列表形态）。
func rulesList(t *testing.T, out []byte) []any {
	t.Helper()
	rules := decodeTop(t, out)["rules"]
	seq, ok := rules.([]any)
	if !ok {
		t.Fatalf("rules = %#v, want sequence\n%s", rules, out)
	}
	return seq
}

func asStringList(t *testing.T, v any) []string {
	t.Helper()
	seq, ok := v.([]any)
	if !ok {
		t.Fatalf("%#v, want sequence", v)
	}
	out := make([]string, 0, len(seq))
	for _, item := range seq {
		out = append(out, item.(string))
	}
	return out
}

// TestSynthEnhanceEmptyByteIdenticalToBaseline 金标准锚（batch3 §6）：增强链全空
// （nil 与空槽两形态）时输出与现版本逐字节一致。
func TestSynthEnhanceEmptyByteIdenticalToBaseline(t *testing.T) {
	sub, err := osReadSubscription()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := Synth(sub, testInjection(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, enh := range map[string]*Enhance{
		"nil":      nil,
		"empty":    {},
		"comment":  {GlobalMerge: []byte("# 只有注释\n"), Rules: []byte("\n"), Proxies: []byte("# c\n"), Groups: []byte("# c\n")},
		"emptysub": {Rules: []byte(""), Proxies: []byte(""), Groups: []byte(""), GlobalMerge: []byte("")},
	} {
		got, err := Synth(sub, testInjection(), enh)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(baseline, got) {
			t.Fatalf("%s: output drifts from baseline (golden anchor broken)", name)
		}
	}
}

func TestEnhanceRulesPrependAndAppendOrder(t *testing.T) {
	// 相对次序钉死（评审 P3）：merge.rules-prepend 在最前（高于三槽 prepend），
	// 其次三槽 rules，再订阅 rules，merge.rules-append 在最后
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\nrules:\n  - MATCH,DIRECT\n")
	enh := &Enhance{
		Rules:       []byte("- DOMAIN-SUFFIX,slot.example,DIRECT\n"),
		GlobalMerge: []byte("rules-prepend:\n  - DOMAIN-SUFFIX,pre.example,DIRECT\nrules-append:\n  - DOMAIN-SUFFIX,app.example,DIRECT\n"),
	}
	out, err := Synth(sub, testInjection(), enh)
	if err != nil {
		t.Fatal(err)
	}
	got := asStringList(t, rulesList(t, out))
	want := []string{
		"DOMAIN-SUFFIX,pre.example,DIRECT",
		"DOMAIN-SUFFIX,slot.example,DIRECT",
		"MATCH,DIRECT",
		"DOMAIN-SUFFIX,app.example,DIRECT",
	}
	if len(got) != len(want) {
		t.Fatalf("rules = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rules[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestEnhanceProxiesGroupsAppend(t *testing.T) {
	// proxies/groups=append；订阅缺该键=仅槽内容
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\n")
	enh := &Enhance{
		Proxies: []byte("- {name: slot-p, type: socks5, server: 10.0.0.1, port: 1080}\n"),
		Groups:  []byte("- {name: slot-g, type: select, proxies: [n1]}\n"),
	}
	out, err := Synth(sub, testInjection(), enh)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTop(t, out)
	proxies := m["proxies"].([]any)
	if len(proxies) != 2 || proxies[0].(map[string]any)["name"] != "n1" || proxies[1].(map[string]any)["name"] != "slot-p" {
		t.Fatalf("proxies order wrong: %#v", proxies)
	}
	groups := m["proxy-groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["name"] != "slot-g" {
		t.Fatalf("groups = %#v, want slot-g only (subscription lacks groups)", groups)
	}
}

func TestEnhancePatchGroupsByName(t *testing.T) {
	// proxy-groups-patch 按 name 项级合并（Auto 组 interval 修改的解锁通道）
	sub := []byte(`proxies:
  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}
proxy-groups:
  - {name: Auto, type: url-test, proxies: [n1], interval: 300}
  - {name: 手选, type: select, proxies: [Auto]}
`)
	enh := &Enhance{
		GlobalMerge: []byte("proxy-groups-patch:\n  - {name: Auto, interval: 60}\n  - {name: 不存在的组, interval: 1}\n"),
	}
	var warns []string
	out, err := Synth(sub, enhanceInjection(&warns), enh)
	if err != nil {
		t.Fatal(err)
	}
	groups := decodeTop(t, out)["proxy-groups"].([]any)
	auto := groups[0].(map[string]any)
	if auto["interval"] != json.Number("60") && auto["interval"] != 60 && auto["interval"] != uint64(60) {
		t.Fatalf("Auto.interval = %#v, want 60", auto["interval"])
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "不存在的组") {
		t.Fatalf("warnings = %v, want miss warning", warns)
	}
}

func TestEnhanceDeepOverrideAndListReplace(t *testing.T) {
	// 其余任意键=深覆盖：map+map 递归（dns.enable 覆盖、nameserver 整体替换）
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\n")
	enh := &Enhance{
		GlobalMerge: []byte("dns:\n  ipv6: true\n  nameserver: [8.8.8.8]\n"),
	}
	out, err := Synth(sub, testInjection(), enh)
	if err != nil {
		t.Fatal(err)
	}
	dns := decodeTop(t, out)["dns"].(map[string]any)
	if dns["enable"] != true {
		t.Fatal("template dns.enable must survive deep merge")
	}
	if dns["ipv6"] != true {
		t.Fatalf("dns.ipv6 = %v, want true (scalar override)", dns["ipv6"])
	}
	ns := dns["nameserver"].([]any)
	if len(ns) != 1 || ns[0] != "8.8.8.8" {
		t.Fatalf("nameserver = %v, want list replace to [8.8.8.8]", ns)
	}
}

func TestEnhanceCannotReopenExternalController(t *testing.T) {
	// external-controller 注入在 merge 之后执行——任何增强层都无法重开 HTTP 控制器（P0 #7）
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\n")
	enh := &Enhance{GlobalMerge: []byte("external-controller: 0.0.0.0:9090\n")}
	out, err := Synth(sub, testInjection(), enh)
	if err != nil {
		t.Fatal(err)
	}
	if v := decodeTop(t, out)["external-controller"]; v != "" {
		t.Fatalf("external-controller = %v, want empty (enhance must not reopen)", v)
	}
}

func TestEnhanceSettingsStillHighestPriority(t *testing.T) {
	// settings 注入仍在链末最高优先（merge 覆盖 mixed-port 也被 settings 压回）
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\n")
	enh := &Enhance{GlobalMerge: []byte("mixed-port: 9999\nmode: global\n")}
	inj := testInjection()
	out, err := Synth(sub, inj, enh)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTop(t, out)
	if m["mixed-port"] != 17890 {
		t.Fatalf("mixed-port = %v, want settings to win", m["mixed-port"])
	}
	if m["mode"] != "rule" {
		t.Fatalf("mode = %v, want settings to win", m["mode"])
	}
}

func TestEnhanceProxiesAppendNameCollisionSkipped(t *testing.T) {
	// proxies-append 防重名：撞名跳过 + 告警（不静默）
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\n")
	enh := &Enhance{GlobalMerge: []byte("proxies-append:\n  - {name: n1, type: socks5, server: 10.9.9.9, port: 1}\n  - {name: n2, type: socks5, server: 10.0.0.2, port: 2}\n")}
	var warns []string
	out, err := Synth(sub, enhanceInjection(&warns), enh)
	if err != nil {
		t.Fatal(err)
	}
	proxies := decodeTop(t, out)["proxies"].([]any)
	if len(proxies) != 2 || proxies[1].(map[string]any)["name"] != "n2" {
		t.Fatalf("proxies = %#v, want n1 + n2 (n1 duplicate skipped)", proxies)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "n1") {
		t.Fatalf("warnings = %v", warns)
	}
}

func TestEnhanceInvalidSlotShapeErrors(t *testing.T) {
	sub := []byte("rules:\n  - MATCH,DIRECT\n")
	// 三槽必须顶层序列
	bad := &Enhance{Rules: []byte("rules-prepend: []\n")}
	if _, err := Synth(sub, testInjection(), bad); err == nil {
		t.Fatal("mapping-shaped rules slot: want error")
	}
	// merge 必须顶层映射
	badMerge := &Enhance{GlobalMerge: []byte("- a\n")}
	if _, err := Synth(sub, testInjection(), badMerge); err == nil {
		t.Fatal("sequence-shaped merge.yaml: want error")
	}
	// 指令键必须序列
	badDirective := &Enhance{GlobalMerge: []byte("rules-prepend: 3\n")}
	if _, err := Synth(sub, testInjection(), badDirective); err == nil {
		t.Fatal("scalar rules-prepend: want error")
	}
	// patch 项缺 name
	badPatch := &Enhance{GlobalMerge: []byte("proxy-groups-patch:\n  - interval: 5\n")}
	if _, err := Synth(sub, testInjection(), badPatch); err == nil {
		t.Fatal("patch item without name: want error")
	}
}

func TestEnhanceSlotParseYieldsDeterministicKeyOrder(t *testing.T) {
	// 键序稳定（golden 前提）：同输入两次合成逐字节一致
	sub := []byte("proxies:\n  - {name: n1, type: socks5, server: 127.0.0.1, port: 1080}\n")
	enh := &Enhance{
		Rules:       []byte("- DOMAIN,a,DIRECT\n"),
		GlobalMerge: []byte("rules-prepend:\n  - DOMAIN,b,DIRECT\n"),
	}
	first, err := Synth(sub, testInjection(), enh)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := Synth(sub, testInjection(), enh)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("enhance synth not deterministic")
		}
	}
}

func TestEnhanceWarningsSinkNilSafe(t *testing.T) {
	// Warnings 为 nil（存量 5 处调用点形态）时 patch 未命中不 panic
	sub := []byte("proxy-groups:\n  - {name: Auto, type: url-test, proxies: [x]}\n")
	enh := &Enhance{GlobalMerge: []byte("proxy-groups-patch:\n  - {name: miss, interval: 1}\n")}
	if _, err := Synth(sub, testInjection(), enh); err != nil {
		t.Fatal(err)
	}
}

// osReadSubscription 读取金标准输入订阅（synth_test.go 同源 fixture）。
func osReadSubscription() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
}
