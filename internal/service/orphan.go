// orphan.go：进程身份/回收/属主工具（孤儿收编与数据目录回收共用，批⑥ §3.1）。
// 语义与 app/process.go 同构——service 包不得依赖 app（层次反向），此处独立实现。
package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// coreProcessMatches 身份校验：ps 命令行含 "-d <dataDir>"（防 pid 复用误杀；
// macOS 无 /proc，取 ps -p 输出）。
func coreProcessMatches(pid int, dataDir string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.TrimSpace(string(out)), "-d "+dataDir)
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

// chownDataDirBack root core 退出后递归 chown 回 data_uid（批⑥ §3.1/R10/F22）：
// root mihomo 会以 root 属主创建/改写 cache.db、geoip*、日志——不回收则 tun off
// 后用户态 core EACCES。root→用户方向回收安全；非 root（测试）为同 uid no-op。
func chownDataDirBack(dataDir string, uid int) error {
	return filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if chErr := os.Chown(path, uid, -1); chErr != nil {
			return fmt.Errorf("service: chown %s: %w", path, chErr)
		}
		return nil
	})
}
