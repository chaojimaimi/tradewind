// orphan.go：进程身份与回收工具（孤儿收编专用，批⑥ §3.1；批⑥b 起不再有数据目录
// 属主回收——内核工作目录收进 root 专属 <core_home>，root 对用户目录零写入，I1）。
// 语义与 app/process.go 同构——service 包不得依赖 app（层次反向），此处独立实现。
package service

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func containsInt(list []int, v int) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// processAlive 用 kill(pid,0) 探活（EPERM 也算存活——权限不足不代表进程不在）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// coreProcessMatches 身份校验：ps 命令行含 "-d <dir>"（防 pid 复用误杀；macOS 无
// /proc，取 ps -p 输出）。批⑥b 起传 <core_home>（内核工作目录）。
func coreProcessMatches(pid int, dir string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.TrimSpace(string(out)), "-d "+dir)
}

// killProcessGracefully SIGTERM → killGrace → SIGKILL（root 身份执行；孤儿收编专用）。
func killProcessGracefully(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("service: sigterm %d: %w", pid, err)
	}
	deadline := time.Now().Add(killGrace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("service: sigkill %d: %w", pid, err)
	}
	return nil
}
