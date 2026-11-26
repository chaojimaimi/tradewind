package store

import (
	"errors"
	"os"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// DesiredRunning / DesiredStopped 是内核期望状态。
// desired 是"我们想要内核处于什么状态"，与实际进程状态解耦——
// 启动期依据 desired 检测 kill -9 之类的非正常退出残留（DESIGN §7.2）。
const (
	DesiredRunning  = "running"
	DesiredStopped  = "stopped"
	DesiredStopping = "stopping"
)

// ProxyState 是单个网络服务上单项代理（web/secure/socks）的完整状态。
// 精确还原（而非简单关闭）依赖 enabled/server/port 三元组。
type ProxyState struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Port    int    `json:"port"`
}

// AutoProxyState 是单个网络服务上自动代理（PAC）的备份态。
// URL 空 + Enabled 假 = 零值（机器未配置自动代理的常态；-getautoproxyurl 的
// (null) 哨兵归一为零值 URL）。
type AutoProxyState struct {
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// ServiceBackup 是单个网络服务的代理全量备份（写入前快照）。
// AutoProxy 为批④新增：旧 state.json 缺键 → 零值（ReadJSON 语义），Restore 据此零扰动。
type ServiceBackup struct {
	Service   string         `json:"service"`
	Web       ProxyState     `json:"web"`
	Secure    ProxyState     `json:"secure"`
	Socks     ProxyState     `json:"socks"`
	Bypass    []string       `json:"bypass"`
	AutoProxy AutoProxyState `json:"auto_proxy"`
}

// SysProxyState 记录系统代理是否由我们写入 + 各服务备份。
// applied=false 即视为无残留——启动清理的唯一判据。
// Form 是本会话写入形态（"manual"/"pac"；空=legacy 视为 manual，Restore 决策表
// 按 Form 分流）。HijackEvents/LastRestore 是代理守护（批④ D1）的抢改事件计数
// 与最近还原时间戳——零值兼容旧 state.json。
type SysProxyState struct {
	Applied      bool            `json:"applied"`
	Services     []ServiceBackup `json:"services"`
	Form         string          `json:"form,omitempty"`
	HijackEvents int             `json:"hijack_events,omitempty"`
	LastRestore  int64           `json:"last_restore,omitempty"`
}

// CoreState 记录内核期望状态与最近已知 pid（孤儿内核检测用）。
// Generation 每次 start 自增：防 KeepAlive 快速循环下新旧实例的 state 覆写交错
// （关停路径检测到 generation 与自身不符 → 放弃 state 写入与系统代理还原，
// 所有权归新实例，M2-plan §2.3）。
// Tun 是本实例内核形态（批⑥ §3.4）：true=root 特权服务托管——cleanupResidues/
// degradeRestart/waitLoop 分流统一读它判形态（root pid 用户态 SIGTERM=EPERM，
// 必须走服务请求）。写入点钉死：postHandshake 全量重建 CoreState 处显式写
// Tun=settings.TunEnabled；markStopped 全量重建清零=有意语义（stopped 无形态残留）。
type CoreState struct {
	Desired    string `json:"desired"`
	PID        int    `json:"pid"`
	Generation int    `json:"generation"`
	Tun        bool   `json:"tun,omitempty"`
}

// State 是 state.json 的顶层结构：内核期望状态 + 系统代理备份。
type State struct {
	Core     CoreState     `json:"core"`
	SysProxy SysProxyState `json:"sys_proxy"`
	// LastForm 是最近一次成功启动的会话形态（"tun" 或 "user"；批⑥b §4.8）。
	// WHY 放顶层而不放进 core 对象：markStopped 会整体重建 core，放进去会被清掉；
	// 放顶层则跨停机、跨重启都在。旧文件无此键 = 空 = 首次启动。
	LastForm string `json:"last_form,omitempty"`
}

// DefaultState 返回"无残留"的初始状态。
func DefaultState() State {
	return State{
		Core:     CoreState{Desired: DesiredStopped, PID: 0},
		SysProxy: SysProxyState{Applied: false},
	}
}

// LoadState 读取 state.json；文件不存在时返回默认状态。
func LoadState(h paths.Home) (State, error) {
	var st State
	if err := ReadJSON(h.StateFile(), &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultState(), nil
		}
		return State{}, err
	}
	return st, nil
}

// SaveState 原子写入 state.json。
func SaveState(h paths.Home, st State) error {
	return WriteJSON(h.StateFile(), st)
}
