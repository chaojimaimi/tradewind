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
	got, err := Synth(sub, testInjection())
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
	first, err := Synth(sub, testInjection())
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := Synth(sub, testInjection())
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
	got, err := Synth(sub, testInjection())
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
	got, err := Synth(sub, inj)
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
	if _, err := Synth([]byte("proxies: [broken"), testInjection()); err == nil {
		t.Fatal("invalid subscription yaml: want error")
	}
	if _, err := Synth([]byte("proxies: []"), Injection{Settings: store.Settings{MixedPort: 0}}); err == nil {
		t.Fatal("invalid settings: want error")
	}
}

func TestSynthSubscriptionWithoutRules(t *testing.T) {
	// 只有 proxies 的最小订阅也应能合成（rules 缺失不报错，由内核默认 DIRECT 兜底）
	sub := []byte("proxies:\n  - name: n1\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n")
	got, err := Synth(sub, testInjection())
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
	got, err := Synth(nil, testInjection())
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
	got, err := Synth(sub, inj)
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
