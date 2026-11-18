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

func TestRestoreCommandsEnabledCorpProxy(t *testing.T) {
	// 用户原本开着 corp 代理：还原必须重新写入 corp server/port 并保持启用——不是简单全关
	got := RestoreCommands(store.ServiceBackup{
		Service: "Wi-Fi",
		Web:     store.ProxyState{Enabled: false},
		Secure:  store.ProxyState{Enabled: true, Server: "corp.proxy", Port: 8080},
		Socks:   store.ProxyState{Enabled: false},
		Bypass:  []string{"*.local", "169.254/16"},
	})
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

func TestRestoreCommandsEmptyBypassUsesEmptyKeyword(t *testing.T) {
	// 边界：备份 bypass 为空时还原必须用 "Empty"（networksetup 传空串会报错）
	got := RestoreCommands(store.ServiceBackup{
		Service: "Ethernet",
		Web:     store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: 7897},
		Secure:  store.ProxyState{},
		Socks:   store.ProxyState{},
		Bypass:  []string{},
	})
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
		!reflect.DeepEqual(b.Bypass, []string{"*.local"}) {
		t.Fatalf("backup = %+v", b)
	}
}

func TestApplyAndRestoreViaFakeRunner(t *testing.T) {
	fr := &fakeRunner{}
	svcs := []string{"Wi-Fi", "Ethernet"}
	if err := Apply(fr, svcs, "127.0.0.1", 17890); err != nil {
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
	if err := Restore(fr2, []store.ServiceBackup{backup}); err != nil {
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
