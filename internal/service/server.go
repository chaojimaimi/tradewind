// server.go：特权服务本体——监听/认证/仲裁/内核托管（批⑥ §3.1）。
package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	gaveUp  bool // Monitor 重启预算耗尽（内核终亡）→ 新 start 请求可重新准入
	curGen  int  // 所有权仲裁基线：start 须 > curGen；显式 stop 后归零（见 handleStop）
	rootCtx context.Context
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
	if err := s.adoptOrphans(); err != nil {
		return fmt.Errorf("service: adopt orphans: %w", err)
	}
	ln, err := s.listen()
	if err != nil {
		return err
	}
	fmt.Printf("service: listening on %s (allowed_uids=%v)\n", s.cfg.SockPath, s.conf.AllowedUIDs)
	go s.acceptLoop(ln)
	<-ctx.Done()
	fmt.Println("service: shutting down...")
	s.stopCore()
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
			fmt.Fprintf(os.Stderr, "service: accept: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "service: %v\n", err)
		return
	}
	if !containsInt(s.conf.AllowedUIDs, uid) {
		// 断连 + 计数日志（服务日志可见，评审 P3）：不回包不提示，直接断
		fmt.Fprintf(os.Stderr, "service: rejected uid %d (not in allowed_uids)\n", uid)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	line, err := bufio.NewReaderSize(conn, maxRequestLine).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "service: uid %d read: %v\n", uid, err)
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
	default:
		return Response{OK: false, Error: fmt.Sprintf("service: unknown action %q", req.Action)}
	}
}

// handleStart：幂等 no-op 先行（core 托管中 → 不重复 spawn，防端口冲突握手循环，
// 评审 P1-5；陈旧代际的 no-op 请求同样安全返回），再 generation 仲裁，再 spawn。
// 已知边界（batch6 review P3-1 登记，本版不修）：mu 跨服务端同步握手（spawnCore
// 内 WaitReady 最长 ~20s）——崩溃重启竞态下 stop/status 请求会排队等锁一次，
// 客户端 10s/30s 档超时触发调用方回滚/指引（状态自洽，无半开态）。主流时序不撞
// （幂等 no-op 快路径不持长锁）；无锁化重构留 plan 观察期评估。
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
	if err := s.spawnCore(); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	s.curGen = req.Generation
	return Response{OK: true, PID: s.mgr.PID()}
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
		fmt.Fprintf(os.Stderr, "service: write response: %v\n", err)
	}
}
