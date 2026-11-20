package app

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// mutationWait 是获取 mutation.lock 的有界等待上限。
// WHY 35s：覆盖单次订阅拉取超时 30s（subscription.fetchTimeout）+ 余量；
// 超时明确报错而非无限挂起（M2-plan §2.2）。
const mutationWait = 35 * time.Second

// MutationLock 是数据目录 mutation.lock 上的 flock 独占锁。
// 调度器（start 进程）与手动 update/use/add（其他进程）之间无既有互斥，
// 该锁覆盖 fetch→SaveIndex→Synthesize→PUT 全程，杜绝交错热重载旧内容。
type MutationLock struct {
	f *os.File
}

// AcquireMutationLock 以 flock 独占 mutation.lock，最长等 mutationWait。
// 锁文件本身常驻不删除——WHY：删除会让并发竞争者在旧 inode 上持锁（flock 经典竞态）。
func AcquireMutationLock(home paths.Home) (*MutationLock, error) {
	return acquireMutationLockFor(home, mutationWait)
}

// acquireMutationLockFor 是可注入超时的实现（测试用短超时验证互斥与超时路径）。
func acquireMutationLockFor(home paths.Home, wait time.Duration) (*MutationLock, error) {
	f, err := os.OpenFile(home.MutationLockFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("app: open mutation.lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &MutationLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("app: flock mutation.lock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("app: another mutation (update/use/add) is in progress; waited %s", wait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Release 解锁并关闭 fd（锁文件保留在磁盘上，见 AcquireMutationLock 注释）。
func (l *MutationLock) Release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// ErrMutationBusy 是 TryAcquireMutationLock 的忙哨兵（watcher 据此跳过本 tick）。
var ErrMutationBusy = errors.New("app: another mutation is in progress")

// TryAcquireMutationLock 以 flock LOCK_EX|LOCK_NB 非阻塞获取 mutation.lock（F15：
// flock 支持 LOCK_NB）；忙=返回 ErrMutationBusy——watcher tick 让路语义（绝不等待，
// 批④评审 P1-2 并发防护第一件套）。与 acquireMutationLockFor 同一锁文件、同一
// flock 语义，锁文件常驻不删除。
func TryAcquireMutationLock(home paths.Home) (*MutationLock, error) {
	f, err := os.OpenFile(home.MutationLockFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("app: open mutation.lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrMutationBusy
		}
		return nil, fmt.Errorf("app: flock mutation.lock: %w", err)
	}
	return &MutationLock{f: f}, nil
}
