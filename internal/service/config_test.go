package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ConfigFile v3 validate 矩阵（批⑥b §4.1）：每个拒绝条件各一例；旧锚判无效；
// LoadConfigFile 以配置文件所在目录为锚。

// v3Config 生成 anchor/dataDir 词法关系全部合法的 v3 锚（路径不必真实存在——
// validate 是纯词法判断）。execDir 放在锚目录之外。
func v3Config(anchor, dataDir string) ConfigFile {
	execDir := filepath.Join(filepath.Dir(anchor), "exec")
	coreHome := filepath.Join(anchor, "service-data")
	return ConfigFile{
		Layout:      3,
		AllowedUIDs: []int{501},
		DataUID:     501,
		DataDir:     dataDir,
		HelperPath:  HelperPathIn(execDir),
		CoreBin:     CorePathIn(execDir),
		Sidecar:     SidecarPathIn(execDir),
		CoreHome:    coreHome,
		KernelSock:  KernelSockIn(coreHome),
		BrokerSock:  filepath.Join(anchor, "core.sock"),
		CoreLog:     filepath.Join(anchor, "logs", CoreLogName),
		UserRuntime: filepath.Join(dataDir, "runtime.yaml"),
	}
}

func TestValidateV3AcceptsCanonicalLayout(t *testing.T) {
	base := shortTempDir(t)
	anchor := filepath.Join(base, "anchor")
	cfg := v3Config(anchor, filepath.Join(base, "data"))
	if err := cfg.validate(anchor); err != nil {
		t.Fatalf("canonical v3 anchor must pass: %v", err)
	}
}

// TestValidateV3Rejections 逐条拒绝条件（每个 mutate 造一种违规）。
func TestValidateV3Rejections(t *testing.T) {
	base := shortTempDir(t)
	anchor := filepath.Join(base, "anchor")
	dataDir := filepath.Join(base, "data")

	// 九个路径字段的 setter（供共享闸逐字段验证）
	pathFields := map[string]func(*ConfigFile, string){
		"data_dir":     func(c *ConfigFile, v string) { c.DataDir = v },
		"core_bin":     func(c *ConfigFile, v string) { c.CoreBin = v },
		"sidecar":      func(c *ConfigFile, v string) { c.Sidecar = v },
		"core_home":    func(c *ConfigFile, v string) { c.CoreHome = v },
		"kernel_sock":  func(c *ConfigFile, v string) { c.KernelSock = v },
		"broker_sock":  func(c *ConfigFile, v string) { c.BrokerSock = v },
		"core_log":     func(c *ConfigFile, v string) { c.CoreLog = v },
		"user_runtime": func(c *ConfigFile, v string) { c.UserRuntime = v },
		"helper_path":  func(c *ConfigFile, v string) { c.HelperPath = v },
	}
	cases := []struct {
		name   string
		mutate func(*ConfigFile)
		want   string
	}{
		{"missing layout (pre-batch6b anchor)", func(c *ConfigFile) { c.Layout = 0 }, "layout must be 3"},
		{"wrong layout", func(c *ConfigFile) { c.Layout = 2 }, "layout must be 3"},
		{"empty allowed_uids", func(c *ConfigFile) { c.AllowedUIDs = nil }, "missing allowed_uids"},
		{"invalid data_uid", func(c *ConfigFile) { c.DataUID = 0 }, "invalid data_uid"},
		{"empty path field", func(c *ConfigFile) { c.Sidecar = "" }, "empty path field"},
		{"relative path field", func(c *ConfigFile) { c.CoreBin = "relative/mihomo" }, "not absolute"},
		{"unclean path (trailing slash)", func(c *ConfigFile) { c.DataDir = dataDir + "/" }, "not clean"},
		{"unclean path (dot-dot)", func(c *ConfigFile) { c.CoreBin = base + "/x/../exec/bin/mihomo" }, "not clean"},
		{"core_home outside anchor", func(c *ConfigFile) {
			c.CoreHome = filepath.Join(base, "elsewhere", "service-data")
			c.KernelSock = KernelSockIn(c.CoreHome)
		}, "must live inside the anchor dir"},
		{"kernel_sock outside anchor", func(c *ConfigFile) {
			c.KernelSock = filepath.Join(base, "elsewhere", "core.sock")
		}, "must live inside the anchor dir"},
		{"core_log outside anchor", func(c *ConfigFile) {
			c.CoreLog = filepath.Join(base, "elsewhere", "core.log")
		}, "must live inside the anchor dir"},
		{"kernel_sock not derived from core_home", func(c *ConfigFile) {
			c.KernelSock = filepath.Join(anchor, "core.sock")
		}, "kernel_sock must be"},
		{"broker_sock wrong name", func(c *ConfigFile) {
			c.BrokerSock = filepath.Join(anchor, "other.sock")
		}, "broker_sock must be"},
		{"broker_sock inside data_dir", func(c *ConfigFile) {
			c.DataDir = filepath.Dir(anchor) // DataDir = 锚的父目录 → broker sock 词法在其内
		}, "broker_sock must not live inside data_dir"},
		{"broker_sock inside core_home", func(c *ConfigFile) {
			c.CoreHome = anchor // CoreHome == 锚 → anchor/core.sock 词法在其内
			c.KernelSock = KernelSockIn(anchor)
		}, "broker_sock must not live inside core_home"},
		{"user_runtime not data runtime.yaml", func(c *ConfigFile) {
			c.UserRuntime = filepath.Join(dataDir, "other.yaml")
		}, "user_runtime must be"},
		{"core_home inside data_dir", func(c *ConfigFile) {
			c.DataDir = filepath.Join(anchor, "data")
			c.CoreHome = filepath.Join(anchor, "data", "service-data")
			c.KernelSock = KernelSockIn(c.CoreHome)
			c.UserRuntime = filepath.Join(c.DataDir, "runtime.yaml")
		}, "core_home must not live inside data_dir"},
		{"data_dir inside core_home", func(c *ConfigFile) {
			c.DataDir = filepath.Join(c.CoreHome, "data")
			c.UserRuntime = filepath.Join(c.DataDir, "runtime.yaml")
		}, "data_dir must not live inside core_home"},
		{"core_bin inside data_dir", func(c *ConfigFile) {
			c.CoreBin = filepath.Join(dataDir, "bin", "mihomo")
		}, "core_bin must not live inside data_dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := v3Config(anchor, dataDir)
			tc.mutate(&cfg)
			err := cfg.validate(anchor)
			if err == nil {
				t.Fatalf("want rejection (%s), got pass", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	// 九个路径字段共享"非空/绝对/Clean"闸——逐字段各过一遍相对路径形态
	for name, set := range pathFields {
		t.Run("path field "+name, func(t *testing.T) {
			cfg := v3Config(anchor, dataDir)
			set(&cfg, "relative/"+name)
			if err := cfg.validate(anchor); err == nil || !strings.Contains(err.Error(), "not absolute") {
				t.Fatalf("relative %s: err = %v, want not-absolute rejection", name, err)
			}
		})
	}
}

// TestLoadConfigFileRejectsLegacyAnchor 旧锚（无 layout 键）→ LoadConfigFile 判无效
// （CLI 侧读不出 → 走既有"陈旧/无效 → tun reinstall"分支）。
func TestLoadConfigFileRejectsLegacyAnchor(t *testing.T) {
	dir := shortTempDir(t)
	legacy := `{"allowed_uids":[501],"data_dir":"` + dir + `","data_uid":501,` +
		`"core_bin":"` + filepath.Join(dir, "exec", "bin", "mihomo") + `",` +
		`"sidecar":"` + filepath.Join(dir, "exec", "bin", "mihomo.sha256") + `",` +
		`"runtime":"` + filepath.Join(dir, "runtime.yaml") + `",` +
		`"sock":"` + filepath.Join(dir, "core.sock") + `",` +
		`"log":"` + filepath.Join(dir, "logs", "core-service.log") + `"}`
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(filepath.Join(dir, "service.json")); err == nil {
		t.Fatal("anchor without layout key must be rejected (fail-closed)")
	}
}

// TestLoadConfigFileAnchorsAtConfigDir 同一份内容放到另一个目录下，结论随锚目录
// 变化——CoreHome 在 A 内合法、放到 B 下即"不在锚内"被拒。
func TestLoadConfigFileAnchorsAtConfigDir(t *testing.T) {
	base := shortTempDir(t)
	anchorA := filepath.Join(base, "A")
	anchorB := filepath.Join(base, "B")
	for _, d := range []string{anchorA, anchorB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := v3Config(anchorA, filepath.Join(base, "data"))
	if err := writeJSONFile(filepath.Join(anchorA, "service.json"), cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfigFile(filepath.Join(anchorA, "service.json")); err != nil {
		t.Fatalf("anchor A must accept its own layout: %v", err)
	} else if got.CoreHome != cfg.CoreHome {
		t.Fatalf("loaded core_home = %q, want %q", got.CoreHome, cfg.CoreHome)
	}
	// 同一份内容放到 B 下：CoreHome 仍在 A 内 → 不在 B（锚）内 → 拒绝
	if err := writeJSONFile(filepath.Join(anchorB, "service.json"), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(filepath.Join(anchorB, "service.json")); err == nil {
		t.Fatal("the same content under a different anchor dir must be rejected")
	}
}

// TestAnchorLayoutSingleSource 锚目录内三个 root 专属条目的名字只有一份来源：生产
// 常量（DefaultLogsDir / DefaultCoreHome / DefaultBrokerSock）与"由锚目录推导"的三个
// 函数必须逐字相等。purge 删的目录、服务用的目录、Install 建的目录全靠它们同源
// ——任何一处改回手写字面量都会在这里断掉（code review c1 P3）。
func TestAnchorLayoutSingleSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"logs", LogsDirIn(DefaultConfigDir), DefaultLogsDir},
		{"core home", CoreHomeIn(DefaultConfigDir), DefaultCoreHome},
		{"broker sock", BrokerSockIn(DefaultConfigDir), DefaultBrokerSock},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s: derived %q, production constant %q", tc.name, tc.got, tc.want)
		}
	}
}
