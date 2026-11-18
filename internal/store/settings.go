package store

import (
	"errors"
	"fmt"
	"os"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// 应用设置的默认值（与内置模板保持一致；settings 覆盖一切）。
const (
	DefaultMixedPort   = 7897
	DefaultGatewayPort = 39097 // 面板网关：避开 Verge 9097 控制器与 7895-7899 代理段（M1-plan §4.1）
	DefaultMode        = "rule"
	DefaultLogLevel    = "info"
	// DefaultLogRetentionDays 滚动日志按天清理的保留天数（demo 设置页同值，M2-plan §2.4）
	DefaultLogRetentionDays = 14
)

// Settings 是 settings.yaml：应用级端口/模式/日志级别。
type Settings struct {
	MixedPort    int    `yaml:"mixed_port" json:"mixed_port"`
	GatewayPort  int    `yaml:"gateway_port" json:"gateway_port"`
	PanelEnabled bool   `yaml:"panel_enabled" json:"panel_enabled"`
	Mode         string `yaml:"mode" json:"mode"`
	LogLevel     string `yaml:"log_level" json:"log_level"`
	// LogRetentionDays 滚动日志保留天数；0 视为未设置 → 归一为默认 14
	LogRetentionDays int `yaml:"log_retention_days,omitempty" json:"log_retention_days,omitempty"`
	// UpdateViaProxy 订阅拉取是否经自身代理（nil=未设置 → 默认 true）。
	// WHY 指针：bool 零值 false 无法区分"显式关闭"与"旧版 settings 缺键"，
	// 而该功能默认开启（M2-plan §2.2），误降级会改变拉取路径。
	UpdateViaProxy *bool `yaml:"update_via_proxy,omitempty" json:"update_via_proxy,omitempty"`
}

// UpdateViaProxyEnabled 返回订阅拉取经自身代理的生效值（nil → true）。
func (s Settings) UpdateViaProxyEnabled() bool {
	return s.UpdateViaProxy == nil || *s.UpdateViaProxy
}

// DefaultSettings 返回默认设置。
func DefaultSettings() Settings {
	return Settings{
		MixedPort:        DefaultMixedPort,
		GatewayPort:      DefaultGatewayPort,
		PanelEnabled:     true,
		Mode:             DefaultMode,
		LogLevel:         DefaultLogLevel,
		LogRetentionDays: DefaultLogRetentionDays,
	}
}

// Validate 校验设置取值范围，防止非法值进入 runtime.yaml 导致内核拒绝启动。
func (s Settings) Validate() error {
	if s.MixedPort < 1 || s.MixedPort > 65535 {
		return fmt.Errorf("store: invalid mixed_port %d", s.MixedPort)
	}
	if s.GatewayPort < 1 || s.GatewayPort > 65535 {
		return fmt.Errorf("store: invalid gateway_port %d", s.GatewayPort)
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
	// 0 = 未设置（LoadSettings 归一为默认 14），直接构造的 Settings 允许 0；
	// 显式负值/超 10 年才拒绝
	if s.LogRetentionDays < 0 || s.LogRetentionDays > 3650 {
		return fmt.Errorf("store: invalid log_retention_days %d", s.LogRetentionDays)
	}
	return nil
}

// LoadSettings 读取设置；文件不存在时返回默认值。
// 旧版 settings.yaml 缺 gateway_port（或显式 0）→ 归一为默认 39097（M1-plan §4.1）；
// 缺 log_retention_days → 归一为默认 14（M2-plan §2.4）。
func LoadSettings(h paths.Home) (Settings, error) {
	s := DefaultSettings()
	if err := ReadYAML(h.SettingsFile(), &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultSettings(), nil
		}
		return Settings{}, err
	}
	if s.GatewayPort == 0 {
		s.GatewayPort = DefaultGatewayPort
	}
	if s.LogRetentionDays == 0 {
		s.LogRetentionDays = DefaultLogRetentionDays
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
