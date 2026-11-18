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
	s := Settings{MixedPort: 17897, GatewayPort: 19097, PanelEnabled: true, Mode: "global", LogLevel: "debug"}
	if err := SaveSettings(home, s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
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
