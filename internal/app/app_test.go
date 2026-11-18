package app

import (
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:            "0 B",
		512:          "512 B",
		1024:         "1.00 KB",
		226100036:    "215.63 MB",
		107374182400: "100.00 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestReportExitCode(t *testing.T) {
	// 退出码契约：desired=running 但内核不可达 → 1；其余 → 0
	if (Report{Desired: store.DesiredRunning, CoreHealthy: false}).ExitCode() != 1 {
		t.Fatal("running but unhealthy should exit 1")
	}
	if (Report{Desired: store.DesiredRunning, CoreHealthy: true}).ExitCode() != 0 {
		t.Fatal("running and healthy should exit 0")
	}
	if (Report{Desired: store.DesiredStopped, CoreHealthy: false}).ExitCode() != 0 {
		t.Fatal("stopped should exit 0")
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://airport.example.com/token?abc=SECRET": "airport.example.com",
		"http://sub.example.net:8080/path":             "sub.example.net:8080",
		"plain.example.org/x":                          "plain.example.org",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Fatalf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(hostOf("https://a.com/t?x=SECRET"), "SECRET") {
		t.Fatal("host must not include query")
	}
}

func TestStatusOnEmptyHome(t *testing.T) {
	// 空数据目录的 status 必须正常退出（desired=stopped, 无订阅）
	home := paths.Home(t.TempDir())
	r, err := Status(home)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if r.Desired != store.DesiredStopped || r.CoreHealthy || r.ExitCode() != 0 {
		t.Fatalf("report = %+v", r)
	}
}
