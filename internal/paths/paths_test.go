package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultUsesEnvOverride(t *testing.T) {
	t.Setenv(EnvHome, "/tmp/tw-custom-home")
	got := Default()
	if got != Home("/tmp/tw-custom-home") {
		t.Fatalf("Default() = %q, want TRADEDWIND_HOME value", got)
	}
}

func TestDefaultFallsBackToUserConfigDir(t *testing.T) {
	t.Setenv(EnvHome, "")
	// 用户配置目录随环境变化，这里只断言目录名是 tradewind
	if filepath.Base(string(Default())) != "tradewind" {
		t.Fatalf("Default() base = %q, want tradewind", filepath.Base(string(Default())))
	}
}

func TestEnsureCreatesDirs700(t *testing.T) {
	home := Home(t.TempDir())
	if err := home.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 模拟首启时目录权限被外部改坏，Ensure 必须收敛回 700（unix socket 的真正防线）
	if err := os.Chmod(home.String(), 0o755); err != nil {
		t.Fatalf("chmod setup: %v", err)
	}
	if err := home.Ensure(); err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	for _, dir := range []string{home.String(), home.BinDir(), home.ProfilesDir(), home.LogsDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s perm = %o, want 700", dir, perm)
		}
	}
}

func TestSubpaths(t *testing.T) {
	home := Home("/data")
	cases := map[string]func() string{
		"bin":               home.BinDir,
		"bin/mihomo":        home.Bin,
		"bin/mihomo.sha256": home.Sidecar,
		"profiles.yaml":     home.IndexFile,
		"settings.yaml":     home.SettingsFile,
		"state.json":        home.StateFile,
		"secret":            home.SecretFile,
		"runtime.yaml":      home.RuntimeFile,
		"core.sock":         home.SockFile,
		"tradewind.pid":     home.PIDFile,
		"logs/core.log":     home.LogFile,
		"logs/core.log.1":   home.LogFile1,
		"geoip.metadb":      home.GeoIP,
		"geosite.dat":       home.GeoSite,
		"profiles/a1.yaml":  func() string { return home.ProfileFile("a1.yaml") },
	}
	// 文件布局锁定（M0-plan §3），路径错误会破坏 SAFE_PATHS 与恢复语义
	for want, fn := range cases {
		if got := fn(); got != filepath.Join("/data", want) {
			t.Fatalf("path = %q, want %q", got, want)
		}
	}
}
