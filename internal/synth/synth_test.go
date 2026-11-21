package synth

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/store"
	"gopkg.in/yaml.v3"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite golden files")

func testInjection() Injection {
	// TunEnabled 缺省 false：TestSynthGolden 即"TUN off = 既有 golden 逐字节"的
	// 零漂移锚（批⑥ §6.1——改行为必须显式更新 golden）
	return Injection{
		Settings: store.Settings{
			MixedPort: 17890, GatewayPort: store.DefaultGatewayPort, PanelEnabled: true,
			Mode: "rule", LogLevel: "info",
		},
		Secret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func TestSynthGolden(t *testing.T) {
	// golden-file：改行为必须显式更新 golden（M0-plan §6）
	sub, err := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	if err != nil {
		t.Fatalf("read subscription fixture: %v", err)
	}
	got, err := Synth(sub, testInjection(), nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	golden := filepath.Join("testdata", "golden", "runtime.yaml")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("synth output differs from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSynthIsDeterministic(t *testing.T) {
	sub, _ := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	first, err := Synth(sub, testInjection(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := Synth(sub, testInjection(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("synth output not deterministic")
		}
	}
}

func TestSynthDropsSubscriptionInfraKeys(t *testing.T) {
	// 白名单语义核心断言：订阅自带的危险键不得进入 runtime
	sub := []byte(`mixed-port: 9999
external-ui: /tmp/evil
tun:
  enable: true
dns:
  enable: false
proxies:
  - name: n1
    type: socks5
    server: 127.0.0.1
    port: 1080
rules:
  - MATCH,DIRECT
`)
	got, err := Synth(sub, testInjection(), nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"external-ui", "tun"} {
		if _, present := out[key]; present {
			t.Fatalf("subscription key %q must be dropped", key)
		}
	}
	if out["mixed-port"] != 17890 {
		t.Fatalf("mixed-port = %v, want settings to override subscription", out["mixed-port"])
	}
	if out["secret"] != testInjection().Secret {
		t.Fatalf("secret = %v, want injected", out["secret"])
	}
	if out["external-controller"] != "" {
		t.Fatalf("external-controller = %v, want empty string (HTTP ctl off)", out["external-controller"])
	}
	dns, _ := out["dns"].(map[string]any)
	if dns == nil || dns["enable"] != true {
		t.Fatalf("dns = %v, want template dns preserved (subscription dns dropped)", out["dns"])
	}
	// M1 前置依赖：profile.store-selected 必须进 runtime（mihomo 默认 false，缺失则重启丢选择）
	profile, _ := out["profile"].(map[string]any)
	if profile == nil || profile["store-selected"] != true {
		t.Fatalf("profile = %v, want store-selected true from template", out["profile"])
	}
}

func TestSynthSettingsOverrideTemplate(t *testing.T) {
	sub, _ := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	inj := testInjection()
	inj.Settings = store.Settings{MixedPort: 18888, GatewayPort: store.DefaultGatewayPort, Mode: "global", LogLevel: "debug"}
	got, err := Synth(sub, inj, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"mixed-port: 18888", "mode: global", "log-level: debug"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestSynthInvalidSubscription(t *testing.T) {
	if _, err := Synth([]byte("proxies: [broken"), testInjection(), nil); err == nil {
		t.Fatal("invalid subscription yaml: want error")
	}
	if _, err := Synth([]byte("proxies: []"), Injection{Settings: store.Settings{MixedPort: 0}}, nil); err == nil {
		t.Fatal("invalid settings: want error")
	}
}

func TestSynthSubscriptionWithoutRules(t *testing.T) {
	// 只有 proxies 的最小订阅也应能合成（rules 缺失不报错，由内核默认 DIRECT 兜底）
	sub := []byte("proxies:\n  - name: n1\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n")
	got, err := Synth(sub, testInjection(), nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	if !strings.Contains(string(got), "proxies:") {
		t.Fatalf("output missing proxies:\n%s", got)
	}
}

func TestSynthEmptySubscriptionIsBareTemplate(t *testing.T) {
	// 裸内核场景（无 current 订阅）：Synthesize 注释声明的"纯模板"路径。
	// 回归背景：空订阅此前因 parseMapping(空) 报 "top level is 0"，从未被覆盖。
	got, err := Synth(nil, testInjection(), nil)
	if err != nil {
		t.Fatalf("Synth(nil): %v", err)
	}
	text := string(got)
	for _, want := range []string{"mixed-port: 17890", "mode: rule"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bare template missing %q:\n%s", want, text)
		}
	}
	for _, banned := range []string{"proxies:", "proxy-groups:", "rules:"} {
		if strings.Contains(text, banned) {
			t.Fatalf("bare template must not contain %q:\n%s", banned, text)
		}
	}
}

func TestSynthInjectsAllowLan(t *testing.T) {
	// allow_lan 全链（M3-plan §11.2-6）：模板静态占位 false，settings.allow_lan=true 注入覆盖
	sub, _ := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	inj := testInjection()
	inj.Settings.AllowLan = true
	got, err := Synth(sub, inj, nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if out["allow-lan"] != true {
		t.Fatalf("allow-lan = %v, want true injected from settings", out["allow-lan"])
	}
}

// --- TUN/DNS 合成（批⑥ §3.2）---

func TestSynthGoldenTunOn(t *testing.T) {
	// tun on 新 golden（与既有 golden 双版并存：off=零漂移锚、on=注入块逐字节锚）
	sub, err := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	if err != nil {
		t.Fatalf("read subscription fixture: %v", err)
	}
	inj := testInjection()
	inj.Settings.TunEnabled = true
	got, err := Synth(sub, inj, nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	golden := filepath.Join("testdata", "golden", "runtime-tun.yaml")
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("tun-on output differs from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSynthTunOffOmitsBlocks(t *testing.T) {
	// tun off：零注入——模板 dns 段（redir-host）原样保留、无 tun 键
	sub, _ := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	got, err := Synth(sub, testInjection(), nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["tun"]; present {
		t.Fatal("tun_enabled=false must not inject tun block")
	}
	dns, _ := out["dns"].(map[string]any)
	if dns == nil || dns["enhanced-mode"] != "redir-host" {
		t.Fatalf("dns = %v, want template redir-host preserved", out["dns"])
	}
}

func TestSynthTunOnInjectsWhitelistAndTunBlock(t *testing.T) {
	// 注入块键值逐项断言 + 黑名单逐条断言（批⑥ §6.1）
	sub, _ := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	inj := testInjection()
	inj.Settings.TunEnabled = true
	got, err := Synth(sub, inj, nil)
	if err != nil {
		t.Fatalf("Synth: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if n := len(fakeIPFilter); n < 25 || n > 30 {
		t.Fatalf("fake-ip-filter has %d entries, want 25-30", n)
	}
	tun, _ := out["tun"].(map[string]any)
	if tun == nil {
		t.Fatal("tun block missing")
	}
	for k, want := range map[string]any{"enable": true, "stack": "system", "auto-route": true, "auto-detect-interface": true} {
		if tun[k] != want {
			t.Fatalf("tun.%s = %v, want %v", k, tun[k], want)
		}
	}
	if hijack, _ := tun["dns-hijack"].([]any); len(hijack) != 1 || hijack[0] != "any:53" {
		t.Fatalf("tun.dns-hijack = %v, want [any:53]", tun["dns-hijack"])
	}
	dns, _ := out["dns"].(map[string]any)
	if dns == nil {
		t.Fatal("dns block missing")
	}
	for k, want := range map[string]any{
		"enable": true, "ipv6": false, "enhanced-mode": "fake-ip", "fake-ip-range": "198.18.0.1/16",
	} {
		if dns[k] != want {
			t.Fatalf("dns.%s = %v, want %v", k, dns[k], want)
		}
	}
	// 黑名单逐条：输出与常量精确一致（含序），且每条为纯域名通配（无 geosite:/URI scheme）
	filter, _ := dns["fake-ip-filter"].([]any)
	if len(filter) != len(fakeIPFilter) {
		t.Fatalf("fake-ip-filter len = %d, want %d", len(filter), len(fakeIPFilter))
	}
	for i, item := range filter {
		s, _ := item.(string)
		if s != fakeIPFilter[i] {
			t.Fatalf("fake-ip-filter[%d] = %q, want %q", i, s, fakeIPFilter[i])
		}
		if strings.Contains(s, "geosite:") || strings.Contains(s, "://") {
			t.Fatalf("fake-ip-filter[%d] = %q must be pure domain (no geosite:/scheme)", i, s)
		}
	}
	if ns, _ := dns["default-nameserver"].([]any); len(ns) != 2 || ns[0] != "223.5.5.5" {
		t.Fatalf("dns.default-nameserver = %v, want aligned with template F5", dns["default-nameserver"])
	}
	if psn, _ := dns["proxy-server-nameserver"].([]any); len(psn) != 1 || psn[0] != "tls://223.5.5.5:853" {
		t.Fatalf("dns.proxy-server-nameserver = %v, want tls://223.5.5.5:853", dns["proxy-server-nameserver"])
	}
	ff, _ := dns["fallback-filter"].(map[string]any)
	if ff == nil || ff["geoip"] != true || ff["geoip-code"] != "CN" {
		t.Fatalf("dns.fallback-filter = %v, want geoip CN", dns["fallback-filter"])
	}
}

func TestSynthTunOnOverridesEnhanceMerge(t *testing.T) {
	// 增强 merge 槽写 dns:/tun: 键被注入段覆盖（R7："settings 覆盖一切"链末语义）
	sub, _ := os.ReadFile(filepath.Join("testdata", "subscription.yaml"))
	inj := testInjection()
	inj.Settings.TunEnabled = true
	merge := []byte("dns:\n  enhanced-mode: redir-host\ntun:\n  enable: false\n")
	got, err := Synth(sub, inj, &Enhance{GlobalMerge: merge})
	if err != nil {
		t.Fatalf("Synth with merge: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	dns, _ := out["dns"].(map[string]any)
	if dns == nil || dns["enhanced-mode"] != "fake-ip" {
		t.Fatalf("dns.enhanced-mode = %v, want injected fake-ip to override merge", dns)
	}
	tun, _ := out["tun"].(map[string]any)
	if tun == nil || tun["enable"] != true {
		t.Fatalf("tun.enable = %v, want injected true to override merge", tun)
	}
}
