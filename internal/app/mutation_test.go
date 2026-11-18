package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestMutationLockExclusive(t *testing.T) {
	// 已持锁时第二次获取必须失败（同进程不同 fd 的 flock 也互斥）
	home := paths.Home(stubSocketDir(t))
	first, err := acquireMutationLockFor(home, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	if _, err := acquireMutationLockFor(home, 300*time.Millisecond); err == nil {
		t.Fatal("second acquire while held: want error")
	}
}

func TestMutationLockBoundedWaitNotEternal(t *testing.T) {
	// 有界等待：短超时必须快速失败，绝不挂起 35s（M2-plan §2.2）
	home := paths.Home(stubSocketDir(t))
	first, err := acquireMutationLockFor(home, 5*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	start := time.Now()
	_, err = acquireMutationLockFor(home, 300*time.Millisecond)
	if err == nil {
		t.Fatal("contended acquire: want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("bounded wait took %s, want ~300ms", elapsed)
	}
	if !strings.Contains(err.Error(), "another mutation") {
		t.Fatalf("timeout error = %v, want explicit contention message", err)
	}
}

func TestMutationLockReleaseAllowsReacquire(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	first, err := acquireMutationLockFor(home, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.Release()
	second, err := acquireMutationLockFor(home, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	second.Release()
}

func TestUseReleasesMutationLock(t *testing.T) {
	// Use 全程持锁，完成后必须释放（Add/Update 的锁路径已由其各自既有测试间接覆盖）
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	if err := Use(context.Background(), home, "second sub"); err != nil {
		// 无内核 → ReloadIfRunning 静默跳过，切换本身必须成功
		t.Fatalf("Use without core: %v", err)
	}
	if _, err := acquireMutationLockFor(home, 300*time.Millisecond); err != nil {
		t.Fatalf("lock must be free after Use completes: %v", err)
	}
}
