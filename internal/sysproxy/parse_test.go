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

func TestParseAutoProxyStateConfigured(t *testing.T) {
	// 批④ §3.3 评审 P2-6 样例钉死：PAC 已配置态
	url, enabled, err := ParseAutoProxyState("URL: http://127.0.0.1:39097/pac\nEnabled: Yes\n")
	if err != nil {
		t.Fatalf("ParseAutoProxyState: %v", err)
	}
	if url != "http://127.0.0.1:39097/pac" || !enabled {
		t.Fatalf("url=%q enabled=%v", url, enabled)
	}
}

func TestParseAutoProxyStateURLButDisabled(t *testing.T) {
	// `URL: x / Enabled: No` 是真实常见 macOS 态（用户曾配 PAC 后关闭）
	url, enabled, err := ParseAutoProxyState("URL: http://old.example/pac\nEnabled: No\n")
	if err != nil {
		t.Fatalf("ParseAutoProxyState: %v", err)
	}
	if url != "http://old.example/pac" || enabled {
		t.Fatalf("url=%q enabled=%v", url, enabled)
	}
}

func TestParseAutoProxyStateNullSentinel(t *testing.T) {
	// 未配置态：URL (null) 哨兵归一为零值
	url, enabled, err := ParseAutoProxyState("URL: (null)\nEnabled: No\n")
	if err != nil {
		t.Fatalf("ParseAutoProxyState: %v", err)
	}
	if url != "" || enabled {
		t.Fatalf("null sentinel must normalize to zero; url=%q enabled=%v", url, enabled)
	}
}

func TestParseAutoProxyStateUnrecognizedOutput(t *testing.T) {
	// 无已知键的输出（错误文本/空串）显式报错——静默零值会让备份误判
	if _, _, err := ParseAutoProxyState(""); err == nil {
		t.Fatal("empty output: want error")
	}
	if _, _, err := ParseAutoProxyState("** Error: Not Found\n"); err == nil {
		t.Fatal("garbage output: want error")
	}
}
