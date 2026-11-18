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
	s := Settings{MixedPort: 17897, Mode: "global", LogLevel: "debug"}
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
		{MixedPort: 0, Mode: "rule", LogLevel: "info"},
		{MixedPort: 70000, Mode: "rule", LogLevel: "info"},
		{MixedPort: 7897, Mode: "turbo", LogLevel: "info"},
		{MixedPort: 7897, Mode: "rule", LogLevel: "loud"},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("Validate(%+v) should fail", s)
		}
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
