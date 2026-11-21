// Package service 实现 Tradewind 特权服务（路线②，批⑥ §3.1，对标 clash-verge-service）：
//
//	root 常驻进程监听 /var/run/tradewind-service.sock（0666），以 root 身份托管
//	mihomo 内核（TUN 需要 root），对外暴露行 JSON 协议（start/stop/status）。
//
// 安全模型：
//   - 唯一认证闸 = getpeereid uid 白名单（service.json 锚定）——白名单外 uid 能
//     connect 但会被即刻断连；
//   - 请求零路径：协议仅 {action,generation}，所有路径来自 service.json（协议层
//     DisallowUnknownFields 拒收未知字段），CLI 侧携带路径的注入面为零；
//   - 所有权仲裁 = generation 单调递增（start 须 > currentGeneration）+ start 幂等
//     no-op（core 已运行不重复 spawn）。
package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// 生产环境锚定路径（helper 安装器写入；测试全部经 ServerConfig/Client 注入短路径，
// F19：macOS unix socket sun_path 104 字符上限）。
const (
	// DefaultSockPath 是服务 unix socket（0666——白名单外的 uid 能 connect 但被
	// getpeereid 即刻拒绝，白名单是唯一认证闸）。
	DefaultSockPath = "/var/run/tradewind-service.sock"
	// DefaultConfigDir 是 root-owned 安装锚目录（plist 同级的 service.json/core.pid）。
	DefaultConfigDir = "/Library/Application Support/Tradewind"
	// DefaultConfigPath 是 service.json（0644 root-owned，服务启动时只读校验）。
	DefaultConfigPath = DefaultConfigDir + "/service.json"
	// DefaultCorePIDPath 是服务自管的 root core pid 记录（孤儿收编判据）。
	DefaultCorePIDPath = DefaultConfigDir + "/core.pid"
	// DaemonLabel 是 launchd daemon 标识（对齐 agent 命名空间 io.github.chaojimaimi.*）。
	DaemonLabel = "io.github.chaojimaimi.tradewind.service"
	// HelperInstallPath 是提权拷贝后的 helper 落点（root:wheel 0755）。
	HelperInstallPath = "/Library/PrivilegedHelperTools/" + DaemonLabel
	// DaemonPlistPath 是 LaunchDaemon plist 落点。
	DaemonPlistPath = "/Library/LaunchDaemons/" + DaemonLabel + ".plist"
	// DaemonLogPath 是服务自身 stdout/stderr（plist StandardOutPath）。
	DaemonLogPath = "/Library/Logs/tradewind-service.log"
)

// 服务请求动作（协议仅此三个；未知 action 协议层拒绝）。
const (
	ActionStart  = "start"
	ActionStop   = "stop"
	ActionStatus = "status"
)

// Request 是特权服务行 JSON 请求：{action, generation}。
// 请求零路径（批⑥ §3.1）：协议层 DisallowUnknownFields——CLI 侧请求携带任何路径
// 一律拒收，注入面为零。
type Request struct {
	Action     string `json:"action"`
	Generation int    `json:"generation,omitempty"`
}

// Response 是特权服务行 JSON 响应：{ok, error?, pid?, version?, already_running?}。
type Response struct {
	OK             bool   `json:"ok"`
	Error          string `json:"error,omitempty"`
	PID            int    `json:"pid,omitempty"`
	Version        string `json:"version,omitempty"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
}

// DecodeRequest 严格解码一行请求：未知字段拒收 + 行内不允许尾随数据。
func DecodeRequest(line []byte) (Request, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("service: bad request: %w", err)
	}
	// 单行协议：一条 JSON 之外不得有尾随 token（防 smuggling）
	if dec.More() {
		return Request{}, errors.New("service: bad request: trailing data after JSON object")
	}
	switch req.Action {
	case ActionStart, ActionStop, ActionStatus:
	default:
		return Request{}, fmt.Errorf("service: unknown action %q", req.Action)
	}
	return req, nil
}

// ConfigFile 是 service.json（root-owned 0644 安装锚）：服务启动时只读校验，
// 非法/缺失 → exit 1（launchd 节流重试，R3）。全部路径由此锚定——服务运行期
// 不接受任何请求侧路径。
type ConfigFile struct {
	AllowedUIDs []int  `json:"allowed_uids"`
	DataDir     string `json:"data_dir"`
	CoreBin     string `json:"core_bin"`
	Sidecar     string `json:"sidecar"`
	Runtime     string `json:"runtime"`
	Sock        string `json:"sock"`
	Log         string `json:"log"`
	DataUID     int    `json:"data_uid"`
}

// validate 锚完整性校验（服务启动闸——缺字段宁可 exit 1 也不带病运行）。
func (c ConfigFile) validate() error {
	if len(c.AllowedUIDs) == 0 {
		return errors.New("service: service.json missing allowed_uids")
	}
	for _, field := range []string{c.DataDir, c.CoreBin, c.Sidecar, c.Runtime, c.Sock, c.Log} {
		if field == "" {
			return errors.New("service: service.json has empty path field")
		}
	}
	if c.DataUID <= 0 {
		return errors.New("service: service.json has invalid data_uid")
	}
	return nil
}

// LoadConfigFile 读取并校验 service.json。
func LoadConfigFile(path string) (ConfigFile, error) {
	var cfg ConfigFile
	if err := ReadJSONFile(path, &cfg); err != nil {
		return ConfigFile{}, err
	}
	if err := cfg.validate(); err != nil {
		return ConfigFile{}, err
	}
	return cfg, nil
}

// Client 是特权服务的行 JSON 客户端（app 编排侧消费；sock 路径/超时可注入，
// 测试用 /tmp 短路径 + 桩服务，F19）。
type Client struct {
	SockPath string
	Timeout  time.Duration
}

// NewClient 构造客户端（timeout 覆盖拨号+写+读全程；start 侧调用方须覆盖服务端
// 同步握手 15s 预算，建议 30s）。
func NewClient(sockPath string, timeout time.Duration) *Client {
	return &Client{SockPath: sockPath, Timeout: timeout}
}

// Start 请求服务启动 root core（generation 仲裁 + 同步握手——服务端 spawn+握手
// 完成才返回；core 已运行时为幂等 no-op，already_running=true）。
func (c *Client) Start(generation int) (Response, error) {
	return c.Do(Request{Action: ActionStart, Generation: generation})
}

// Stop 请求服务停止 root core（未运行时为 no-op；无 generation 校验语义）。
func (c *Client) Stop() (Response, error) {
	return c.Do(Request{Action: ActionStop})
}

// Status 查询服务托管的 root core（pid/version；未运行 pid=0）。
func (c *Client) Status() (Response, error) {
	return c.Do(Request{Action: ActionStatus})
}

// Do 执行单次请求：连接 → 写一行 → 读一行 → 解码。
func (c *Client) Do(req Request) (Response, error) {
	var resp Response
	conn, err := DialUnix(c.SockPath, c.Timeout)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	deadline := time.Now().Add(c.Timeout)
	_ = conn.SetDeadline(deadline)
	line, err := json.Marshal(req)
	if err != nil {
		return resp, fmt.Errorf("service: marshal request: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return resp, fmt.Errorf("service: write request: %w", err)
	}
	return ReadResponse(conn)
}

// DialUnix 连接服务 socket（超时覆盖拨号；rawRequest 测试缝与 Do 共用）。
func DialUnix(sockPath string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", sockPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("service: dial %s: %w", sockPath, err)
	}
	return conn, nil
}

// ReadResponse 读取并解码一行应答。
func ReadResponse(conn net.Conn) (Response, error) {
	var resp Response
	respLine, err := bufio.NewReaderSize(conn, 4096).ReadString('\n')
	if err != nil {
		return resp, fmt.Errorf("service: read response: %w", err)
	}
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		return resp, fmt.Errorf("service: parse response: %w", err)
	}
	return resp, nil
}

// ReadJSONFile 是包内 service.json 读取（错误带路径上下文）。
func ReadJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("service: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("service: parse %s: %w", path, err)
	}
	return nil
}
