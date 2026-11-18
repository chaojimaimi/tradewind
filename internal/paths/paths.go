// Package paths 定位并管理 Tradewind 数据目录。
//
// 默认位于 ~/Library/Application Support/tradewind/，
// 可用 TRADEDWIND_HOME 环境变量整体覆盖（测试与冒烟隔离的关键机制）。
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvHome 是覆盖数据目录的环境变量名。
const EnvHome = "TRADEDWIND_HOME"

// Home 表示一个数据目录根路径。
type Home string

// Default 返回当前生效的数据目录：优先 TRADEDWIND_HOME，否则用户配置目录下的 tradewind/。
func Default() Home {
	if v := os.Getenv(EnvHome); v != "" {
		return Home(v)
	}
	u, err := os.UserConfigDir()
	if err != nil {
		// os.UserConfigDir 仅在无 HOME 等极端环境失败；回退到相对目录保证可用性
		return Home("tradewind-data")
	}
	return Home(filepath.Join(u, "tradewind"))
}

func (h Home) String() string { return string(h) }

// Ensure 创建数据目录及固定子目录，权限 700/700。
// 显式 chmod：MkdirAll 对已存在目录不会修正权限，而目录权限 700 是 unix socket 的真正防线。
func (h Home) Ensure() error {
	for _, dir := range []string{h.String(), h.Bin(), h.ProfilesDir(), h.LogsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("paths: create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("paths: chmod %s: %w", dir, err)
		}
	}
	return nil
}

func (h Home) join(elem ...string) string {
	return filepath.Join(append([]string{h.String()}, elem...)...)
}

func (h Home) Bin() string          { return h.join("bin", "mihomo") }
func (h Home) Sidecar() string      { return h.join("bin", "mihomo.sha256") }
func (h Home) ProfilesDir() string  { return h.join("profiles") }
func (h Home) LogsDir() string      { return h.join("logs") }
func (h Home) IndexFile() string    { return h.join("profiles.yaml") }
func (h Home) SettingsFile() string { return h.join("settings.yaml") }
func (h Home) StateFile() string    { return h.join("state.json") }
func (h Home) SecretFile() string   { return h.join("secret") }
func (h Home) RuntimeFile() string  { return h.join("runtime.yaml") }
func (h Home) SockFile() string     { return h.join("core.sock") }
func (h Home) PIDFile() string      { return h.join("tradewind.pid") }
func (h Home) LogFile() string      { return h.join("logs", "core.log") }
func (h Home) LogFile1() string     { return h.join("logs", "core.log.1") }
func (h Home) GeoIP() string        { return h.join("geoip.metadb") }
func (h Home) GeoSite() string      { return h.join("geosite.dat") }

// ProfileFile 返回某个订阅原文件路径（订阅文件本身只读）。
func (h Home) ProfileFile(file string) string { return h.join("profiles", file) }
