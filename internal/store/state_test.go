package store

import (
	"os"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestLoadStateDefaults(t *testing.T) {
	// 缺省状态即"无残留"：applied=false 是启动清理的唯一判据
	home := paths.Home(t.TempDir())
	st, err := LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	want := DefaultState()
	if st.Core != want.Core || st.SysProxy.Applied != want.SysProxy.Applied || len(st.SysProxy.Services) != 0 {
		t.Fatalf("default state = %+v, want %+v", st, want)
	}
	if st.SysProxy.Applied || st.Core.PID != 0 || st.Core.Desired != DesiredStopped {
		t.Fatalf("default state must mean no residue: %+v", st)
	}
}

func TestStateRoundTripKeepsEmptyBypass(t *testing.T) {
	// bypass 为空时还原须走 "-setproxybypassdomains <svc> Empty"，空列表不能被序列化吞成 nil 语义
	home := paths.Home(t.TempDir())
	st := DefaultState()
	st.Core = CoreState{Desired: DesiredRunning, PID: 12345}
	st.SysProxy = SysProxyState{Applied: true, Services: []ServiceBackup{{
		Service: "Wi-Fi",
		Web:     ProxyState{Enabled: false},
		Secure:  ProxyState{Enabled: true, Server: "corp.proxy", Port: 8080},
		Socks:   ProxyState{Enabled: false},
		Bypass:  []string{},
	}}}
	if err := SaveState(home, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(home)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Core.PID != 12345 || got.Core.Desired != DesiredRunning {
		t.Fatalf("core state got %+v", got.Core)
	}
	if !got.SysProxy.Applied || len(got.SysProxy.Services) != 1 {
		t.Fatalf("sys_proxy got %+v", got.SysProxy)
	}
	svc := got.SysProxy.Services[0]
	if svc.Service != "Wi-Fi" || !svc.Secure.Enabled || svc.Secure.Server != "corp.proxy" || svc.Secure.Port != 8080 {
		t.Fatalf("service backup got %+v", svc)
	}
	if svc.Bypass == nil || len(svc.Bypass) != 0 {
		t.Fatalf("bypass got %#v, want non-nil empty slice", svc.Bypass)
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.StateFile(), []byte("{broken json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(home); err == nil {
		t.Fatal("corrupt state: want error")
	}
}

func TestEnsureSecret(t *testing.T) {
	home := paths.Home(t.TempDir())
	s1, err := EnsureSecret(home)
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	if len(s1) != 64 {
		t.Fatalf("secret len = %d, want 64 hex chars", len(s1))
	}
	// 二次读取必须返回同一 secret（否则每次重启换 secret，热重载握手全挂）
	s2, err := EnsureSecret(home)
	if err != nil {
		t.Fatalf("EnsureSecret again: %v", err)
	}
	if s1 != s2 {
		t.Fatalf("secret changed between calls")
	}
	info, err := os.Stat(home.SecretFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret perm = %o, want 600", info.Mode().Perm())
	}
}

func TestEnsureSecretKeepsExisting(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.SecretFile(), []byte("preset-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := EnsureSecret(home)
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	if s != "preset-secret" {
		t.Fatalf("secret = %q, want existing value", s)
	}
}
