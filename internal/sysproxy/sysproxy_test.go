package sysproxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// fakeRunner 按完整参数串回放预置输出，并记录全部命令（绝不触碰真实系统）。
type fakeRunner struct {
	outputs  map[string]string
	commands [][]string
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.commands = append(f.commands, args)
	if out, ok := f.outputs[strings.Join(args, " ")]; ok {
		return out, nil
	}
	return "", nil
}

func (f *fakeRunner) ran(prefix string) [][]string {
	var matched [][]string
	for _, c := range f.commands {
		if strings.Join(c, " ") == prefix || (len(c) > 0 && c[0] == prefix) {
			matched = append(matched, c)
		}
	}
	return matched
}

func TestApplyCommandsSequence(t *testing.T) {
	got := ApplyCommands("Wi-Fi", "127.0.0.1", 7897, appliedBypass)
	want := [][]string{
		{"-setwebproxy", "Wi-Fi", "127.0.0.1", "7897"},
		{"-setsecurewebproxy", "Wi-Fi", "127.0.0.1", "7897"},
		{"-setsocksfirewallproxy", "Wi-Fi", "127.0.0.1", "7897"},
		{"-setproxybypassdomains", "Wi-Fi", "127.0.0.1", "localhost", "*.local", "169.254/16"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestApplyPACCommandsSequence(t *testing.T) {
	// 批④ §3.3 评审 P1-1：-setautoproxyurl 必须恒配 -setautoproxystate on
	//（URL 命令不保证启用自动代理开关，缺 state 命令恒静默不生效）
	got := ApplyPACCommands("Wi-Fi", "http://127.0.0.1:39097/pac", appliedBypass)
	want := [][]string{
		{"-setautoproxyurl", "Wi-Fi", "http://127.0.0.1:39097/pac"},
		{"-setautoproxystate", "Wi-Fi", "on"},
		{"-setproxybypassdomains", "Wi-Fi", "127.0.0.1", "localhost", "*.local", "169.254/16"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestRestoreCommandsEnabledCorpProxy(t *testing.T) {
	// 用户原本开着 corp 代理：还原必须重新写入 corp server/port 并保持启用——不是简单全关
	got := RestoreCommands(store.ServiceBackup{
		Service: "Wi-Fi",
		Web:     store.ProxyState{Enabled: false},
		Secure:  store.ProxyState{Enabled: true, Server: "corp.proxy", Port: 8080},
		Socks:   store.ProxyState{Enabled: false},
		Bypass:  []string{"*.local", "169.254/16"},
	}, FormManual)
	wantSeq := [][]string{
		{"-setwebproxystate", "Wi-Fi", "off"},
		{"-setsecurewebproxy", "Wi-Fi", "corp.proxy", "8080"},
		{"-setsocksfirewallproxystate", "Wi-Fi", "off"},
		{"-setproxybypassdomains", "Wi-Fi", "*.local", "169.254/16"},
	}
	if !reflect.DeepEqual(got, wantSeq) {
		t.Fatalf("got %#v, want %#v", got, wantSeq)
	}
}

func TestRestoreCommandsLegacyBackupNoAutoproxyCommands(t *testing.T) {
	// 金标准锚（批④纪律 9）：旧备份（无 auto_proxy/无 Form）Restore 零 autoproxy 扰动——
	// form=manual/空（legacy 视为 manual）下不出现任何 autoproxy 命令
	for _, form := range []string{"", FormManual} {
		got := RestoreCommands(store.ServiceBackup{
			Service: "Wi-Fi",
			Web:     store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: 7897},
			Secure:  store.ProxyState{},
			Socks:   store.ProxyState{},
			Bypass:  []string{},
		}, form)
		for _, c := range got {
			if c[0] == "-setautoproxyurl" || c[0] == "-setautoproxystate" {
				t.Fatalf("form=%q: legacy backup must not touch autoproxy; got %#v", form, got)
			}
		}
	}
}

func TestRestoreCommandsPACFormFollowsBackup(t *testing.T) {
	// Restore 决策表（终审定稿）：form=pac → 备份非零（URL 非空）按备份精确恢复，
	// 开关跟随备份 Enabled——`URL: x / Enabled: No` 强行 on 会劫持到用户已关的旧 PAC
	backup := store.ServiceBackup{
		Service:   "Wi-Fi",
		Web:       store.ProxyState{},
		Secure:    store.ProxyState{},
		Socks:     store.ProxyState{},
		Bypass:    []string{},
		AutoProxy: store.AutoProxyState{URL: "http://old.example/pac", Enabled: false},
	}
	got := RestoreCommands(backup, FormPAC)
	want := [][]string{
		{"-setwebproxystate", "Wi-Fi", "off"},
		{"-setsecurewebproxystate", "Wi-Fi", "off"},
		{"-setsocksfirewallproxystate", "Wi-Fi", "off"},
		{"-setproxybypassdomains", "Wi-Fi", "Empty"},
		{"-setautoproxyurl", "Wi-Fi", "http://old.example/pac"},
		{"-setautoproxystate", "Wi-Fi", "off"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// Enabled=true 备份 → on
	backup.AutoProxy.Enabled = true
	got = RestoreCommands(backup, FormPAC)
	if got[len(got)-1][2] != "on" {
		t.Fatalf("enabled backup must restore state on: %#v", got)
	}
}

func TestRestoreCommandsPACFormZeroBackupWritesOff(t *testing.T) {
	// 决策表：form=pac + 备份零值（URL 空）→ setautoproxystate off（幂等，
	// 机器本无自动代理）；Enabled=true 但 URL 空同样按零值处理（URL 是载荷，
	// 无 URL 的"启用"无法有意义还原）
	for _, auto := range []store.AutoProxyState{{}, {Enabled: true}} {
		got := RestoreCommands(store.ServiceBackup{
			Service: "Wi-Fi", AutoProxy: auto,
		}, FormPAC)
		want := [][]string{
			{"-setwebproxystate", "Wi-Fi", "off"},
			{"-setsecurewebproxystate", "Wi-Fi", "off"},
			{"-setsocksfirewallproxystate", "Wi-Fi", "off"},
			{"-setproxybypassdomains", "Wi-Fi", "Empty"},
			{"-setautoproxystate", "Wi-Fi", "off"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("auto=%+v: got %#v, want %#v", auto, got, want)
		}
	}
}

func TestRestoreCommandsEmptyBypassUsesEmptyKeyword(t *testing.T) {
	// 边界：备份 bypass 为空时还原必须用 "Empty"（networksetup 传空串会报错）
	got := RestoreCommands(store.ServiceBackup{
		Service: "Ethernet",
		Web:     store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: 7897},
		Secure:  store.ProxyState{},
		Socks:   store.ProxyState{},
		Bypass:  []string{},
	}, FormManual)
	var bypassCmd []string
	for _, c := range got {
		if c[0] == "-setproxybypassdomains" {
			bypassCmd = c
		}
	}
	if bypassCmd == nil || !reflect.DeepEqual(bypassCmd, []string{"-setproxybypassdomains", "Ethernet", "Empty"}) {
		t.Fatalf("bypass cmd = %#v, want Empty keyword", bypassCmd)
	}
}

func TestSnapshotWithFakeRunner(t *testing.T) {
	fr := &fakeRunner{outputs: map[string]string{
		"-getwebproxy Wi-Fi":           "Enabled: No\nServer: \nPort: 0\n",
		"-getsecurewebproxy Wi-Fi":     "Enabled: Yes\nServer: corp.proxy\nPort: 8080\n",
		"-getsocksfirewallproxy Wi-Fi": "Enabled: No\nServer: \nPort: 0\n",
		"-getproxybypassdomains Wi-Fi": "*.local\n",
		"-getautoproxyurl Wi-Fi":       "URL: (null)\nEnabled: No\n",
	}}
	backups, err := Snapshot(fr, []string{"Wi-Fi"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %#v", backups)
	}
	b := backups[0]
	if b.Service != "Wi-Fi" || b.Web.Enabled || !b.Secure.Enabled ||
		b.Secure.Server != "corp.proxy" || b.Socks.Port != 0 ||
		!reflect.DeepEqual(b.Bypass, []string{"*.local"}) ||
		b.AutoProxy.URL != "" || b.AutoProxy.Enabled {
		t.Fatalf("backup = %+v", b)
	}
	// 自动代理已配置的服务：备份必须捕获 URL 与开关（Restore 决策表的精确还原依据）
	fr2 := &fakeRunner{outputs: map[string]string{
		"-getwebproxy Wi-Fi":           "Enabled: No\nServer: \nPort: 0\n",
		"-getsecurewebproxy Wi-Fi":     "Enabled: No\nServer: \nPort: 0\n",
		"-getsocksfirewallproxy Wi-Fi": "Enabled: No\nServer: \nPort: 0\n",
		"-getproxybypassdomains Wi-Fi": "There aren't any bypass domains\n",
		"-getautoproxyurl Wi-Fi":       "URL: http://127.0.0.1:39097/pac\nEnabled: Yes\n",
	}}
	backups, err = Snapshot(fr2, []string{"Wi-Fi"})
	if err != nil {
		t.Fatalf("Snapshot configured: %v", err)
	}
	if backups[0].AutoProxy.URL != "http://127.0.0.1:39097/pac" || !backups[0].AutoProxy.Enabled {
		t.Fatalf("autoproxy backup = %+v", backups[0].AutoProxy)
	}
}

func TestApplyAndRestoreViaFakeRunner(t *testing.T) {
	fr := &fakeRunner{}
	svcs := []string{"Wi-Fi", "Ethernet"}
	if err := Apply(fr, svcs, "127.0.0.1", 17890, appliedBypass); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 2 服务 x 4 命令
	if len(fr.ran("-setwebproxy")) != 2 {
		t.Fatalf("setwebproxy commands = %#v", fr.ran("-setwebproxy"))
	}
	if len(fr.commands) != len(svcs)*4 {
		t.Fatalf("total commands = %d, want %d", len(fr.commands), len(svcs)*4)
	}

	fr2 := &fakeRunner{}
	backup := store.ServiceBackup{
		Service: "Wi-Fi",
		Web:     store.ProxyState{Enabled: false},
		Secure:  store.ProxyState{Enabled: true, Server: "corp.proxy", Port: 8080},
		Socks:   store.ProxyState{Enabled: false},
		Bypass:  nil,
	}
	if err := Restore(fr2, []store.ServiceBackup{backup}, FormManual); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := [][]string{
		{"-setwebproxystate", "Wi-Fi", "off"},
		{"-setsecurewebproxy", "Wi-Fi", "corp.proxy", "8080"},
		{"-setsocksfirewallproxystate", "Wi-Fi", "off"},
		{"-setproxybypassdomains", "Wi-Fi", "Empty"},
	}
	if !reflect.DeepEqual(fr2.commands, want) {
		t.Fatalf("restore commands = %#v, want %#v", fr2.commands, want)
	}
}

func TestApplyPACViaFakeRunner(t *testing.T) {
	// PAC 形态端到端（fake runner）：2 服务 x 3 命令（url+state+bypass），state on 恒配对
	fr := &fakeRunner{}
	svcs := []string{"Wi-Fi", "Ethernet"}
	if err := ApplyPAC(fr, svcs, "http://127.0.0.1:39097/pac", nil); err != nil {
		t.Fatalf("ApplyPAC: %v", err)
	}
	if len(fr.commands) != len(svcs)*3 {
		t.Fatalf("total commands = %d, want %d", len(fr.commands), len(svcs)*3)
	}
	for _, svc := range svcs {
		var urlCmd, stateCmd []string
		for _, c := range fr.commands {
			if len(c) > 1 && c[1] != svc {
				continue
			}
			if c[0] == "-setautoproxyurl" {
				urlCmd = c
			}
			if c[0] == "-setautoproxystate" {
				stateCmd = c
			}
		}
		if urlCmd == nil || len(urlCmd) < 3 || urlCmd[2] != "http://127.0.0.1:39097/pac" {
			t.Fatalf("autoproxyurl for %s missing: %#v", svc, urlCmd)
		}
		if stateCmd == nil || stateCmd[2] != "on" {
			t.Fatalf("autoproxystate for %s must be on: %#v", svc, stateCmd)
		}
	}
}

func TestEffectiveBypass(t *testing.T) {
	// nil/空 → 缺省集（金标准锚：手工模式+缺省 bypass 与现版本逐命令一致）；
	// 自定义非空 → 原样透传
	if !reflect.DeepEqual(EffectiveBypass(nil), appliedBypass) {
		t.Fatalf("nil custom = %#v", EffectiveBypass(nil))
	}
	if !reflect.DeepEqual(EffectiveBypass([]string{}), appliedBypass) {
		t.Fatalf("empty custom = %#v", EffectiveBypass([]string{}))
	}
	custom := []string{"*.corp.example"}
	if !reflect.DeepEqual(EffectiveBypass(custom), custom) {
		t.Fatalf("custom = %#v", EffectiveBypass(custom))
	}
}

func TestActiveServicesFiltersDisabledAndDisconnected(t *testing.T) {
	fr := &fakeRunner{outputs: map[string]string{
		"-listallnetworkservices": "An asterisk (*) denotes that a network service is disabled.\nWi-Fi\n*VPN\nEthernet\n",
		"-getinfo Wi-Fi":          "Manually Using DHCP\nIP address: 192.168.1.5\n",
		"-getinfo Ethernet":       "Manually Using DHCP\n*IP address: \n",
	}}
	got, err := ActiveServices(fr)
	if err != nil {
		t.Fatalf("ActiveServices: %v", err)
	}
	// 禁用（*VPN）与未连接（无 IP）都必须被过滤
	if !reflect.DeepEqual(got, []string{"Wi-Fi"}) {
		t.Fatalf("got %#v", got)
	}
}
