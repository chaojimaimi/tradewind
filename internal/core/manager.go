package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 内核生命周期默认参数（M0-plan §5.1 步骤 6/9/10）。
const (
	DefaultHandshakeTimeout = 15 * time.Second
	DefaultStopGrace        = 5 * time.Second
	logRotateSize           = 5 << 20 // core.log >5MB 轮转为 .1，保留 1 份
	logTailSize             = 8 << 10 // fail-fast 时读取的日志尾部
	// DefaultSockPerm 握手成功后 core.sock 收敛权限的默认值（纵深防御，M0-plan §8）。
	// 批⑥b 起特权服务沿用这个默认 0600——内核原生 socket 只对 root 服务可达，用户域
	// 只经代理访问（批⑥b §4.3）；用户态形态不传 SockPerm（零值即本默认，socket 归
	// 用户本人，0600 可连）。
	DefaultSockPerm = 0o600
)

// Config 是内核管理器配置。零值字段使用默认值。
type Config struct {
	BinPath     string
	SidecarPath string
	DataDir     string // mihomo -d
	RuntimePath string // mihomo -f
	SockPath    string // mihomo -ext-ctl-unix
	Secret      string
	LogPath     string
	// HandshakeTimeout 握手超时（默认 15s）；RestartBackoff 崩溃重启退避序列（默认 1s/2s/4s）；
	// StopGrace SIGTERM 后等待退出的宽限（默认 5s）
	HandshakeTimeout time.Duration
	RestartBackoff   []time.Duration
	StopGrace        time.Duration
	// Env 覆盖子进程环境（默认继承父进程；测试注入桩内核模式用。特权服务传显式
	// 最小环境——批⑥b §4.1）
	Env []string
	// SockPerm 握手成功后 core.sock 的收敛权限（零值=默认 0600。批⑥b 起特权服务
	// 沿用默认——用户域只经代理访问内核 API，批⑥b §4.3。Monitor 崩溃重启内部
	// Spawn+WaitReady 走同参数，自愈后权限保持）
	SockPerm os.FileMode
	// PreSpawn 是每次 spawn 前的前置钩子（nil = 无）：返回错误则本次 spawn 不执行。
	// 特权服务用它复核执行目录归属——Monitor 崩溃自愈的内部 respawn 不经过服务的
	// spawnCore，钩子保证初始 spawn 与自愈 respawn 走同一道闸。
	PreSpawn func() error
	// LogOwner 非空时：openLogFile 之后对新打开的日志 fd 做 fchown(uid,-1) +
	// fchmod(0600)；失败 → 关闭 fd 并返回错误，本次 spawn 不执行。批⑥b §4.1/§4.6：
	// 新 core.log 只在 Spawn() 里产生（初次启动，或自愈 respawn 时轮转出一份新的，
	// F21），收敛放 Manager 里两条 spawn 路径都覆盖；打开带了 O_NOFOLLOW，fd 指向
	// 的就是那个普通文件，不存在"按路径 chown 跟随符号链接"的面。
	LogOwner *int
	// OnSpawned 在 cmd.Start() 成功、m.cmd 赋值之后调用，传入子进程 pid（nil = 无）。
	// 批⑥b §4.1：特权服务用它重写 core.pid——每次 spawn 都记录（含自愈 respawn，
	// F20），否则服务重启时收编不到自愈出来的内核。
	OnSpawned func(pid int)
}

func (c *Config) fillDefaults() {
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if len(c.RestartBackoff) == 0 {
		c.RestartBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	}
	if c.StopGrace <= 0 {
		c.StopGrace = DefaultStopGrace
	}
	if c.SockPerm == 0 {
		c.SockPerm = DefaultSockPerm
	}
}

// Manager 管理 mihomo 子进程：spawn / 握手 / 崩溃重启 / stop。
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	cmd      *exec.Cmd
	exit     chan struct{} // 当前子进程退出时关闭
	stopping bool          // 主动 Stop 置位，用于区分崩溃与正常退出
}

// NewManager 构造管理器（填充默认参数）。
func NewManager(cfg Config) *Manager {
	cfg.fillDefaults()
	return &Manager{cfg: cfg}
}

// errStoppedByOwner 是 Monitor 专用 respawn 的拒绝信号：Owner 已经 Stop，这次自愈
// 不该发生。Monitor 据此安静收场，而不是当作"重启失败"去触发 onGiveUp。
var errStoppedByOwner = errors.New("core: manager was stopped; respawn refused")

// Spawn 校验二进制并启动内核子进程，stdout/stderr 落日志。
func (m *Manager) Spawn() error { return m.start(false) }

// respawn 是 Monitor 崩溃自愈专用的启动：与 Spawn 的唯一区别是"Owner 已 Stop 则
// 拒绝"（见 start）。
func (m *Manager) respawn() error { return m.start(true) }

// start 是 Spawn / respawn 的公共主体。前置工作（钩子、校验、日志）在锁外做；
// stopping 的判定与 cmd.Start() 放进同一把 m.mu 临界区。
//
// WHY（Stop/respawn 竞争缺陷）：Monitor 在退避等待期间被 Stop 时，旧实现是
// "先 cmd.Start() 再加锁存句柄"——Stop 锁内置位 stopping、快照到的却是已死的旧
// 进程，于是正常返回；退避结束后 Monitor 无条件 Spawn，拉起一个调用方已经丢弃
// 引用的内核（特权形态下 = 失控的 root 内核，后续启动端口冲突）。Stop 同样在
// m.mu 内置位并快照，互斥之后只剩两种结局：Stop 先进临界区 → respawn 看到
// stopping 直接拒绝；respawn 先进 → Stop 快照到新进程，照常把它停掉。
func (m *Manager) start(monitor bool) error {
	// 自愈路径先做一次无锁预检：Monitor 的早退与这里之间仍有窗口，若 Owner 恰在其间
	// Stop，预检让本次 respawn 在动日志（轮转、新建、属主收敛）之前就收场。锁内的
	// 判定（下方）才是正确性所在，这一处只是少留一份被轮转的日志。
	if monitor && m.isStopping() {
		return errStoppedByOwner
	}
	if m.cfg.PreSpawn != nil {
		if err := m.cfg.PreSpawn(); err != nil {
			return fmt.Errorf("core: pre-spawn check: %w", err)
		}
	}
	if err := VerifyBinary(m.cfg.BinPath, m.cfg.SidecarPath); err != nil {
		return err
	}
	if err := rotateLog(m.cfg.LogPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.LogPath), 0o700); err != nil {
		return fmt.Errorf("core: mkdir log dir: %w", err)
	}
	logFile, err := openLogFile(m.cfg.LogPath)
	if err != nil {
		return err
	}
	// 日志属主/权限经 fd 收敛（批⑥b §4.6）：fchown + fchmod 打在刚打开的 fd 上，
	// 失败 → 关闭 fd 返回错误，本次 spawn 不执行
	if m.cfg.LogOwner != nil {
		if err := logFile.Chown(*m.cfg.LogOwner, -1); err != nil {
			logFile.Close()
			return fmt.Errorf("core: chown log: %w", err)
		}
		if err := logFile.Chmod(0o600); err != nil {
			logFile.Close()
			return fmt.Errorf("core: chmod log: %w", err)
		}
	}
	cmd := exec.Command(m.cfg.BinPath,
		"-d", m.cfg.DataDir,
		"-f", m.cfg.RuntimePath,
		"-ext-ctl-unix", m.cfg.SockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if m.cfg.Env != nil {
		cmd.Env = m.cfg.Env
	}
	m.mu.Lock()
	// 只拦自愈：对外的 Spawn 语义不变（调用方每次启动都 new 一个 Manager）
	if monitor && m.stopping {
		m.mu.Unlock()
		logFile.Close()
		return errStoppedByOwner
	}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		logFile.Close()
		return fmt.Errorf("core: spawn %s: %w", m.cfg.BinPath, err)
	}
	m.cmd = cmd
	m.exit = make(chan struct{})
	m.stopping = false
	m.mu.Unlock()
	// 每次 spawn 都通知调用方（批⑥b F20）：特权服务在回调里重写 core.pid——自愈
	// respawn 不经过服务的 spawnCore，回调保证自愈出来的内核也记录在案
	if m.cfg.OnSpawned != nil {
		m.cfg.OnSpawned(cmd.Process.Pid)
	}
	go func() {
		cmd.Wait()
		logFile.Close()
		close(m.exit)
	}()
	return nil
}

// WaitReady 握手：双路等待（M0-plan §5.1 步骤 6）——sock 可用且 GET /version 返回 2xx → ready；
// 进程提前退出 → 立即 fail-fast 并携带日志尾部，不傻等满超时。
func (m *Manager) WaitReady(ctx context.Context) error {
	deadline := time.Now().Add(m.cfg.HandshakeTimeout)
	client := NewClient(m.cfg.SockPath, m.cfg.Secret)
	for {
		if !m.exitClosed() {
			vctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := client.Version(vctx)
			cancel()
			if err == nil {
				// mihomo 建 listener 时自 chmod 666（提前改会被覆盖），故成功后才收敛权限
				if cErr := os.Chmod(m.cfg.SockPath, m.cfg.SockPerm); cErr != nil {
					return fmt.Errorf("core: chmod sock: %w", cErr)
				}
				return nil
			}
		}
		select {
		case <-m.exitSnapshot():
			return m.failFastError()
		case <-ctx.Done():
			return fmt.Errorf("core: handshake canceled: %w", ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("core: handshake timeout after %s; log tail:\n%s",
				m.cfg.HandshakeTimeout, m.TailLog())
		}
	}
}

// failFastError 构造进程提前退出的错误，提取日志尾部并识别端口占用。
func (m *Manager) failFastError() error {
	tail := m.TailLog()
	err := fmt.Errorf("core: process exited during handshake; log tail:\n%s", tail)
	if strings.Contains(tail, "address already in use") {
		err = fmt.Errorf("%w\nhint: 监听端口可能被其他进程占用（检查 settings.yaml 的 mixed_port，或退出冲突应用）", err)
	}
	return err
}

// Monitor 在 Spawn+WaitReady 成功后运行：监视内核意外退出并指数退避重启。
// ctx 结束（调用方收到信号）→ 返回 nil，由调用方执行优雅退出；
// 重启预算（RestartBackoff 长度）耗尽 → 调用 onGiveUp 后返回错误。
// WHY 计数跨崩溃不重置：防止"活一下就崩"的内核无限 flapping；
// 给弃后由调用方完整还原系统代理再退出（绝不留下指向死端口的代理）。
func (m *Manager) Monitor(ctx context.Context, onGiveUp func(error)) error {
	restarts := 0
	giveUp := func(err error) error {
		wrapped := fmt.Errorf("core: restart failed after %d attempt(s): %w; log tail:\n%s",
			restarts, err, m.TailLog())
		if onGiveUp != nil {
			onGiveUp(wrapped)
		}
		return wrapped
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.exitSnapshot():
		}
		if m.isStopping() {
			return nil // 主动 Stop 触发的退出不算崩溃
		}
		if restarts >= len(m.cfg.RestartBackoff) {
			// 预算耗尽且内核又退出了（此前检查必须在前，否则陷入空转）
			return giveUp(errors.New("restart budget exhausted"))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(m.cfg.RestartBackoff[restarts]):
		}
		// 退避窗口内被 Stop：循环顶部那次 isStopping 只覆盖"崩溃时已停"，拦不住窗口
		// 中途的 Stop。这里早退省掉一次无谓的钩子/校验/开日志；漏网的由 start 兜底
		if m.isStopping() {
			return nil
		}
		restarts++
		if err := m.respawn(); err != nil {
			if errors.Is(err, errStoppedByOwner) {
				// Owner 的 Stop 赢得临界区：这是主动停止的收场，不是重启失败
				return nil
			}
			return giveUp(fmt.Errorf("respawn: %w", err))
		}
		if err := m.WaitReady(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !m.exitClosed() {
				// 进程存活但握手失败（配置类问题，重试无意义）→ 回收后放弃
				_ = m.Stop()
				return giveUp(fmt.Errorf("handshake after restart: %w", err))
			}
			// fail-fast：进程又立即崩溃 → 回到顶部继续消耗预算
			continue
		}
	}
}

// Stop 优雅停止内核：SIGTERM → 等待 StopGrace → SIGKILL → 移除 core.sock。
// WHY 控制进程移除 sock：实测 mihomo 退出时不 unlink，残留 sock 需控制进程清理（M0-plan §3）。
func (m *Manager) Stop() error {
	m.mu.Lock()
	m.stopping = true
	cmd, exit := m.cmd, m.exit
	m.mu.Unlock()
	m.removeSock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// 进程已死时 Signal 返回 "process already finished"，与正常路径同等处理（等 exit 即可）
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exit:
	case <-time.After(m.cfg.StopGrace):
		_ = cmd.Process.Kill()
		<-exit
	}
	return nil
}

// PID 返回当前内核子进程 pid；未运行返回 0。
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		return 0
	}
	return m.cmd.Process.Pid
}

func (m *Manager) exitSnapshot() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exit
}

func (m *Manager) exitClosed() bool {
	select {
	case <-m.exitSnapshot():
		return true
	default:
		return false
	}
}

func (m *Manager) isStopping() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping
}

func (m *Manager) removeSock() {
	if err := os.Remove(m.cfg.SockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// sock 清理失败不影响停止流程；目录 700 下残留 sock 无安全面，下次 mihomo listen 前自清
		fmt.Fprintf(os.Stderr, "core: remove sock %s: %v\n", m.cfg.SockPath, err)
	}
}

// TailLog 读取日志末尾（≤logTailSize 字节）用于错误诊断。
func (m *Manager) TailLog() string {
	return tailFile(m.cfg.LogPath, logTailSize)
}
