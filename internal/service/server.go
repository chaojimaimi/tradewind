// server.go：特权服务本体——监听/认证/仲裁/内核托管（批⑥ §3.1）。
package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
)

// 服务生命周期参数：单请求读超时（防慢速占位）、内核握手窗口、孤儿回收宽限。
const (
	requestReadTimeout = 10 * time.Second
	handshakeWindow    = core.DefaultHandshakeTimeout + 5*time.Second
	killGrace          = 5 * time.Second
	maxRequestLine     = 4096
)

// ServerConfig 是服务运行锚（生产=DefaultServerConfig；测试注入 /tmp 短路径，F19）。
type ServerConfig struct {
	SockPath   string
	ConfigPath string
	PIDPath    string
}

// DefaultServerConfig 返回生产锚。
func DefaultServerConfig() ServerConfig {
	return ServerConfig{SockPath: DefaultSockPath, ConfigPath: DefaultConfigPath, PIDPath: DefaultCorePIDPath}
}

// RunCLI 是 `tradewind service` 隐藏子命令入口：root 守卫（launchd daemon 是唯一
// 合法运行形态）+ 信号驱动生命周期（SIGINT/SIGTERM → 停核/sock 清理）。
func RunCLI() error {
	// 批⑥b §4.1：umask 022 钉死并被子进程（内核）继承——内核解压 UI、新建文件的
	// 权限由此收敛；服务自身没有 umask(0) 窗口（v1.0 的窗口随代理入口移出用户目录
	// 一并删除，§3.1）。
	syscall.Umask(0o022)
	if os.Geteuid() != 0 {
		return errors.New("service: tradewind service must run as root (launchd daemon)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return NewServer(DefaultServerConfig()).Run(ctx)
}

// Server 是特权服务：root 身份托管内核（core.Manager 复用；批⑥b 起内核工作目录
// 收进 root 属主的 <core_home>，用户域只经代理访问内核 API）。
type Server struct {
	cfg  ServerConfig
	conf ConfigFile

	mu      sync.Mutex
	mgr     *core.Manager
	gaveUp  bool       // Monitor 重启预算耗尽（内核终亡）→ 新 start 请求可重新准入
	curGen  int        // 所有权仲裁基线：start 须 > curGen；显式 stop 后归零（见 handleStop）
	broker  *apiBroker // 用户域 API 入口（<anchor>/core.sock 白名单代理，批⑥b §4.3/I4）
	rootCtx context.Context
	// snapMu 保护 snapshot（最近一份归一化产物，nil = 无；批⑥b §4.4）。独立于 s.mu：
	// restoreRuntime 只取它（start 路径调用它时调用方已持 s.mu；取锁顺序恒
	// s.mu → snapMu）。全部触碰点在 snapshot.go。
	snapMu   sync.Mutex
	snapshot []byte
	// verifyExecTrust 执行目录信任链复核（锚所指可执行路径 + 全部祖先目录）。生产=
	// verifyAnchorExecTrust；测试注入——测试夹具在 /tmp 下，祖先含系统符号链接
	// /tmp 与全局可写目录，真 Lstat 必拒。
	verifyExecTrust func(ConfigFile) error
	// verifyHomeTrust root 专属内核工作目录复核（<core_home> 祖先链 + <anchor>/logs
	// + CoreLog）。生产=verifyCoreHomeTrust；测试注入桩（同 verifyExecTrust 的原因）。
	verifyHomeTrust func(ConfigFile) error
	// coreEnv 内核进程环境（生产=恰好三个固定变量，不继承 launchd 环境；测试缝——
	// 桩内核的 FAKE_CORE_MODE 等测试模式靠环境变量读取）。绝不在生产路径拼接
	// os.Environ()：SKIP_SAFE_PATH_CHECK 一旦经 launchctl setenv 带入即全线失守（M8）。
	coreEnv func(ConfigFile) []string
}

// NewServer 构造服务。
func NewServer(cfg ServerConfig) *Server {
	return &Server{
		cfg:             cfg,
		broker:          newAPIBroker(),
		verifyExecTrust: verifyAnchorExecTrust,
		verifyHomeTrust: verifyCoreHomeTrust,
		coreEnv:         coreEnvProduction,
	}
}

// Run 常驻主循环：加载锚 → 信任链复核 → 孤儿收编 → 监听（0666）→ accept；ctx
// 结束 → 收敛退出。service.json 非法/缺失、或执行目录/内核工作目录信任链不达标
// → 返回错误（CLI exit 1 → launchd 节流重试，R3 fail-closed）。
func (s *Server) Run(ctx context.Context) error {
	conf, err := LoadConfigFile(s.cfg.ConfigPath)
	if err != nil {
		return err
	}
	s.conf = conf
	s.rootCtx = ctx
	// 启动复核（review P2）：Install 时的归属校验只跑一次；此后祖先目录若用户/admin
	// 可写，执行目录可被整个换成符号链接。不达标一律不带病运行。
	if err := s.verifyExecTrust(conf); err != nil {
		return fmt.Errorf("service: exec dir failed the ownership check (fail-closed): %w", err)
	}
	// 批⑥b I2：内核工作目录与其产物所在子树（<anchor>/logs）同样逐级复核。
	if err := s.verifyHomeTrust(conf); err != nil {
		return fmt.Errorf("service: core home failed the trust check (fail-closed): %w", err)
	}
	// L1 种子（批⑥b §4.4）：先于孤儿收编与任何请求协程——磁盘上的 R 过当前归一化器
	// 复验后成为快照；拒绝/缺失一律 nil（L2 的孤儿回收因此不重拉，等下一次 start）。
	s.seedSnapshot(s.conf)
	if err := s.adoptOrphans(); err != nil {
		return fmt.Errorf("service: adopt orphans: %w", err)
	}
	ln, err := s.listen()
	if err != nil {
		return err
	}
	svcOutf("service: listening on %s (allowed_uids=%v)", s.cfg.SockPath, s.conf.AllowedUIDs)
	go s.acceptLoop(ln)
	<-ctx.Done()
	svcOutf("service: shutting down...")
	s.stopCore()
	s.broker.close() // 幂等：stopCore 已关过则 no-op（批⑥b §4.3）
	_ = ln.Close()
	// sock 清理：mihomo 退出不 unlink 的残留由控制进程收敛（Manager 同语义）
	_ = os.Remove(s.cfg.SockPath)
	return nil
}

// listen 清残留 sock 后监听并收敛 0666（白名单外的 uid 能 connect 但被 getpeereid
// 即刻拒绝——权限放开的暴露面由白名单闸兜住，评审 P3）。
func (s *Server) listen() (net.Listener, error) {
	if err := os.Remove(s.cfg.SockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("service: remove stale sock: %w", err)
	}
	ln, err := net.Listen("unix", s.cfg.SockPath)
	if err != nil {
		return nil, fmt.Errorf("service: listen %s: %w", s.cfg.SockPath, err)
	}
	if err := os.Chmod(s.cfg.SockPath, 0o666); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("service: chmod sock: %w", err)
	}
	return ln, nil
}

// acceptLoop 接受循环：每连接一个 goroutine（生命周期由 handleConn 的 defer Close
// 与读超时兜底）。
func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.rootCtx.Done():
				return // 正常关停（Run 关闭 listener）
			default:
			}
			svcErrf("service: accept: %v", err)
			return
		}
		go s.handleConn(conn)
	}
}

// handleConn 单连接处理：peer uid 白名单（唯一认证闸）→ 严格解码 → 分发 → 应答。
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	uid, err := peerUID(conn)
	if err != nil {
		svcErrf("service: %v", err)
		return
	}
	if !containsInt(s.conf.AllowedUIDs, uid) {
		// 断连 + 计数日志（服务日志可见，评审 P3）：不回包不提示，直接断
		svcErrf("service: rejected uid %d (not in allowed_uids)", uid)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	line, err := bufio.NewReaderSize(conn, maxRequestLine).ReadString('\n')
	if err != nil {
		svcErrf("service: uid %d read: %v", uid, err)
		return
	}
	req, err := DecodeRequest([]byte(line))
	if err != nil {
		writeResponse(conn, Response{OK: false, Error: err.Error()})
		return
	}
	writeResponse(conn, s.dispatch(req))
}

// dispatch 动作分发。
func (s *Server) dispatch(req Request) Response {
	switch req.Action {
	case ActionStart:
		return s.handleStart(req)
	case ActionStop:
		return s.handleStop()
	case ActionStatus:
		return s.handleStatus()
	case ActionReload:
		return s.handleReload()
	default:
		return Response{OK: false, Error: fmt.Sprintf("service: unknown action %q", req.Action)}
	}
}

// handleStart（批⑥b §4.4 L3）：幂等 no-op 先行（core 托管中 → 不重复 spawn，防端口
// 冲突握手循环，评审 P1-5；陈旧代际的 no-op 请求同样安全返回），再 generation 仲裁，
// 再"归一化 → 提交（promote 快照 + 落盘 R）→ spawn"。start 从不沿用旧快照——用户
// 在服务空闲期间改的配置，下一次 start 必须生效。归一化拒绝 → rejected:true，curGen
// 不动、R 不动、不 spawn（I3：内核只加载服务归一化过的配置）。
// 已知边界（batch6 review P3-1 登记，本版不修）：mu 跨服务端同步握手（spawnCore
// 内 WaitReady 最长 ~20s）——崩溃重启竞态下 stop/status 请求会排队等锁一次，
// 客户端 10s/30s 档超时触发调用方回滚/指引（状态自洽，无半开态）。主流时序不撞
// （幂等 no-op 快路径不持长锁）；无锁化重构留 plan 观察期评估。同理，start 持
// s.mu 期间的归一化还要等 normalizeGate（最多一份在途 reload 的归一化）——排队
// 上界是它的一次归一化时长。
func (s *Server) handleStart(req Request) Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coreManaged() {
		return Response{OK: true, PID: s.mgr.PID(), AlreadyRunning: true}
	}
	if req.Generation <= s.curGen {
		return Response{OK: false, Error: fmt.Sprintf(
			"service: stale generation %d (want > %d); run `tradewind start` to refresh", req.Generation, s.curGen)}
	}
	res, rej, err := loadNormalized(s.conf)
	if rej != nil {
		text := rejectionText(rej)
		svcErrf("service: start refused: %s", text)
		return Response{OK: false, Rejected: true, Error: text}
	}
	if err != nil {
		svcErrf("service: start refused: %v", err)
		return Response{OK: false, Error: err.Error()}
	}
	if err := s.commitRuntime(s.conf, res.Bytes); err != nil {
		svcErrf("service: start refused: %v", err)
		return Response{OK: false, Error: err.Error()}
	}
	if err := s.spawnCore(); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	s.curGen = req.Generation
	return Response{OK: true, PID: s.mgr.PID(), Ignored: res.Ignored}
}

// reloadPushTimeout 是服务端推送（PUT /configs 内联归一化产物）的预算上限，与
// core.putConfigsTimeout 同档（ctx 与 http.Client 双保险）。
const reloadPushTimeout = 20 * time.Second

// handleReload（批⑥b §4.4 L4）：归一化在 s.mu 之外先做（纯计算——reload 的持锁
// 上界不挤占 stop/status，R15）；归一化自身经 normalizeGate 全服务串行（并发 reload
// 不叠加内存，见 rootstate.go）。托管中 → 归一化产物内联推送内核，内核接受（204）
// 才提交（promote 快照 + 落盘）；内核 400 → rejected:true，磁盘与快照原封不动；
// 未托管（从未启动 / 自愈预算耗尽）→ 直接提交。请求零路径、不校验 generation
// （与 stop/status 同类）。
func (s *Server) handleReload() Response {
	res, rej, err := loadNormalized(s.conf)
	if rej != nil {
		text := rejectionText(rej)
		svcErrf("service: reload refused: %s", text)
		return Response{OK: false, Rejected: true, Error: text}
	}
	if err != nil {
		svcErrf("service: reload refused: %v", err)
		return Response{OK: false, Error: err.Error()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coreManaged() {
		if resp := s.pushToKernel(res.Bytes); resp != nil {
			return *resp
		}
		if err := s.commitRuntime(s.conf, res.Bytes); err != nil {
			svcErrf("service: reload commit failed: %v", err)
			return Response{OK: false, Error: err.Error()}
		}
		svcOutf("service: reload pushed to kernel (%d bytes, %d keys ignored)", len(res.Bytes), len(res.Ignored))
		return Response{OK: true, Ignored: res.Ignored}
	}
	if err := s.commitRuntime(s.conf, res.Bytes); err != nil {
		svcErrf("service: reload commit failed: %v", err)
		return Response{OK: false, Error: err.Error()}
	}
	svcOutf("service: reload persisted (%d bytes, %d keys ignored; core not managed)", len(res.Bytes), len(res.Ignored))
	return Response{OK: true, Ignored: res.Ignored}
}

// pushToKernel 把归一化产物内联推给运行中的内核（调用方持 s.mu）。返回 nil = 内核已
// 接受；非 nil = 应答失败：内核 400 → rejected:true（日志只记状态码，理由只回调用方，
// I6）；其它状态码与传输错误 → 普通失败。
func (s *Server) pushToKernel(payload []byte) *Response {
	// 内核的 unix-socket 控制器从不鉴权（实测 mihomo 对 -ext-ctl-unix 不校验
	// Authorization），secret 恒空
	kc := core.NewClient(s.conf.KernelSock, "")
	ctx, cancel := context.WithTimeout(s.rootCtx, reloadPushTimeout)
	defer cancel()
	err := kc.PutConfigsPayload(ctx, string(payload))
	var se *core.StatusError
	if errors.As(err, &se) {
		if se.Code == http.StatusBadRequest {
			// 日志只记状态码：内核的拒绝理由可能回显配置值（yaml 类型错误会带出
			// 片段），它只回给调用方——那是用户自己的配置；不进 root 属主的
			// 日志（I6）
			text := "service: 内核拒绝新配置：" + kernelRejectMessage(se)
			svcErrf("service: reload refused by kernel (status 400)")
			return &Response{OK: false, Rejected: true, Error: text}
		}
		// 其它状态码：响应体是内核自由文本，过一道闸再回给调用方（不复现 legacy 文案）
		text := fmt.Sprintf("service: 内核推送失败（status %d）：%s", se.Code, sanitizeForLog(se.Body))
		svcErrf("service: reload push failed (status %d)", se.Code)
		return &Response{OK: false, Error: text}
	}
	if err != nil {
		svcErrf("service: reload push failed: %v", err)
		return &Response{OK: false, Error: err.Error()}
	}
	return nil
}

// handleStop：优雅停核（Manager Stop 含 StopGrace/SIGKILL）→ 数据目录属主回收 →
// 代际基线归零。WHY 归零：防"服务 start 成功但 start 进程 state 写入丢失"的代际
// 死锁（下次 start 携带同代际会被拒）——与"服务重启后归零只会放宽准入"同一 F13
// 语义；双 spawn 风险由 start 幂等 no-op + flock 串行化兜住。
func (s *Server) handleStop() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stopCoreLocked(); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	s.curGen = 0
	return Response{OK: true}
}

// handleStatus：托管 pid + /version 探活（version 空/pid=0 = 内核未运行）。
// 已知边界（batch6 review P3-1 登记，本版不修）：mu 跨 2s 探活——排队上界 2s，
// 远小于客户端 status 档（5s），主流时序不阻塞；与 handleStart 的长持锁同源，
// 观察期一并评估。
func (s *Server) handleStatus() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid := 0
	if s.mgr != nil {
		pid = s.mgr.PID()
	}
	ctx, cancel := context.WithTimeout(s.rootCtx, 2*time.Second)
	defer cancel()
	v, err := core.NewClient(s.conf.KernelSock, "").Version(ctx)
	if err != nil {
		return Response{OK: true}
	}
	return Response{OK: true, PID: pid, Version: v}
}

// coreManaged 内核托管态：spawn 过、未主动停止、Monitor 未放弃（崩溃自愈窗口内
// 也算托管中——期间 start 请求 no-op，健康由 start 进程就绪轮询确认）。
func (s *Server) coreManaged() bool {
	return s.mgr != nil && !s.gaveUp
}

// writeResponse 应答一行 JSON（写失败仅日志——对端可能已断开）。
func writeResponse(conn net.Conn, resp Response) {
	line, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		svcErrf("service: write response: %v", err)
	}
}
