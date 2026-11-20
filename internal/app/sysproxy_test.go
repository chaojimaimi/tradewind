package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// fakeNetRunner 记录全部 networksetup 调用（断言零调用/调用序列的关键），
// 读命令返回单服务 Wi-Fi 的合法输出——绝不触碰真实系统代理。
// autoURL/autoEnabled 可配置自动代理读回态（PAC 备份矩阵用）。
type fakeNetRunner struct {
	calls        [][]string
	applyFailure string // 非空时写入命令返回错误（模拟权限失败）
	autoURL      string
	autoEnabled  bool
}

func (f *fakeNetRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	action := args[0]
	switch {
	case action == "-listallnetworkservices":
		return "An asterisk (*) denotes that a network service is disabled.\nWi-Fi", nil
	case action == "-getinfo":
		return "Manually Configured: yes\nIP address: 192.168.1.9", nil
	case action == "-getwebproxy", action == "-getsecurewebproxy", action == "-getsocksfirewallproxy":
		// 预置"干净"原值：代理关闭、无 server
		return "Enabled: No\nServer: \nPort: 0", nil
	case action == "-getproxybypassdomains":
		return "There aren't any bypass domains", nil
	case action == "-getautoproxyurl":
		enabled := "No"
		if f.autoEnabled {
			enabled = "Yes"
		}
		url := f.autoURL
		if url == "" {
			url = "(null)"
		}
		return fmt.Sprintf("URL: %s\nEnabled: %s", url, enabled), nil
	}
	// 写命令（-set*）
	if f.applyFailure != "" {
		return "", fmt.Errorf("simulated: %s", f.applyFailure)
	}
	return "", nil
}

// writeAppliedState 写入 applied=true 的状态（幂等/on 分支的前置）。
func writeAppliedState(t *testing.T, home paths.Home) []store.ServiceBackup {
	t.Helper()
	return writeAppliedStateForm(t, home, "")
}

// writeAppliedStateForm 同 writeAppliedState，可指定会话 Form（legacy=""/manual/pac）。
func writeAppliedStateForm(t *testing.T, home paths.Home, form string) []store.ServiceBackup {
	t.Helper()
	backups := []store.ServiceBackup{{
		Service: "Wi-Fi",
		Web:     store.ProxyState{Enabled: true, Server: "10.0.0.1", Port: 8888},
		Secure:  store.ProxyState{Enabled: false},
		Socks:   store.ProxyState{Enabled: false},
		Bypass:  []string{"*.local"},
	}}
	st := store.DefaultState()
	st.SysProxy = store.SysProxyState{Applied: true, Services: backups, Form: form}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	return backups
}

func appliedState(t *testing.T, home paths.Home) store.SysProxyState {
	t.Helper()
	st, err := store.LoadState(home)
	if err != nil {
		t.Fatal(err)
	}
	return st.SysProxy
}

func TestSysProxyOnRequiresReachableCore(t *testing.T) {
	// 内核不可达 → on 必须报错拒绝且零 networksetup 调用（M2-plan §4.1 防断网陷阱）
	home := paths.Home(stubSocketDir(t)) // 无 sock → 不可达
	err := SysProxyOn(home)
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("SysProxyOn(unreachable) err = %v, want guard error", err)
	}
}

func TestSysProxyOnIdempotentApplied(t *testing.T) {
	// applied=true 时 on：即使内核可达也必须 no-op、零 networksetup 调用、备份不变
	var seen []string
	home, cleanup := modeStubCore(t, &seen) // stub core → 可达
	defer cleanup()
	backup := writeAppliedState(t, home)

	var fake fakeNetRunner
	if err := sysProxyOnWith(home, 7897, &fake, sysproxy.FormManual); err != nil {
		t.Fatalf("second on: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("second on must make zero networksetup calls; got %v", fake.calls)
	}
	now := appliedState(t, home)
	// ServiceBackup 含 []string 不可直接 ==，逐字段断言备份未变
	if len(now.Services) != 1 ||
		now.Services[0].Service != backup[0].Service ||
		now.Services[0].Web != backup[0].Web ||
		len(now.Services[0].Bypass) != len(backup[0].Bypass) {
		t.Fatalf("backup changed after idempotent on: %+v", now.Services)
	}
}

// TestSysProxyOnManualGoldenSequence 金标准锚（批④纪律 9）：手工形态 + settings 缺省
// bypass 的写入命令序列与现版本逐命令一致（bypass 参数化后 EffectiveBypass(nil)
// 必须展开为同一缺省集，无新增/删减命令）。
func TestSysProxyOnManualGoldenSequence(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home) // 默认 settings：PacMode=false、BypassDomains=nil
	var fake fakeNetRunner
	if err := sysProxyOnWith(home, 7897, &fake, sysproxy.FormManual); err != nil {
		t.Fatalf("on: %v", err)
	}
	want := [][]string{
		{"-listallnetworkservices"},
		{"-getinfo", "Wi-Fi"},
		{"-getwebproxy", "Wi-Fi"},
		{"-getsecurewebproxy", "Wi-Fi"},
		{"-getsocksfirewallproxy", "Wi-Fi"},
		{"-getproxybypassdomains", "Wi-Fi"},
		{"-getautoproxyurl", "Wi-Fi"},
		{"-setwebproxy", "Wi-Fi", "127.0.0.1", "7897"},
		{"-setsecurewebproxy", "Wi-Fi", "127.0.0.1", "7897"},
		{"-setsocksfirewallproxy", "Wi-Fi", "127.0.0.1", "7897"},
		{"-setproxybypassdomains", "Wi-Fi", "127.0.0.1", "localhost", "*.local", "169.254/16"},
	}
	if len(fake.calls) != len(want) {
		t.Fatalf("call count = %d, want %d; calls: %v", len(fake.calls), len(want), fake.calls)
	}
	for i, c := range want {
		if strings.Join(fake.calls[i], " ") != strings.Join(c, " ") {
			t.Fatalf("call[%d] = %v, want %v", i, fake.calls[i], c)
		}
	}
	st := appliedState(t, home)
	if !st.Applied || st.Form != sysproxy.FormManual {
		t.Fatalf("state = applied=%v form=%q, want applied=true form=manual", st.Applied, st.Form)
	}
}

// stubGatewayReachable 替换 gatewayReachable 缝隙并记录调用；t.Cleanup 还原
// （真探测与本机端口占用环境耦合——评审守卫分支测试钉死可注入）。
func stubGatewayReachable(t *testing.T, reachable bool) *[]int {
	t.Helper()
	var probed []int
	old := gatewayReachable
	gatewayReachable = func(port int) bool {
		probed = append(probed, port)
		return reachable
	}
	t.Cleanup(func() { gatewayReachable = old })
	return &probed
}

// TestSysProxyOnPACFormMatrix 双形态矩阵（fake runner）：PAC 形态写入 autoproxy
// url+state on+bypass；备份捕获原 autoproxy 态；state Form=pac。
func TestSysProxyOnPACFormMatrix(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	stubGatewayReachable(t, true) // 守卫放行（真探测会与运行中网关耦合）
	// 用户机器原本有自动代理（URL 指旧 PAC、开关关——真实常见态），必须被完整捕获
	fake := &fakeNetRunner{autoURL: "http://old.example/pac", autoEnabled: false}
	if err := sysProxyOnWith(home, 7897, fake, sysproxy.FormPAC); err != nil {
		t.Fatalf("on pac: %v", err)
	}
	var hasURL, hasStateOn, hasBypass bool
	for _, c := range fake.calls {
		switch {
		case c[0] == "-setautoproxyurl" && c[2] == "http://127.0.0.1:39097/pac":
			hasURL = true
		case c[0] == "-setautoproxystate" && c[2] == "on":
			hasStateOn = true
		case c[0] == "-setproxybypassdomains":
			hasBypass = true
		case c[0] == "-setwebproxy" || c[0] == "-setsecurewebproxy" || c[0] == "-setsocksfirewallproxy":
			t.Fatalf("PAC form must not write manual proxy commands: %v", c)
		}
	}
	if !hasURL || !hasStateOn || !hasBypass {
		t.Fatalf("PAC apply incomplete: url=%v stateOn=%v bypass=%v; calls %v", hasURL, hasStateOn, hasBypass, fake.calls)
	}
	st := appliedState(t, home)
	if !st.Applied || st.Form != sysproxy.FormPAC {
		t.Fatalf("state = applied=%v form=%q, want applied=true form=pac", st.Applied, st.Form)
	}
	if len(st.Services) != 1 || st.Services[0].AutoProxy.URL != "http://old.example/pac" || st.Services[0].AutoProxy.Enabled {
		t.Fatalf("backup autoproxy = %+v, want URL preserved + Enabled=false", st.Services[0].AutoProxy)
	}
}

// TestSysProxyOffPACFormRestoresFromBackup 双形态矩阵 off 侧：form=pac + 备份
// AutoProxy{URL:x, Enabled:No}（真实常见态）→ Restore 按 URL 恒写回、state 跟随
// 备份 Enabled=off；form=manual/legacy → 零 autoproxy 命令。
func TestSysProxyOffPACFormRestoresFromBackup(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	backups := writeAppliedStateForm(t, home, sysproxy.FormPAC)
	backups[0].AutoProxy = store.AutoProxyState{URL: "http://old.example/pac", Enabled: false}
	st, _ := store.LoadState(home)
	st.SysProxy.Services = backups
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}

	var fake fakeNetRunner
	if err := sysProxyOffWith(home, &fake); err != nil {
		t.Fatalf("off: %v", err)
	}
	var urlCmd, stateCmd []string
	for _, c := range fake.calls {
		if c[0] == "-setautoproxyurl" {
			urlCmd = c
		}
		if c[0] == "-setautoproxystate" {
			stateCmd = c
		}
	}
	if urlCmd == nil || len(urlCmd) < 3 || urlCmd[2] != "http://old.example/pac" {
		t.Fatalf("pac restore must write back backup URL; calls: %v", fake.calls)
	}
	if stateCmd == nil || stateCmd[2] != "off" {
		t.Fatalf("state must follow backup Enabled=false (off); got %v", stateCmd)
	}
	if s := appliedState(t, home); s.Applied {
		t.Fatal("state must be applied=false after off")
	}
}

// TestSysProxyOffLegacyBackupZeroAutoproxyTouch 旧备份兼容（金标准锚）：无 Form
// （空串=legacy manual）+ 备份无 auto_proxy 键的 off 零 autoproxy 扰动。
func TestSysProxyOffLegacyBackupZeroAutoproxyTouch(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	writeAppliedState(t, home) // Form=""，ServiceBackup 无 AutoProxy（零值）
	var fake fakeNetRunner
	if err := sysProxyOffWith(home, &fake); err != nil {
		t.Fatalf("off: %v", err)
	}
	for _, c := range fake.calls {
		if c[0] == "-setautoproxyurl" || c[0] == "-setautoproxystate" {
			t.Fatalf("legacy backup restore must not touch autoproxy; calls: %v", fake.calls)
		}
	}
}

// TestSysProxyOnPACGatewayUnreachable PAC 生效守卫（批④ §3.1）：form=pac + 网关
// 不可达 → 报错"面板网关不可达，PAC 无法生效"，不写任何系统代理命令、不落盘 applied。
func TestSysProxyOnPACGatewayUnreachable(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	probed := stubGatewayReachable(t, false)
	var fake fakeNetRunner
	err := sysProxyOnWith(home, 7897, &fake, sysproxy.FormPAC)
	if err == nil || !strings.Contains(err.Error(), "面板网关不可达，PAC 无法生效") {
		t.Fatalf("err = %v, want PAC gateway guard error", err)
	}
	if len(*probed) != 1 || (*probed)[0] != store.DefaultGatewayPort {
		t.Fatalf("guard must probe settings gateway port; probed %v", *probed)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("guard failure must make zero networksetup calls; got %v", fake.calls)
	}
	if st := appliedState(t, home); st.Applied {
		t.Fatal("state must stay applied=false on guard failure")
	}
}

func TestSysProxyOnApplyFailureRollsBack(t *testing.T) {
	// 写入失败：立即回滚备份，applied=false（绝不留半应用状态）
	home := paths.Home(stubSocketDir(t))
	fake := &fakeNetRunner{applyFailure: "permission denied"}
	if err := sysProxyOnWith(home, 7897, fake, sysproxy.FormManual); err == nil {
		t.Fatal("apply failure: want error")
	}
	if st := appliedState(t, home); st.Applied {
		t.Fatal("state after failed apply must be applied=false")
	}
}

func TestSysProxyOffRestoresFromBackup(t *testing.T) {
	// off：按备份精确还原（含原 server/port 恢复与 enabled=false 关闭），applied=false
	home := paths.Home(stubSocketDir(t))
	backup := writeAppliedState(t, home)
	var fake fakeNetRunner
	if err := sysProxyOffWith(home, &fake); err != nil {
		t.Fatalf("off: %v", err)
	}
	var hasSetWeb, hasStateOff bool
	for _, call := range fake.calls {
		if call[0] == "-setwebproxy" && call[2] == backup[0].Web.Server && call[3] == "8888" {
			hasSetWeb = true
		}
		if call[0] == "-setsecurewebproxystate" && call[2] == "off" {
			hasStateOff = true
		}
	}
	if !hasSetWeb || !hasStateOff {
		t.Fatalf("restore commands incomplete; calls: %v", fake.calls)
	}
	if st := appliedState(t, home); st.Applied {
		t.Fatal("state must be applied=false after off")
	}
}

func TestSysProxyOffNoOpWhenNotApplied(t *testing.T) {
	// applied=false：off 直接成功返回，零调用（无残留语义）
	home := paths.Home(stubSocketDir(t))
	var fake fakeNetRunner
	if err := sysProxyOffWith(home, &fake); err != nil {
		t.Fatalf("off when not applied: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("off when not applied must make zero calls; got %v", fake.calls)
	}
}

func TestSysProxyOnOffRoundTrip(t *testing.T) {
	// 集成形态：on（假 runner 落盘）→ off → 再 off（no-op），状态全程一致
	home := paths.Home(stubSocketDir(t))
	var apply, restore fakeNetRunner
	if err := sysProxyOnWith(home, 7897, &apply, sysproxy.FormManual); err != nil {
		t.Fatalf("on: %v", err)
	}
	if err := sysProxyOffWith(home, &restore); err != nil {
		t.Fatalf("off: %v", err)
	}
	before := len(restore.calls)
	if err := sysProxyOffWith(home, &restore); err != nil {
		t.Fatalf("second off: %v", err)
	}
	if len(restore.calls) != before {
		t.Fatalf("second off must be zero-call no-op")
	}
}
