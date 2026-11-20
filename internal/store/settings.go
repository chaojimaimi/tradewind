package store

import (
	"errors"
	"fmt"
	"os"
	"regexp"

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
	MixedPort    int  `yaml:"mixed_port" json:"mixed_port"`
	GatewayPort  int  `yaml:"gateway_port" json:"gateway_port"`
	PanelEnabled bool `yaml:"panel_enabled" json:"panel_enabled"`
	// AllowLan 是否向局域网暴露代理端口（默认 false 仅监听回环；synth 注入 allow-lan 键）
	AllowLan bool   `yaml:"allow_lan" json:"allow_lan"`
	Mode     string `yaml:"mode" json:"mode"`
	LogLevel string `yaml:"log_level" json:"log_level"`
	// LogRetentionDays 滚动日志保留天数；0 视为未设置 → 归一为默认 14
	LogRetentionDays int `yaml:"log_retention_days,omitempty" json:"log_retention_days,omitempty"`
	// UpdateViaProxy 订阅拉取是否经自身代理（nil=未设置 → 默认 true）。
	// WHY 指针：bool 零值 false 无法区分"显式关闭"与"旧版 settings 缺键"，
	// 而该功能默认开启（M2-plan §2.2），误降级会改变拉取路径。
	UpdateViaProxy *bool `yaml:"update_via_proxy,omitempty" json:"update_via_proxy,omitempty"`
	// PacMode 系统代理写入形态：false=手工三项代理（legacy 语义），true=PAC 自动
	// 代理（指向面板网关 /pac）。缺省 false=旧 settings.yaml 零回归。
	PacMode bool `yaml:"pac_mode,omitempty" json:"pac_mode,omitempty"`
	// BypassDomains 绕过域名列表（nil/空=未设置 → 生效值取 sysproxy 缺省集）。
	// 校验（白名单正则+条数上限）下沉 Validate：手改 settings.yaml 经 LoadSettings
	// 是唯一闸门，PAC JS 注入面在 /pac 合成前被拦（批④ §3.1）。
	BypassDomains []string `yaml:"bypass_domains,omitempty" json:"bypass_domains,omitempty"`
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
	// 跨字段互斥（M3-plan §11.2-5）：mixed_port 与 gateway_port 同端口会在监听上冲突；
	// 落位 Validate 使手改 YAML 也被 LoadSettings 拦截，CLI 校验免费获得
	if s.MixedPort == s.GatewayPort {
		return fmt.Errorf("store: mixed_port and gateway_port must differ (both %d)", s.MixedPort)
	}
	// bypass 白名单（批④ §3.1 评审 P2-2/P2-4）：每条 pattern 仅允许域名/通配/
	// CIDR/<local> 字符集，引号/分号/空格/& 全拒（PAC JS 注入面），最多 64 条。
	// 层次与 mixed_port 互斥同先例：只挂 CLI 层则手改 YAML 可直达 /pac 注入。
	if len(s.BypassDomains) > MaxBypassDomains {
		return fmt.Errorf("store: bypass_domains has %d entries, max %d",
			len(s.BypassDomains), MaxBypassDomains)
	}
	for _, d := range s.BypassDomains {
		if !bypassPatternRe.MatchString(d) {
			return fmt.Errorf("store: invalid bypass_domains entry %q", d)
		}
	}
	return nil
}

// MaxBypassDomains 是 bypass_domains 的条数上限（壳 TextEditor 大输入防线）。
const MaxBypassDomains = 64

// bypassPatternRe 是单条 bypass pattern 的白名单正则：域名/点分通配/CIDR（如
// 169.254/16）/<local> 字符集；长度 1-253（域名上限）。引号、分号、空格、&、
// 换行等一切 JS 语法字符不在集内。
var bypassPatternRe = regexp.MustCompile(`^[A-Za-z0-9._*?<>/-]{1,253}$`)

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
