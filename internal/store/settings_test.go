package store

import (
	"os"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestLoadSettingsDefaults(t *testing.T) {
	home := paths.Home(t.TempDir())
	s, err := LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	want := DefaultSettings()
	if s != want {
		t.Fatalf("default settings = %+v, want %+v", s, want)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	home := paths.Home(t.TempDir())
	s := Settings{MixedPort: 17897, GatewayPort: 19097, PanelEnabled: true, AllowLan: true, Mode: "global", LogLevel: "debug"}
	if err := SaveSettings(home, s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	// 缺省的 log_retention_days 在加载时归一为默认 14（M2-plan §2.4）
	s.LogRetentionDays = DefaultLogRetentionDays
	if got != s {
		t.Fatalf("round trip got %+v, want %+v", got, s)
	}
}

func TestSettingsValidate(t *testing.T) {
	bad := []Settings{
		{MixedPort: 0, GatewayPort: 39097, Mode: "rule", LogLevel: "info"},
		{MixedPort: 70000, GatewayPort: 39097, Mode: "rule", LogLevel: "info"},
		{MixedPort: 7897, GatewayPort: 0, Mode: "rule", LogLevel: "info"},
		{MixedPort: 7897, GatewayPort: 70000, Mode: "rule", LogLevel: "info"},
		{MixedPort: 7897, GatewayPort: 39097, Mode: "turbo", LogLevel: "info"},
		{MixedPort: 7897, GatewayPort: 39097, Mode: "rule", LogLevel: "loud"},
		{MixedPort: 7897, GatewayPort: 39097, Mode: "rule", LogLevel: "info", LogRetentionDays: -1},
		// 跨字段互斥（M3-plan §11.2-5）：mixed_port == gateway_port 拒绝（手改 YAML 同样被拦）
		{MixedPort: 7897, GatewayPort: 7897, Mode: "rule", LogLevel: "info"},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("Validate(%+v) should fail", s)
		}
	}
	good := Settings{MixedPort: 7897, GatewayPort: 39097, PanelEnabled: false, Mode: "rule", LogLevel: "info"}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(good) = %v", err)
	}
}

func TestLoadSettingsPartialFileGetsDefaults(t *testing.T) {
	// M0 时代的 settings.yaml 缺 gateway_port/panel_enabled：加载后必须补默认值
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.SettingsFile(), []byte("mixed_port: 17897\nmode: rule\nlog_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.GatewayPort != DefaultGatewayPort {
		t.Fatalf("gateway_port = %d, want default %d", s.GatewayPort, DefaultGatewayPort)
	}
	if !s.PanelEnabled {
		t.Fatal("panel_enabled = false, want default true")
	}
}

func TestLoadSettingsExplicitZeroGatewayPortNormalized(t *testing.T) {
	// 显式 0 同样归一为默认（M1-plan §4.1：0/缺失 → 默认 39097）
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.SettingsFile(), []byte("gateway_port: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.GatewayPort != DefaultGatewayPort {
		t.Fatalf("gateway_port = %d, want normalized default", s.GatewayPort)
	}
}

func TestLoadSettingsCorrupt(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.SettingsFile(), []byte("mixed_port: notanint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(home); err == nil {
		t.Fatal("corrupt settings: want error")
	}
}

func TestLoadSettingsM2NewKeysDefaults(t *testing.T) {
	// M2 新增键：旧 settings.yaml 缺 log_retention_days/update_via_proxy → 归一默认（14 / true）
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.SettingsFile(), []byte("mixed_port: 17897\nmode: rule\nlog_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if s.LogRetentionDays != DefaultLogRetentionDays {
		t.Fatalf("log_retention_days = %d, want default %d", s.LogRetentionDays, DefaultLogRetentionDays)
	}
	if !s.UpdateViaProxyEnabled() {
		t.Fatal("update_via_proxy nil must default to true")
	}
}

func TestUpdateViaProxyExplicitValues(t *testing.T) {
	// 指针语义：nil=true；显式 false 必须生效（不能被默认吞掉）
	no := false
	s := Settings{UpdateViaProxy: &no}
	if s.UpdateViaProxyEnabled() {
		t.Fatal("explicit update_via_proxy=false must stay false")
	}
	yes := true
	if !(Settings{UpdateViaProxy: &yes}).UpdateViaProxyEnabled() {
		t.Fatal("explicit true must stay true")
	}
	if !(Settings{}).UpdateViaProxyEnabled() {
		t.Fatal("nil must default true")
	}
}
