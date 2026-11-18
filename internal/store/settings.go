package store

import (
	"errors"
	"fmt"
	"os"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// 应用设置的默认值（与内置模板保持一致；settings 覆盖一切）。
const (
	DefaultMixedPort = 7897
	DefaultMode      = "rule"
	DefaultLogLevel  = "info"
)

// Settings 是 settings.yaml：应用级端口/模式/日志级别。
type Settings struct {
	MixedPort int    `yaml:"mixed_port" json:"mixed_port"`
	Mode      string `yaml:"mode" json:"mode"`
	LogLevel  string `yaml:"log_level" json:"log_level"`
}

// DefaultSettings 返回默认设置。
func DefaultSettings() Settings {
	return Settings{MixedPort: DefaultMixedPort, Mode: DefaultMode, LogLevel: DefaultLogLevel}
}

// Validate 校验设置取值范围，防止非法值进入 runtime.yaml 导致内核拒绝启动。
func (s Settings) Validate() error {
	if s.MixedPort < 1 || s.MixedPort > 65535 {
		return fmt.Errorf("store: invalid mixed_port %d", s.MixedPort)
	}
	switch s.Mode {
	case "rule", "global", "direct":
	default:
		return fmt.Errorf("store: invalid mode %q", s.Mode)
	}
	switch s.LogLevel {
	case "silent", "error", "warning", "info", "debug":
	default:
		return fmt.Errorf("store: invalid log_level %q", s.LogLevel)
	}
	return nil
}

// LoadSettings 读取设置；文件不存在时返回默认值。
func LoadSettings(h paths.Home) (Settings, error) {
	s := DefaultSettings()
	if err := ReadYAML(h.SettingsFile(), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultSettings(), nil
		}
		return Settings{}, err
	}
	if err := s.Validate(); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// SaveSettings 原子写入设置。
func SaveSettings(h paths.Home, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return WriteYAML(h.SettingsFile(), s)
}
