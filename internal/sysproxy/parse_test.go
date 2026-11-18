package sysproxy

import (
	"reflect"
	"testing"
)

func TestParseEnabledServices(t *testing.T) {
	out := "An asterisk (*) denotes that a network service is disabled.\n" +
		"Wi-Fi\nThunderbolt Bridge\n*Disabled VPN\n\n"
	got := ParseEnabledServices(out)
	want := []string{"Wi-Fi", "Thunderbolt Bridge"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestParseProxyStateEnabledCorp(t *testing.T) {
	out := "Enabled: Yes\nServer: corp.proxy\nPort: 8080\nAuthenticated Proxy Enabled: 0\n"
	st, err := ParseProxyState(out)
	if err != nil {
		t.Fatalf("ParseProxyState: %v", err)
	}
	if !st.Enabled || st.Server != "corp.proxy" || st.Port != 8080 {
		t.Fatalf("state = %+v", st)
	}
}

func TestParseProxyStateDisabledEmpty(t *testing.T) {
	// 未设置过代理的典型输出：Enabled No / Server 空 / Port 0
	out := "Enabled: No\nServer: \nPort: 0\nAuthenticated Proxy Enabled: 0\n"
	st, err := ParseProxyState(out)
	if err != nil {
		t.Fatalf("ParseProxyState: %v", err)
	}
	if st.Enabled || st.Server != "" || st.Port != 0 {
		t.Fatalf("state = %+v", st)
	}
}

func TestParseProxyStateBadPort(t *testing.T) {
	if _, err := ParseProxyState("Enabled: No\nServer: x\nPort: notanumber\n"); err == nil {
		t.Fatal("bad port: want error")
	}
}

func TestParseConnected(t *testing.T) {
	connected := "Manually Using DHCP\nIP address: 192.168.1.5\nSubnet mask: 255.255.255.0\n"
	disconnected := "Manually Using DHCP\n*IP address: \nRouter: \n"
	if !ParseConnected(connected) {
		t.Fatal("service with IP should be connected")
	}
	if ParseConnected(disconnected) {
		t.Fatal("service without IP should be disconnected")
	}
	if ParseConnected("DHCP Configuration\n") {
		t.Fatal("no IP line should be disconnected")
	}
}

func TestParseBypassDomains(t *testing.T) {
	if got := ParseBypassDomains("*.local\n169.254/16\n"); !reflect.DeepEqual(got, []string{"*.local", "169.254/16"}) {
		t.Fatalf("got %#v", got)
	}
	if got := ParseBypassDomains(""); got != nil {
		t.Fatalf("empty output should give nil, got %#v", got)
	}
	// 清空后的读回可能输出说明文字或 Empty 关键字
	if got := ParseBypassDomains("There aren't any bypass domains for this service on the network interface.\n"); got != nil {
		t.Fatalf("explanatory line should give nil, got %#v", got)
	}
	if got := ParseBypassDomains("Empty\n"); got != nil {
		t.Fatalf("Empty keyword should give nil, got %#v", got)
	}
}
