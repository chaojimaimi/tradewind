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
	sockPerm                = 0o600   // 握手成功后收敛 sock 权限（纵深防御，M0-plan §8）
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
	// Env 覆盖子进程环境（默认继承父进程；测试注入桩内核模式用）
	Env []string
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
	if err := VerifyBinary(m.cfg.BinPath, m.cfg.SidecarPath); err != nil {
		return err
	}
	if err := rotateLog(m.cfg.LogPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.LogPath), 0o700); err != nil {
		return fmt.Errorf("core: mkdir log dir: %w", err)
	}
	logFile, err := os.OpenFile(m.cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("core: open log: %w", err)
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
				if cErr := os.Chmod(m.cfg.SockPath, sockPerm); cErr != nil {
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

// rotateLog 超过 logRotateSize 时将日志轮转为 .1（保留 1 份，直接覆盖旧 .1）。
func rotateLog(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("core: stat log: %w", err)
	}
	if info.Size() <= logRotateSize {
		return nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("core: rotate log: %w", err)
	}
	return nil
}

// tailFile 读文件末尾 n 字节。
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
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
