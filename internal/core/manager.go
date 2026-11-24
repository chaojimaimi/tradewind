package core

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// Spawn 校验二进制并启动内核子进程，stdout/stderr 落日志。
func (m *Manager) Spawn() error {
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
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("core: spawn %s: %w", m.cfg.BinPath, err)
	}
	m.mu.Lock()
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
		restarts++
		if err := m.Spawn(); err != nil {
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

// openLogFile 打开内核日志（O_APPEND 追加写）。
// WHY O_NOFOLLOW（batch6 真机 fold-in 2）：特权服务以 root 打开日志路径（批⑥b 起
// 是 root 属主目录下的 <anchor>/logs/core.log，纵深防御原样保留）——该路径若被换成
// 指向系统文件的符号链接，跟随打开 = root 往目标追加日志。O_NOFOLLOW 由内核拒绝
// （ELOOP），不依赖调用前的 Lstat（那有 TOCTOU 窗口）。只作用于路径末段，普通
// 文件零回归。
func openLogFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("core: open log: %s is a symbolic link (refusing to follow)", path)
		}
		return nil, fmt.Errorf("core: open log: %w", err)
	}
	return f, nil
}

// rotateLog 超过 logRotateSize 时将日志轮转为 .1（保留 1 份，直接覆盖旧 .1）。
// Lstat 判符号链接并直接拒绝：跟随 Stat 会读到目标的大小，随后 Rename 移走的是
// 链接本身——以 root 对用户可控路径做这些动作都不该发生。
func rotateLog(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("core: stat log: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("core: log path %s is a symbolic link (refusing to rotate)", path)
	}
	if info.Size() <= logRotateSize {
		return nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("core: rotate log: %w", err)
	}
	return nil
}

// tailFile 读文件末尾 n 字节。O_NOFOLLOW：日志内容会进错误信息（fail-fast /
// giveup 的 log tail）——特权服务下若跟随符号链接，root 专读文件的内容会被带进
// 返回给用户态的错误里。读不了只表现为"日志不可用"，不影响主流程。
func tailFile(path string, n int64) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	offset := info.Size() - n
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("(log read failed: %v)", err)
	}
	return string(buf)
}
