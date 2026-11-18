package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// stopTimeout 等待 start 进程完成优雅还原的窗口（含内核 SIGTERM 5s 宽限）。
const stopTimeout = 20 * time.Second

// Stop 向 pidfile 进程发 SIGTERM 并等待退出。
// 发信号前校验进程身份（ps 命令行含 tradewind），防 pid 复用误杀无关进程（M0-plan §4）。
func Stop(home paths.Home) error {
	data, err := os.ReadFile(home.PIDFile())
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("tradewind is not running")
		return nil
	}
	if err != nil {
		return fmt.Errorf("app: read pidfile: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		// pidfile 损坏视同无运行实例，清理后返回
		_ = os.Remove(home.PIDFile())
		return fmt.Errorf("app: corrupt pidfile %q, removed", strings.TrimSpace(string(data)))
	}
	if pid == os.Getpid() {
		return fmt.Errorf("app: refusing to stop self (pid %d)", pid)
	}
	cmdline := processCommand(pid)
	if cmdline == "" {
		_ = os.Remove(home.PIDFile())
		fmt.Printf("removed stale pidfile (pid %d not alive)\n", pid)
		return nil
	}
	if !pidCommandIsTradewind(pid) {
		return fmt.Errorf("app: pid %d is not a tradewind process (%.80s), refusing to kill", pid, cmdline)
	}
	fmt.Printf("stopping tradewind (pid %d)...\n", pid)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("app: sigterm %d: %w", pid, err)
	}
	if !waitForExit(pid, stopTimeout) {
		return fmt.Errorf("app: tradewind (pid %d) did not exit within %s; try SIGKILL manually", pid, stopTimeout)
	}
	fmt.Println("tradewind stopped")
	return nil
}
