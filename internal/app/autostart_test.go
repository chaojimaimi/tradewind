package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// fakeLaunchctl 记录 launchctl 调用；listEnabled 决定 `launchctl list` 是否成功。
type fakeLaunchctl struct {
	calls      [][]string
	listLoaded bool
}

func (f *fakeLaunchctl) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if args[0] == "list" && !f.listLoaded {
		return "", errNotFound()
	}
	return "", nil
}

func errNotFound() error { return os.ErrNotExist }

func TestLaunchdPlistFields(t *testing.T) {
	// 字段断言（M2-plan §6）：SuccessfulExit / os.Executable 形态 ProgramArguments / Standard*Path
	content, err := launchdPlist(LaunchAgentLabel, "/opt/tools/tradewind", "/Users/x/data/logs")
	if err != nil {
		t.Fatalf("launchdPlist: %v", err)
	}
	s := string(content)
	for _, want := range []string{
		"<string>" + LaunchAgentLabel + "</string>",
		"<string>/opt/tools/tradewind</string>", // 实际路径而非硬编码 /opt/homebrew/bin
		"<string>start</string>",
		"<key>RunAtLoad</key>",
		"<key>SuccessfulExit</key>",
		"<string>/Users/x/data/logs/launchd.out.log</string>",
		"<string>/Users/x/data/logs/launchd.err.log</string>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("plist missing %q:\n%s", want, s)
		}
	}
	// SuccessfulExit=false 的精确排布：KeepAlive dict 内必须为 false（崩溃才拉起）
	ka := s[strings.Index(s, "<key>KeepAlive</key>"):]
	if !strings.Contains(ka, "<false/>") {
		t.Fatalf("KeepAlive must gate on SuccessfulExit=false:\n%s", ka)
	}
}

func TestEnableAutostartWritesPlistAndLoads(t *testing.T) {
	dir := t.TempDir()
	home := paths.Home(t.TempDir())
	fake := &fakeLaunchctl{}
	exe := filepath.Join(t.TempDir(), "tradewind")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := enableAutostart(home, dir, exe, fake); err != nil {
		t.Fatalf("enableAutostart: %v", err)
	}
	p := plistPathFor(dir)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if !strings.Contains(string(data), exe) {
		t.Fatalf("plist missing exe path:\n%s", data)
	}
	if len(fake.calls) != 1 || fake.calls[0][0] != "load" || fake.calls[0][1] != p {
		t.Fatalf("launchctl calls = %v, want load %s", fake.calls, p)
	}
}

func TestEnableAutostartRefusesWhenManualInstanceHeld(t *testing.T) {
	// 迁移语义：pidfile 存在但持有者不是 tradewind（测试用假 pid）→ Stop 拒绝 → on 失败，
	// 且绝不执行 launchctl load（防第二实例撞锁）
	dir := t.TempDir()
	home := paths.Home(t.TempDir())
	pid := os.Getpid() // 测试进程自身：cmdline 无 "-d <home>"、非 start 实例
	if err := os.WriteFile(home.PIDFile(), []byte(itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLaunchctl{}
	exe := filepath.Join(t.TempDir(), "tradewind")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := enableAutostart(home, dir, exe, fake); err == nil {
		t.Fatal("migration with live foreign holder: want error")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("load must not run when migration fails; calls = %v", fake.calls)
	}
}

func TestEnableAutostartProceedsWithStalePidfile(t *testing.T) {
	// stale pidfile（持有者已死）：Stop 自清后迁移继续
	dir := t.TempDir()
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.PIDFile(), []byte("999999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLaunchctl{}
	exe := filepath.Join(t.TempDir(), "tradewind")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := enableAutostart(home, dir, exe, fake); err != nil {
		t.Fatalf("enable with stale pidfile: %v", err)
	}
	if _, err := os.Stat(home.PIDFile()); err == nil {
		t.Fatal("stale pidfile should have been removed by Stop")
	}
}

func TestDisableAutostartUnloadsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	content, _ := launchdPlist(LaunchAgentLabel, "/x/tradewind", "/x/logs")
	p := plistPathFor(dir)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLaunchctl{}
	if err := disableAutostart(dir, fake); err != nil {
		t.Fatalf("disableAutostart: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("plist should be removed")
	}
	if len(fake.calls) != 1 || fake.calls[0][0] != "unload" || fake.calls[0][1] != p {
		t.Fatalf("launchctl calls = %v, want unload %s", fake.calls, p)
	}
	// 幂等：再次 off → no-op
	fake2 := &fakeLaunchctl{}
	if err := disableAutostart(dir, fake2); err != nil {
		t.Fatalf("idempotent off: %v", err)
	}
	if len(fake2.calls) != 0 {
		t.Fatalf("second off must be no-op; calls = %v", fake2.calls)
	}
}

func TestAutostartStatusStates(t *testing.T) {
	dir := t.TempDir()
	// 未安装
	if err := autostartStatus(dir, &fakeLaunchctl{}); err != nil {
		t.Fatalf("status(disabled): %v", err)
	}
	// 已安装未加载
	content, _ := launchdPlist(LaunchAgentLabel, "/x/tradewind", "/x/logs")
	if err := os.WriteFile(plistPathFor(dir), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := autostartStatus(dir, &fakeLaunchctl{}); err != nil {
		t.Fatalf("status(installed): %v", err)
	}
	// 已加载
	if err := autostartStatus(dir, &fakeLaunchctl{listLoaded: true}); err != nil {
		t.Fatalf("status(loaded): %v", err)
	}
}

func TestAutostartInvalidAction(t *testing.T) {
	if err := Autostart(paths.Home(t.TempDir()), "restart"); err == nil {
		t.Fatal("invalid action: want usage error")
	}
}
