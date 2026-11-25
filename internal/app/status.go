package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/panel"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// healthProbeTimeout 是 status 探测内核 /version 的超时。
const healthProbeTimeout = 2 * time.Second

// Report 是 status 概览。Zero 值安全。
type Report struct {
	Desired     string
	PID         int
	CoreVersion string
	CoreHealthy bool
	SysProxy    string
	// SysProxyApplied/Services 供 --json 结构化输出（人读版见 SysProxy 字符串）
	SysProxyApplied  bool
	SysProxyServices int
	// 会话形态与守护事件（批④ §2.1）：Form 空=legacy manual；HijackEvents/LastRestore
	// 是代理守护的抢改事件计数与最近还原时间戳（壳 toast 数据源）
	SysProxyForm         string
	SysProxyHijackEvents int
	SysProxyLastRestore  int64
	Profile              string
	Traffic              string
}

// ExitCode 供脚本判断（M0-plan §4）：0 正常 / 1 内核异常（desired=running 但不可达）。
func (r Report) ExitCode() int {
	if r.Desired == store.DesiredRunning && !r.CoreHealthy {
		return 1
	}
	return 0
}

// Status 汇总内核 / 系统代理 / 当前订阅概览。只读，不改任何状态。
func Status(home paths.Home) (Report, error) {
	st, err := store.LoadState(home)
	if err != nil {
		return Report{}, err
	}
	r := Report{
		Desired:              st.Core.Desired,
		PID:                  st.Core.PID,
		SysProxy:             map[bool]string{true: "applied", false: "not applied"}[st.SysProxy.Applied],
		SysProxyApplied:      st.SysProxy.Applied,
		SysProxyServices:     len(st.SysProxy.Services),
		SysProxyForm:         st.SysProxy.Form,
		SysProxyHijackEvents: st.SysProxy.HijackEvents,
		SysProxyLastRestore:  st.SysProxy.LastRestore,
	}
	if st.SysProxy.Applied {
		r.SysProxy = fmt.Sprintf("applied (%d services backed up)", len(st.SysProxy.Services))
	}
	r.CoreVersion, r.CoreHealthy = probeCore(home)
	r.Profile, r.Traffic = currentProfile(home)
	return r, nil
}

// --- status --json（壳控制面唯一状态源，M2-plan §2.5）---

// statusCoreJSON 是 JSON 输出的 core 段。
type statusCoreJSON struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

// statusSysProxyJSON 是 JSON 输出的 sys_proxy 段。
// Form/HijackEvents/LastRestore 为批④新增（omitempty 对旧 state.json 零值缺键——
// 壳侧解码模型按可选处理，版本偏斜兼容）。
type statusSysProxyJSON struct {
	Applied      bool   `json:"applied"`
	Services     int    `json:"services"`
	Form         string `json:"form,omitempty"`
	HijackEvents int    `json:"hijack_events,omitempty"`
	LastRestore  int64  `json:"last_restore,omitempty"`
}

// statusProfileJSON 是 JSON 输出的 profile 段。
type statusProfileJSON struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// statusTunJSON 是 JSON 输出的 tun 段（批⑥ §4 三态数据源：enabled+running /
// enabled+服务异常 / disabled；popover TUN 行与设置页开关初值）。旧 CLI 缺键 →
// 壳侧解码模型按 nil 处理（version 偏斜兼容，Form/HijackEvents 同先例）。
type statusTunJSON struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
}

// StatusJSON 是 `status --json` 的顶层结构：mode 提顶层，panel_url 是壳
// 数据面端口与 token 的唯一来源（免 YAML 解析）。降级时 panel_url 为空串，
// 壳据此显示"面板数据不可用"（M2-plan §5）。
type StatusJSON struct {
	Core     statusCoreJSON     `json:"core"`
	Mode     string             `json:"mode"`
	SysProxy statusSysProxyJSON `json:"sys_proxy"`
	Tun      statusTunJSON      `json:"tun"`
	Profile  statusProfileJSON  `json:"profile"`
	PanelURL string             `json:"panel_url"`
}

// StatusReport 组装机器可读状态。只读：panel 凭据走只读访问器，
// 绝不因 status 而生成 token 文件；panel 不可用 → panel_url=""（非错误）。
func StatusReport(home paths.Home, r Report) (StatusJSON, error) {
	mode, err := currentMode(home)
	if err != nil {
		return StatusJSON{}, err
	}
	out := StatusJSON{
		Core: statusCoreJSON{Running: r.CoreHealthy, PID: r.PID, Version: r.CoreVersion},
		Mode: mode,
		SysProxy: statusSysProxyJSON{
			Applied: r.SysProxyApplied, Services: r.SysProxyServices,
			Form: r.SysProxyForm, HijackEvents: r.SysProxyHijackEvents,
			LastRestore: r.SysProxyLastRestore,
		},
	}
	// tun 三态（批⑥ §4）：Running 以 core.sock 健康为据（TUN 形态下即服务托管
	// root core；settings 说开但 sock 不可达 = 服务异常态，壳据此提示）
	if settings, err := store.LoadSettings(home); err == nil {
		out.Tun = statusTunJSON{Enabled: settings.TunEnabled, Running: settings.TunEnabled && r.CoreHealthy}
	}
	if idx, err := store.LoadIndex(home); err == nil {
		if item, ok := idx.CurrentItem(); ok {
			out.Profile = statusProfileJSON{UID: item.UID, Name: item.Name}
		}
	}
	out.PanelURL = panelURLIfReachable(home)
	return out, nil
}

// panelURLIfReachable 网关可达时返回入口 URL（含端口与 token），否则空串。
// WHY 静默降级：status 的职责是如实上报，panel 关闭不是 status 的错误。
func panelURLIfReachable(home paths.Home) string {
	settings, err := store.LoadSettings(home)
	if err != nil || !settings.PanelEnabled || !uiDirReady(home) {
		return ""
	}
	if !gatewayReachable(settings.GatewayPort) {
		return ""
	}
	token := panel.ReadPanelToken(home)
	if token == "" {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/ui/%s/", settings.GatewayPort, token)
}

// MarshalStatusJSON 序列化（缩进关闭，壳按 stdout 单行 JSON 解析）。
func MarshalStatusJSON(out StatusJSON) (string, error) {
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("app: marshal status json: %w", err)
	}
	return string(data), nil
}

// probeCore 经 unix socket 探测内核健康；sock 不存在或请求失败即不健康。
// 路径按当前会话形态分流（批⑥b §4.3：TUN 形态拨服务的代理入口）。
func probeCore(home paths.Home) (version string, healthy bool) {
	sock := coreSockPath(home)
	if _, err := os.Stat(sock); err != nil {
		return "", false
	}
	client := core.NewClient(sock, store.ReadSecret(home))
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	v, err := client.Version(ctx)
	if err != nil {
		return "", false
	}
	return v, true
}

// currentProfile 返回当前订阅与流量摘要行。
func currentProfile(home paths.Home) (profile, traffic string) {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return "(index unreadable)", ""
	}
	item, ok := idx.CurrentItem()
	if !ok {
		return "(none)", ""
	}
	profile = fmt.Sprintf("%s (uid %s, %s, updated %s)",
		item.Name, item.UID, item.Type, time.Unix(item.Updated, 0).Format("2006-01-02 15:04"))
	if item.Extra.Total > 0 || item.Extra.Expire > 0 {
		traffic = fmt.Sprintf("up %s / down %s / total %s",
			humanBytes(item.Extra.Upload), humanBytes(item.Extra.Download), humanBytes(item.Extra.Total))
		if item.Extra.Expire > 0 {
			traffic += ", expire " + time.Unix(item.Extra.Expire, 0).Format("2006-01-02")
		}
	}
	return profile, traffic
}

// Print 输出 status 概览。
func (r Report) Print() {
	if r.CoreHealthy {
		fmt.Printf("core:      running (pid %d, %s)\n", r.PID, r.CoreVersion)
	} else {
		fmt.Printf("core:      not reachable (desired=%s)\n", r.Desired)
	}
	fmt.Printf("sys_proxy: %s\n", r.SysProxy)
	fmt.Printf("profile:   %s\n", r.Profile)
	if r.Traffic != "" {
		fmt.Printf("traffic:   %s\n", r.Traffic)
	}
}

// humanBytes 人类可读字节数。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
