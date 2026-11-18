package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// fakeNetRunner 记录全部 networksetup 调用（断言零调用/调用序列的关键），
// 读命令返回单服务 Wi-Fi 的合法输出——绝不触碰真实系统代理。
type fakeNetRunner struct {
	calls        [][]string
	applyFailure string // 非空时写入命令返回错误（模拟权限失败）
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
	backups := []store.ServiceBackup{{
		Service: "Wi-Fi",
		Web:     store.ProxyState{Enabled: true, Server: "10.0.0.1", Port: 8888},
		Secure:  store.ProxyState{Enabled: false},
		Socks:   store.ProxyState{Enabled: false},
		Bypass:  []string{"*.local"},
	}}
	st := store.DefaultState()
	st.SysProxy = store.SysProxyState{Applied: true, Services: backups}
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
	if err := sysProxyOnWith(home, 7897, &fake); err != nil {
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

func TestSysProxyOnAppliesWhenNotApplied(t *testing.T) {
	// 未应用 + 内核可达：备份 → 落盘 applied=true → 写入
	var seen []string
	home, cleanup := modeStubCore(t, &seen)
	defer cleanup()

	var fake fakeNetRunner
	if err := sysProxyOnWith(home, 7897, &fake); err != nil {
		t.Fatalf("on: %v", err)
	}
	for _, want := range []string{"-setwebproxy", "-setsecurewebproxy", "-setsocksfirewallproxy", "-setproxybypassdomains"} {
		found := false
		for _, call := range fake.calls {
			if call[0] == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("apply missing %s; calls: %v", want, fake.calls)
		}
	}
	if st := appliedState(t, home); !st.Applied {
		t.Fatal("state must be applied=true after on")
	}
}

func TestSysProxyOnApplyFailureRollsBack(t *testing.T) {
	// 写入失败：立即回滚备份，applied=false（绝不留半应用状态）
	home := paths.Home(stubSocketDir(t))
	fake := &fakeNetRunner{applyFailure: "permission denied"}
	if err := sysProxyOnWith(home, 7897, fake); err == nil {
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
	if err := sysProxyOnWith(home, 7897, &apply); err != nil {
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
