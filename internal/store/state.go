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

// ServiceBackup 是单个网络服务的代理全量备份（写入前快照）。
type ServiceBackup struct {
	Service string     `json:"service"`
	Web     ProxyState `json:"web"`
	Secure  ProxyState `json:"secure"`
	Socks   ProxyState `json:"socks"`
	Bypass  []string   `json:"bypass"`
}

// SysProxyState 记录系统代理是否由我们写入 + 各服务备份。
// applied=false 即视为无残留——启动清理的唯一判据。
type SysProxyState struct {
	Applied  bool            `json:"applied"`
	Services []ServiceBackup `json:"services"`
}

// CoreState 记录内核期望状态与最近已知 pid（孤儿内核检测用）。
type CoreState struct {
	Desired string `json:"desired"`
	PID     int    `json:"pid"`
}

// State 是 state.json 的顶层结构：内核期望状态 + 系统代理备份。
type State struct {
	Core     CoreState     `json:"core"`
	SysProxy SysProxyState `json:"sys_proxy"`
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
