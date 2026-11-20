package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// TestSettingsSetWhitelist 全部白名单键可写（未运行场景：仅落盘 + 重合成）。
func TestSettingsSetWhitelist(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	cases := []struct {
		key, value string
		check      func(store.Settings)
	}{
		{"mixed_port", "17897", func(s store.Settings) {
			if s.MixedPort != 17897 {
				t.Fatalf("mixed_port = %d, want 17897", s.MixedPort)
			}
		}},
		{"gateway_port", "19097", func(s store.Settings) {
			if s.GatewayPort != 19097 {
				t.Fatalf("gateway_port = %d, want 19097", s.GatewayPort)
			}
		}},
		{"panel_enabled", "off", func(s store.Settings) {
			if s.PanelEnabled {
				t.Fatal("panel_enabled = true, want false")
			}
		}},
		{"allow_lan", "on", func(s store.Settings) {
			if !s.AllowLan {
				t.Fatal("allow_lan = false, want true")
			}
		}},
		{"log_retention_days", "30", func(s store.Settings) {
			if s.LogRetentionDays != 30 {
				t.Fatalf("log_retention_days = %d, want 30", s.LogRetentionDays)
			}
		}},
	}
	for _, tc := range cases {
		if err := SettingsSet(context.Background(), home, tc.key, tc.value); err != nil {
			t.Fatalf("set %s=%s: %v", tc.key, tc.value, err)
		}
		s, err := store.LoadSettings(home)
		if err != nil {
			t.Fatal(err)
		}
		tc.check(s)
	}
	// runtime.yaml 与最后一次落盘一致（写编排含 Synthesize）
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !strings.Contains(string(data), "mixed-port: 17897") {
		t.Fatalf("runtime.yaml not resynthesized: %v", err)
	}
}

// TestSettingsSetInvalidKey 非法 key 显式报错且零写入（含 settings 字段）。
func TestSettingsSetInvalidKey(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	if err := SettingsSet(context.Background(), home, "dns", "on"); err == nil {
		t.Fatal("unknown key: want error")
	} else if !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, want unknown key hint", err)
	}
	if _, err := os.Stat(home.SettingsFile()); !os.IsNotExist(err) {
		t.Fatal("settings.yaml must not be written on invalid key")
	}
}

// TestSettingsSetInvalidValue 非法值显式报错（非整数 / 非开关 / 越界端口走 Validate）。
func TestSettingsSetInvalidValue(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	for _, tc := range []struct{ key, value string }{
		{"mixed_port", "abc"},
		{"panel_enabled", "maybe"},
		{"mixed_port", "70000"}, // 越界：Atoi 通过，SaveSettings 内 Validate 拒绝
	} {
		if err := SettingsSet(context.Background(), home, tc.key, tc.value); err == nil {
			t.Fatalf("set %s=%s: want error", tc.key, tc.value)
		}
	}
	s, err := store.LoadSettings(home)
	if err != nil || s.MixedPort != store.DefaultMixedPort {
		t.Fatalf("invalid values must not touch settings; got %+v err %v", s, err)
	}
}

// TestSettingsSetMutexRejected 跨字段互斥落位 Validate（M3-plan §11.2-5）：mixed_port == gateway_port 拒绝。
func TestSettingsSetMutexRejected(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	err := SettingsSet(context.Background(), home, "mixed_port", "39097")
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want mutual-exclusion error", err)
	}
	s, _ := store.LoadSettings(home)
	if s.MixedPort != store.DefaultMixedPort {
		t.Fatalf("mixed_port = %d, want untouched default", s.MixedPort)
	}
}

// TestSettingsSetOrchestrationRunning 运行中写编排生效：落盘 + 重合成 + PUT 热重载。
func TestSettingsSetOrchestrationRunning(t *testing.T) {
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedRuntimeInputs(t, home)

	if err := SettingsSet(context.Background(), home, "allow_lan", "on"); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	if len(reloads) != 1 || reloads[0] != http.MethodPut {
		t.Fatalf("config calls = %v, want exactly one PUT", reloads)
	}
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !strings.Contains(string(data), "allow-lan: true") {
		t.Fatalf("runtime.yaml missing allow-lan: true: %v", err)
	}
}

// TestSettingsSetMixedPortRetargetWhenApplied 评审 P1-3：applied 态 mixed_port 变更必须
// retarget 新端口（SysProxyOn applied 态 no-op 不可复用），且既有备份态原样保留。
func TestSettingsSetMixedPortRetargetWhenApplied(t *testing.T) {
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedRuntimeInputs(t, home)
	backup := writeAppliedState(t, home)

	var fake fakeNetRunner
	err := applySettingsMutationWith(context.Background(), home, func(s *store.Settings) error {
		return settingsSetKey(s, "mixed_port", "17890")
	}, &fake, false)
	if err != nil {
		t.Fatalf("applySettingsMutationWith: %v", err)
	}
	var hasNewPort bool
	for _, call := range fake.calls {
		if call[0] == "-setwebproxy" && call[2] == "127.0.0.1" && call[3] == "17890" {
			hasNewPort = true
		}
	}
	if !hasNewPort {
		t.Fatalf("retarget must apply new port 17890; calls: %v", fake.calls)
	}
	// 顺序契约：先 reload 后 retarget（此处 reload 已发生）
	if len(reloads) != 1 {
		t.Fatalf("reloads = %v, want exactly one", reloads)
	}
	// 备份态不变（还原语义仍指向用户原始网络配置）
	now := appliedState(t, home)
	if len(now.Services) != 1 || now.Services[0].Web != backup[0].Web {
		t.Fatalf("backup changed after retarget: %+v", now.Services)
	}
}

// TestSettingsSetRetargetApplyFailureExplicitError 审查 P3：retarget 的 networksetup
// 写命令失败（模拟权限失败）→ 显式报错含中文提示、不静默不回滚；settings 已落盘。
func TestSettingsSetRetargetApplyFailureExplicitError(t *testing.T) {
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedRuntimeInputs(t, home)
	writeAppliedState(t, home)

	fake := fakeNetRunner{applyFailure: "simulated permission failure"}
	err := applySettingsMutationWith(context.Background(), home, func(s *store.Settings) error {
		return settingsSetKey(s, "mixed_port", "17890")
	}, &fake, false)
	if err == nil {
		t.Fatal("retarget apply failure: want explicit error")
	}
	if !strings.Contains(err.Error(), "系统代理迁移失败") {
		t.Fatalf("error missing guidance text: %v", err)
	}
	s, _ := store.LoadSettings(home)
	if s.MixedPort != 17890 {
		t.Fatalf("mixed_port = %d, want 17890 persisted (crash-safe)", s.MixedPort)
	}
}

// TestSettingsSetReloadFailureNoRetarget reload 失败且降级重启也无着落 → 显式报错、不 retarget。
func TestSettingsSetReloadFailureNoRetarget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedRuntimeInputs(t, home)
	writeAppliedState(t, home) // state 无 PID → 降级重启无内核可管

	var fake fakeNetRunner
	err := applySettingsMutationWith(context.Background(), home, func(s *store.Settings) error {
		return settingsSetKey(s, "mixed_port", "17890")
	}, &fake, false)
	if err == nil {
		t.Fatal("reload failure with no manageable core: want error")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("failed reload must not retarget; calls: %v", fake.calls)
	}
	// 崩溃安全语义：settings 已落盘（文件先行，运行态由重试收敛）
	s, _ := store.LoadSettings(home)
	if s.MixedPort != 17890 {
		t.Fatalf("mixed_port = %d, want 17890 persisted", s.MixedPort)
	}
}

// TestSettingsSetNotRunningNoRetarget 内核未运行：仅落盘，reload 与 retarget 均不触发
// （系统代理不指向死端口，评审 P2 分支表）。
func TestSettingsSetNotRunningNoRetarget(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	writeAppliedState(t, home)

	var fake fakeNetRunner
	err := applySettingsMutationWith(context.Background(), home, func(s *store.Settings) error {
		return settingsSetKey(s, "mixed_port", "17890")
	}, &fake, false)
	if err != nil {
		t.Fatalf("not-running path: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("not-running must make zero networksetup calls; got %v", fake.calls)
	}
	s, _ := store.LoadSettings(home)
	if s.MixedPort != 17890 {
		t.Fatalf("mixed_port = %d, want 17890", s.MixedPort)
	}
}

// TestSettingsResetRestoresDefaults 恢复默认值走同一写编排（含定制 mixed_port 的落盘联动）。
func TestSettingsResetRestoresDefaults(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	s := store.DefaultSettings()
	s.Mode = "global"
	s.MixedPort = 12345
	s.AllowLan = true
	s.LogRetentionDays = 3
	if err := store.SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}

	var fake fakeNetRunner
	if err := applySettingsMutationWith(context.Background(), home, func(s *store.Settings) error {
		*s = store.DefaultSettings()
		return nil
	}, &fake, false); err != nil {
		t.Fatalf("SettingsReset: %v", err)
	}
	got, err := store.LoadSettings(home)
	if err != nil || !reflect.DeepEqual(got, store.DefaultSettings()) {
		t.Fatalf("after reset = %+v err %v, want defaults", got, err)
	}
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !strings.Contains(string(data), "mode: rule") || strings.Contains(string(data), "allow-lan: true") {
		t.Fatalf("runtime.yaml not resynthesized to defaults:\n%s", data)
	}
}

// TestSettingsGetJSONDecodable settings get --json 可解码且与默认值一致（壳通用页数据源契约）。
func TestSettingsGetJSONDecodable(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	if err := store.SaveSettings(home, store.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	s, err := store.LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	line, err := MarshalSettingsJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	var got store.Settings
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("settings --json not decodable: %v (%s)", err, line)
	}
	if !reflect.DeepEqual(got, store.DefaultSettings()) {
		t.Fatalf("decoded = %+v, want defaults", got)
	}
	// 键名与 yaml 一致（snake_case），壳按这些键建 Codable 模型
	for _, key := range []string{"mixed_port", "gateway_port", "panel_enabled", "allow_lan", "mode", "log_retention_days"} {
		if !strings.Contains(line, `"`+key+`"`) {
			t.Fatalf("settings --json missing key %q: %s", key, line)
		}
	}
}

// TestSettingsSetPacModeAndBypass 批④ §3.1：pac_mode 开关与 bypass_domains 逗号
// 分隔写入门（空=恢复缺省），白名单校验拒绝注入形态且不落盘。
func TestSettingsSetPacModeAndBypass(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)

	if err := SettingsSet(context.Background(), home, "pac_mode", "on"); err != nil {
		t.Fatalf("set pac_mode: %v", err)
	}
	s, err := store.LoadSettings(home)
	if err != nil || !s.PacMode {
		t.Fatalf("pac_mode = %v err %v, want true", s.PacMode, err)
	}

	if err := SettingsSet(context.Background(), home, "bypass_domains", "*.local, 169.254/16 ,<local>"); err != nil {
		t.Fatalf("set bypass_domains: %v", err)
	}
	s, _ = store.LoadSettings(home)
	want := []string{"*.local", "169.254/16", "<local>"}
	if !reflect.DeepEqual(s.BypassDomains, want) {
		t.Fatalf("bypass = %#v, want %#v", s.BypassDomains, want)
	}

	// 空=恢复缺省（nil → 生效值取 sysproxy 缺省集）
	if err := SettingsSet(context.Background(), home, "bypass_domains", ""); err != nil {
		t.Fatalf("set bypass_domains empty: %v", err)
	}
	s, _ = store.LoadSettings(home)
	if s.BypassDomains != nil {
		t.Fatalf("empty bypass must reset to nil, got %#v", s.BypassDomains)
	}

	// 非法形态拒绝且不落盘（Validate 下沉）
	if err := SettingsSet(context.Background(), home, "bypass_domains", "evil;x"); err == nil {
		t.Fatal("injected bypass: want error")
	}
	s, _ = store.LoadSettings(home)
	if s.BypassDomains != nil {
		t.Fatalf("rejected bypass must not persist: %#v", s.BypassDomains)
	}
	if err := SettingsSet(context.Background(), home, "pac_mode", "maybe"); err == nil {
		t.Fatal("invalid pac_mode: want error")
	}
}

// TestPostHandshakePACGatewayDowngradedSkips 批④ §3.3 评审二轮 P2-3：start 路径
// pac_mode on + 网关降级 → 跳过系统代理写入 + 显著警告，不 fail start（F4 降级哲学）。
func TestPostHandshakePACGatewayDowngradedSkips(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	stubGatewayReachable(t, false)

	mgr := core.NewManager(core.Config{SockPath: home.SockFile()}) // 未 spawn：仅装配态
	settings := store.DefaultSettings()
	settings.PacMode = true
	opts := StartOptions{ApplySysProxy: true}
	generation, err := postHandshake(context.Background(), home, mgr, opts, settings, "")
	if err != nil {
		t.Fatalf("postHandshake with downgraded gateway = %v, want nil (skip, not fail start)", err)
	}
	if generation != 1 {
		t.Fatalf("generation = %d, want 1", generation)
	}
	st, err := store.LoadState(home)
	if err != nil {
		t.Fatal(err)
	}
	if st.SysProxy.Applied {
		t.Fatal("sysproxy must stay unapplied on PAC guard skip")
	}
	if st.Core.Desired != store.DesiredRunning {
		t.Fatalf("desired = %q, want running (start continues)", st.Core.Desired)
	}
}
