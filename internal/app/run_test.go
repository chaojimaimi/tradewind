package app

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// TestRunIdempotentWhenAlreadyRunning 锁定 v0.4.12 幂等语义：活实例持 pidfile flock 锁时
// Run 返回 nil（"已在运行"=目标态已达成）而非错误。
// WHY launchd agent KeepAlive={SuccessfulExit:false} 是 crash-restart 语义——壳侧主开关
// detached spawn 抢先持锁后，agent 重试撞锁若 exit(1) 会触发无限 crash-restart 节流循环
// （autostart.go 注释预言的坑，2026-11-19 真机日志实证）。exit 0 让 launchd 视为成功停止重试。
// 本测试只打冲突路径：持锁后 Run 应在 LockPIDFile 即返回，不触碰内核/网关/系统代理。
func TestRunIdempotentWhenAlreadyRunning(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := home.Ensure(); err != nil {
		t.Fatalf("ensure home: %v", err)
	}
	// 模拟活实例：持有 pidfile flock 独占锁
	f, err := os.OpenFile(home.PIDFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open pidfile: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	if err := Run(context.Background(), home, StartOptions{ApplySysProxy: false}); err != nil {
		t.Fatalf("Run with live lock holder = %v, want nil (idempotent success)", err)
	}
}

// TestRunFailsWhenPidfileUnopenable 锁定审查 P2 修复的另一半：pidfile 打开失败（非
// flock 冲突）必须照常报错而非幂等成功——否则假报"已运行"会让 launchd crash-restart
// 永久停止拉起（守护静默下线）。用同名目录占位制造 EISDIR。
func TestRunFailsWhenPidfileUnopenable(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := home.Ensure(); err != nil {
		t.Fatalf("ensure home: %v", err)
	}
	if err := os.Mkdir(home.PIDFile(), 0o755); err != nil {
		t.Fatalf("plant pidfile-as-directory: %v", err)
	}
	err := Run(context.Background(), home, StartOptions{ApplySysProxy: false})
	if err == nil {
		t.Fatal("Run with unopenable pidfile = nil, want error")
	}
	if errors.Is(err, ErrInstanceRunning) {
		t.Fatalf("error = %v, must not be classified as instance running", err)
	}
}
