// Package app 组装 tradewind 顶层行为：start/stop/status/add/update 的全流程编排。
package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// killGrace 是向孤儿内核/残留进程发 SIGTERM 后等待退出的宽限。
const killGrace = 5 * time.Second

// pidLock 是持有期间阻止第二个 start 实例的 flock 文件锁。
type pidLock struct {
	f    *os.File
	path string
}

// LockPIDFile 以 flock 独占 pidfile；已有实例持锁 → 报错（防双开，M0-plan §4）。
func LockPIDFile(home paths.Home) (*pidLock, error) {
	f, err := os.OpenFile(home.PIDFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("app: open pidfile: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("app: another tradewind instance is running (%s locked)", home.PIDFile())
	}
	if err := f.Truncate(0); err == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return &pidLock{f: f, path: home.PIDFile()}, nil
}

// Release 解锁并删除 pidfile（优雅退出最后一步，M0-plan §5.1 步骤 10）。
func (l *pidLock) Release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = os.Remove(l.path)
	_ = l.f.Close()
}

// processAlive 用 kill(pid,0) 探测进程存活（EPERM 也算存活——权限不足不代表进程不在）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processCommand 返回进程命令行（ps）；进程不存在返回空串。
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// killProcessGracefully SIGTERM → 等待 killGrace → SIGKILL。
func killProcessGracefully(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // 已退出
		}
		return fmt.Errorf("app: sigterm %d: %w", pid, err)
	}
	if waitForExit(pid, killGrace) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("app: sigkill %d: %w", pid, err)
	}
	if !waitForExit(pid, killGrace) {
		return fmt.Errorf("app: process %d did not exit after SIGKILL", pid)
	}
	return nil
}

func waitForExit(pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// pidCommandIsCore 校验孤儿内核身份：命令行含 "-d <dataDir>" 才允许动手（防 pid 复用误杀）。
func pidCommandIsCore(pid int, home paths.Home) bool {
	return strings.Contains(processCommand(pid), "-d "+home.String())
}

// pidCommandIsTradewind 校验 pidfile 进程身份：命令行含 tradewind 才允许 SIGTERM（防 pid 复用误杀）。
func pidCommandIsTradewind(pid int) bool {
	return strings.Contains(processCommand(pid), "tradewind")
}
