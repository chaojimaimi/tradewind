package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

func TestStatusJSONDegradedPanel(t *testing.T) {
	// 壳降级态（M2-plan §5）：网关不可达 → panel_url=""（空串而非错误），控制面字段仍完整
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads) // 内核可达（core.running=true）
	defer cleanup()
	settings := store.DefaultSettings()
	settings.GatewayPort = freePort(t) // 无网关监听
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, home, nil)
	// pid 报告自 state.json（运行态真相源）；预置 running 态供断言
	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: 4242, Generation: 1}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}

	r, err := Status(home)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	out, err := StatusReport(home, r)
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	line, err := MarshalStatusJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["mode"] != store.DefaultMode {
		t.Fatalf("top-level mode = %v, want %q", m["mode"], store.DefaultMode)
	}
	core := m["core"].(map[string]any)
	if core["running"] != true || core["pid"].(float64) <= 0 {
		t.Fatalf("core = %v, want running with pid", core)
	}
	sp := m["sys_proxy"].(map[string]any)
	if sp["applied"] != false {
		t.Fatalf("sys_proxy = %v, want applied=false", sp)
	}
	if out.PanelURL != "" {
		t.Fatalf("panel_url = %q, want empty on unreachable gateway", out.PanelURL)
	}
}

func TestStatusJSONPanelURLWithGateway(t *testing.T) {
	// panel_url 是壳数据面端口/token 的唯一来源（M2-plan §2.5）：含端口 + token 入口
	port := freePort(t)
	home := paths.Home(stubSocketDir(t)) // sock 不存在 → core.running=false（也要正确上报）
	seedUIDir(t, home)
	gw, url, err := startGateway(home, store.Settings{PanelEnabled: true, GatewayPort: port})
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	defer gw.close()
	_ = url

	r, err := Status(home)
	if err != nil {
		t.Fatal(err)
	}
	out, err := StatusReport(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.PanelURL, "http://127.0.0.1:") || !strings.Contains(out.PanelURL, "/ui/") {
		t.Fatalf("panel_url = %q, want entry URL with port and token", out.PanelURL)
	}
	if len(strings.Split(out.PanelURL, "/ui/")) != 2 || len(strings.TrimSuffix(strings.Split(out.PanelURL, "/ui/")[1], "/")) != 64 {
		t.Fatalf("panel_url token segment malformed: %q", out.PanelURL)
	}
	if out.Core.Running {
		t.Fatalf("core.running = true, want false (no sock)")
	}
}

func TestPanelURLOnlyPositive(t *testing.T) {
	// panel --url：网关可达 → 返回入口 URL（不开浏览器；浏览器打开路径由 Panel 覆盖）
	port := freePort(t)
	home := paths.Home(stubSocketDir(t))
	seedUIDir(t, home)
	gw, _, err := startGateway(home, store.Settings{PanelEnabled: true, GatewayPort: port})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.close()
	url, err := PanelEntryURL(home)
	if err != nil {
		t.Fatalf("PanelEntryURL: %v", err)
	}
	if url == "" {
		t.Fatal("reachable gateway must yield URL")
	}
}

// TestStatusJSONGuardKeys 批④ §2.1：status --json 透出 Form/HijackEvents/LastRestore
// 三新键；applied=false 时 Form/事件 omitempty 缺键（旧壳解码兼容）。
func TestStatusJSONGuardKeys(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	st := store.DefaultState()
	st.SysProxy = store.SysProxyState{
		Applied:      true,
		Form:         sysproxy.FormPAC,
		HijackEvents: 2,
		LastRestore:  1737500000,
	}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	r, err := Status(home)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	out, err := StatusReport(home, r)
	if err != nil {
		t.Fatalf("StatusReport: %v", err)
	}
	line, err := MarshalStatusJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"form":"pac"`, `"hijack_events":2`, `"last_restore":1737500000`} {
		if !strings.Contains(line, want) {
			t.Fatalf("status --json missing %s: %s", want, line)
		}
	}
	// legacy 态（零值）三键缺省
	home2 := paths.Home(stubSocketDir(t))
	if err := store.SaveState(home2, store.DefaultState()); err != nil {
		t.Fatal(err)
	}
	r2, _ := Status(home2)
	out2, err := StatusReport(home2, r2)
	if err != nil {
		t.Fatal(err)
	}
	line2, _ := MarshalStatusJSON(out2)
	if strings.Contains(line2, `"form"`) || strings.Contains(line2, `"hijack_events"`) || strings.Contains(line2, `"last_restore"`) {
		t.Fatalf("legacy state must omit guard keys: %s", line2)
	}
}
