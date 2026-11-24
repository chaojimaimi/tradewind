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
//     no-op（core 已运行不重复 spawn）；
//   - 内核工作目录 = root 属主的 <core_home>（锚目录内 service-data，0700）——内核
//     的一切产物（runtime.yaml、core.sock、cache.db、geo、日志之外的写点）都落在
//     root 独占可写目录里，用户域无从植入（批⑥b §3 I1/I2）；
//   - 服务对用户数据目录只有"经安全打开读取"一种操作（safeopen：O_NOFOLLOW +
//     fstat 普通文件 + 属主 = data_uid + 大小上限），零写入、零 chown（批⑥b I1）。
package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// HelperInstallPath 是旧布局的 helper 落点——降为「遗留位置」（batch6 真机首测：
	// Gatekeeper/AppleSystemPolicy 对 /Library/PrivilegedHelperTools 做执行评估，
	// ad-hoc 签名二进制被 SIGKILL，helper install 根本没启动）。不再落位；安装与
	// 卸载脚本均清理它。标识符保留原名以免无谓改名。
	HelperInstallPath = "/Library/PrivilegedHelperTools/" + DaemonLabel
	// HelperFileName 是执行目录内的 helper 副本文件名。
	HelperFileName = "tradewind-service"
	// DaemonPlistPath 是 LaunchDaemon plist 落点。
	DaemonPlistPath = "/Library/LaunchDaemons/" + DaemonLabel + ".plist"
	// 锚目录内三个 root 专属条目的名字（批⑥b §4.1）。名字只在这里定义一次：生产
	// 常量、validate 的派生式、Install 的落点、purge 的删除目标全部由它们推导——
	// "purge 删的目录 == 服务用的目录 == Install 建的目录"因此是同一份事实，而不是
	// 几处手写字面量碰巧相等（config_test.go 钉死）。
	logsDirName    = "logs"
	coreHomeName   = "service-data"
	brokerSockName = "core.sock"
	// DefaultLogsDir 是锚目录内的日志目录（root 属主 0755；批⑥b §4.1——服务与内核
	// 日志迁出 admin 可写的 /Library/Logs，R22）。目录归 root，两份日志文件归
	// data_uid 0600（Install 预创建，见 install.go）。
	DefaultLogsDir = DefaultConfigDir + "/" + logsDirName
	// DefaultCoreHome 是 root 专属内核工作目录（内核 -d；root:wheel 0700，用户域
	// 不可进入——内核一切产物都落在这里，批⑥b §3 I2）。
	DefaultCoreHome = DefaultConfigDir + "/" + coreHomeName
	// DefaultBrokerSock 是用户域 API 入口（代理入口，§4.3；c1 只写入并校验该字段，
	// 监听在 c4）。
	DefaultBrokerSock = DefaultConfigDir + "/" + brokerSockName
	// ServiceLogName / CoreLogName 是 <logs> 目录内两份日志的文件名（属主 data_uid
	// 0600；目录 root 属主——用户无法替换目录项，只能改自己那份日志的内容）。
	ServiceLogName = "service.log"
	CoreLogName    = "core.log"
)

// LogsDirIn / CoreHomeIn / BrokerSockIn 由锚目录推导三个 root 专属条目的路径
// （与上面的生产常量同源；测试的临时锚目录同样成立）。
func LogsDirIn(anchorDir string) string    { return filepath.Join(anchorDir, logsDirName) }
func CoreHomeIn(anchorDir string) string   { return filepath.Join(anchorDir, coreHomeName) }
func BrokerSockIn(anchorDir string) string { return filepath.Join(anchorDir, brokerSockName) }

// KernelSockIn 返回 <core_home> 内的内核原生 API socket 路径（仅 root 服务可达；
// 用户域只经代理访问）。
func KernelSockIn(coreHome string) string { return filepath.Join(coreHome, "core.sock") }

// RuntimeIn 返回 <core_home> 内的内核 -f 配置路径（服务写入，内核只读）。
func RuntimeIn(coreHome string) string { return filepath.Join(coreHome, "runtime.yaml") }

// LibexecDir 是第二候选的父目录（可能由安装脚本新建；卸载时仅在空时回收——
// /opt 等既有系统目录绝不回收）。
const LibexecDir = "/usr/local/libexec"

// ExecDirCandidates 返回执行目录候选（按序探测，首个通过执行自测者选定；返回
// 副本防调用方改动）。真机实证 Gatekeeper/ASP 对 /Library/PrivilegedHelperTools
// 拦截 ad-hoc 二进制，而同一二进制在 /opt/homebrew、/tmp、~/Library 下可执行——
// 哪个系统位置放行只能执行时探测，故脚本对每个候选拷贝后实跑 `version`/`-v`
// 自测（batch6 真机 fold-in）。执行目录内放 helper 副本 + 内核的 root 属主副本
// （bin/mihomo、bin/mihomo.sha256）。
func ExecDirCandidates() []string {
	return []string{DefaultConfigDir, LibexecDir + "/tradewind", "/opt/tradewind"}
}

// HelperPathIn / CorePathIn / SidecarPathIn 是执行目录内三个文件的落点。
func HelperPathIn(execDir string) string { return filepath.Join(execDir, HelperFileName) }
func CorePathIn(execDir string) string   { return filepath.Join(execDir, "bin", "mihomo") }
func SidecarPathIn(execDir string) string {
	return filepath.Join(execDir, "bin", "mihomo.sha256")
}

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
//
// v3（批⑥b §4.1）：layout=3 引入 root 专属布局——内核工作目录收进锚目录内的
// service-data，日志收进锚目录内的 logs/，代理入口 <anchor>/core.sock；旧布局
// （runtime/sock/log 在用户数据目录、/Library/Logs）的锚缺 layout 键，一律判无效。
type ConfigFile struct {
	// Layout 恒为 3；旧锚缺此键 → validate 失败（fail-closed，用户须 tun reinstall）。
	Layout int `json:"layout"`
	// AllowedUIDs 是 peer-uid 白名单（唯一认证闸）。
	AllowedUIDs []int `json:"allowed_uids"`
	// DataDir 是用户数据目录——服务对它**只读**（归一化输入与 geo 同步的来源，I1）。
	DataDir string `json:"data_dir"`
	// DataUID 是数据目录属主（安全打开的属主校验基准 + 日志文件的收敛属主）。
	DataUID int `json:"data_uid"`
	// HelperPath 执行目录内的 helper 副本（plist ProgramArguments[0]；CLI 陈旧检测的
	// 比对来源）。旧布局锚缺此键——服务运行期不消费它，CLI 侧按陈旧处理。
	HelperPath string `json:"helper_path"`
	// CoreBin/Sidecar 是内核的 **root 属主副本**（执行目录下），绝不指向用户可写的
	// data_dir。LPE 修复（batch6 真机 fold-in）：旧布局指向 data_dir/bin——二进制与
	// 校验文件均用户可写，任何用户态进程替换二者再 stop/start 即可以 root 执行任意
	// 代码（sha256 校验挡不住：校验文件本身用户可写）。validate 拒绝该形态。
	CoreBin string `json:"core_bin"`
	Sidecar string `json:"sidecar"`
	// CoreHome 是内核 -d（root:wheel 0700，位于锚目录内）。
	CoreHome string `json:"core_home"`
	// KernelSock 是内核原生 API socket（<core_home>/core.sock，仅 root 服务可达）。
	KernelSock string `json:"kernel_sock"`
	// BrokerSock 是用户域 API 入口（<anchor>/core.sock；c1 只写入并校验，监听在 c4）。
	BrokerSock string `json:"broker_sock"`
	// CoreLog 是内核日志（<anchor>/logs/core.log，data_uid 属主 0600）。
	CoreLog string `json:"core_log"`
	// UserRuntime 是归一化输入（<data_dir>/runtime.yaml，只经 safeopen 读取）。
	UserRuntime string `json:"user_runtime"`
}

// validate 锚完整性校验（服务启动闸——缺字段宁可 exit 1 也不带病运行）。
// anchorDir 是锚所在目录（LoadConfigFile 以 filepath.Dir(path) 传入）：CoreHome/
// KernelSock/CoreLog/BrokerSock 必须在它之内，测试的临时目录同样成立。
func (c ConfigFile) validate(anchorDir string) error {
	// ① 布局代际：旧锚（无 layout 键或 ≠3）一律判无效——批⑥b 起内核工作目录必须
	// 在 root 属主目录里，带旧锚运行等于把 I2 整个打开。
	if c.Layout != 3 {
		return errors.New("service: service.json layout must be 3 (got a pre-batch6b anchor); run `tradewind tun reinstall`")
	}
	// ② 身份字段
	if len(c.AllowedUIDs) == 0 {
		return errors.New("service: service.json missing allowed_uids")
	}
	if c.DataUID <= 0 {
		return errors.New("service: service.json has invalid data_uid")
	}
	// ③ 全部路径字段：非空、绝对路径、Clean 后与原值相等（拒绝 ..、尾斜杠、重复分隔符
	// ——后面的 pathWithin 都是词法判断，先保证输入形态单一）。
	paths := []string{c.DataDir, c.CoreBin, c.Sidecar, c.CoreHome, c.KernelSock, c.BrokerSock, c.CoreLog, c.UserRuntime, c.HelperPath}
	for _, p := range paths {
		if p == "" {
			return errors.New("service: service.json has empty path field")
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("service: service.json path %q is not absolute", p)
		}
		if filepath.Clean(p) != p {
			return fmt.Errorf("service: service.json path %q is not clean (rejecting .., trailing slash, duplicate separators)", p)
		}
	}
	// ④ 内核专属产物必须全部位于锚目录内（root 属主、用户不可写），且内核 socket
	// 路径与 CoreHome 派生一致（防"sock 指到目录外"的锚篡改）。
	for _, p := range []string{c.CoreHome, c.KernelSock, c.CoreLog} {
		if !pathWithin(p, anchorDir) {
			return fmt.Errorf("service: %s must live inside the anchor dir %s", p, anchorDir)
		}
	}
	if want := KernelSockIn(c.CoreHome); c.KernelSock != want {
		return fmt.Errorf("service: kernel_sock must be %s (got %s)", want, c.KernelSock)
	}
	// ⑤ 代理入口：锚目录下的固定文件名（词法事实，编译期常量同形）；不得落在用户
	// 可写的 DataDir 或内核的 CoreHome 里。
	if want := BrokerSockIn(anchorDir); c.BrokerSock != want {
		return fmt.Errorf("service: broker_sock must be %s (got %s)", want, c.BrokerSock)
	}
	if pathWithin(c.BrokerSock, c.DataDir) {
		return errors.New("service: broker_sock must not live inside data_dir (user-writable)")
	}
	if pathWithin(c.BrokerSock, c.CoreHome) {
		return errors.New("service: broker_sock must not live inside core_home")
	}
	// ⑥ 归一化输入固定为数据目录的 runtime.yaml。
	if want := filepath.Join(c.DataDir, "runtime.yaml"); c.UserRuntime != want {
		return fmt.Errorf("service: user_runtime must be %s (got %s)", want, c.UserRuntime)
	}
	// ⑦ 用户数据目录与内核工作目录互不嵌套；root 执行的内核副本不得落在用户可写
	// 的 data_dir 之内（LPE 纵深防御沿用）。
	if pathWithin(c.CoreHome, c.DataDir) {
		return errors.New("service: core_home must not live inside data_dir (user-writable)")
	}
	if pathWithin(c.DataDir, c.CoreHome) {
		return errors.New("service: data_dir must not live inside core_home")
	}
	if pathWithin(c.CoreBin, c.DataDir) {
		return errors.New("service: core_bin must not live inside data_dir (user-writable; root must run the root-owned copy)")
	}
	return nil
}

// pathWithin 词法包含判断：child 是否位于 parent 目录之内（".." 逃逸与不同根
// 前缀均视为不在其内）。
func pathWithin(child, parent string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// LoadConfigFile 读取并校验 service.json（以配置文件所在目录为锚目录——CoreHome
// 等内核专属路径必须在它之内；生产即 DefaultConfigDir）。
func LoadConfigFile(path string) (ConfigFile, error) {
	var cfg ConfigFile
	if err := ReadJSONFile(path, &cfg); err != nil {
		return ConfigFile{}, err
	}
	if err := cfg.validate(filepath.Dir(path)); err != nil {
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
