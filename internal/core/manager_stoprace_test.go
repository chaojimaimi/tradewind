package core

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Stop 与 Monitor 退避重启的竞争（回归）：内核崩溃后 Monitor 进入退避等待，此时
// Owner 调 Stop——修复前退避结束后无条件 Spawn，复活一个调用方已丢弃引用的孤儿
// 内核（特权形态 = 无人管理的 root 内核，后续启动端口冲突）。

// pidLog 按序记录每次 spawn 的 pid（并发安全）。
// WHY 不用 m.PID() 判"是否复活"：PID() 在进程死后仍返回旧 pid，回调计数才可信。
type pidLog struct {
	mu   sync.Mutex
	pids []int
}

func (l *pidLog) onSpawned(pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pids = append(l.pids, pid)
}

func (l *pidLog) snapshot() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int{}, l.pids...)
}

// processAlive 以 kill(pid,0) 判存活：nil = 存活；进程被 Wait 收割后为 ESRCH。
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestStopDuringBackoffPreventsResurrect 回归主用例：崩溃 → 退避窗口内 Stop →
// Monitor 必须安静收场（返回 nil、不触发 onGiveUp——主动停止不是重启失败），
// 且退避窗口过后无任何新内核、sock 不复活。
func TestStopDuringBackoffPreventsResurrect(t *testing.T) {
	dir := shortTempDir(t)
	// crash 300ms：与既有用例同理，> 握手轮询间隔 150ms，保证首次握手能成功
	cfg := stubConfig(t, dir, "crash-after-ms", "FAKE_CORE_CRASH_MS=300")
	// 退避窗口 800ms：给 Stop 留出足够落点（Stop 必须落在窗口内才有竞争）
	cfg.RestartBackoff = []time.Duration{800 * time.Millisecond}
	var spawned pidLog
	cfg.OnSpawned = spawned.onSpawned
	m := NewManager(cfg)
	t.Cleanup(func() { _ = m.Stop() })

	if err := m.Spawn(); err != nil {
		t.Fatalf("initial Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("initial handshake: %v", err)
	}
	monDone := make(chan error, 1)
	go func() {
		monDone <- m.Monitor(context.Background(), func(e error) { t.Errorf("unexpected onGiveUp: %v", e) })
	}()
	// 等崩溃退出；再多留一拍确保 Monitor 已进入退避 select，Stop 才落在窗口内
	deadline := time.Now().Add(3 * time.Second)
	for !m.exitClosed() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !m.exitClosed() {
		t.Fatal("stub core did not crash within 3s")
	}
	time.Sleep(100 * time.Millisecond)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-monDone:
		if err != nil {
			t.Fatalf("Monitor = %v, want nil (a deliberate Stop is not a restart failure)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Monitor did not return after Stop during backoff")
	}
	// Monitor 返回 = 退避窗口已越过；再留余量给"错误复活"暴露时间
	time.Sleep(250 * time.Millisecond)
	pids := spawned.snapshot()
	if len(pids) != 1 {
		// 走到这里只有一种合法解释：机器调度停顿让 Stop 晚于退避终点，respawn 抢先
		// 进了临界区——那一次 spawn 是合法的，但必须已经被 Stop 收割（修复的另一侧
		// 保证）。修复前的缺陷形态是 spawn 出来后无人收割且 Monitor 以 giveUp 收场，
		// 会在上面的 Monitor 返回值断言处先失败。
		for i, pid := range pids[1:] {
			if processAlive(pid) {
				t.Fatalf("respawn #%d (pid %d) is alive after Stop — a resurrected core escaped its owner", i+2, pid)
			}
		}
		t.Logf("timing note: Stop landed after the backoff window (%d spawns); each extra one was reaped by Stop", len(pids))
	} else if !m.exitClosed() {
		t.Fatal("exit must stay closed — no new core may be tracked")
	}
	if processAlive(m.PID()) {
		t.Fatalf("pid %d still alive after Stop; a resurrected core escaped its owner", m.PID())
	}
	if _, err := os.Stat(cfg.SockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sock must not reappear after Stop, err = %v", err)
	}
}

// TestMonitorSelfHealStillWorks 对照用例：同样崩溃但无人 Stop → 修复不得破坏正常
// 自愈——退避后 respawn 成功（OnSpawned 两次、新进程存活）。
func TestMonitorSelfHealStillWorks(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "crash-after-ms", "FAKE_CORE_CRASH_MS=500")
	cfg.RestartBackoff = []time.Duration{100 * time.Millisecond, 100 * time.Millisecond}
	var spawned pidLog
	cfg.OnSpawned = spawned.onSpawned
	m := NewManager(cfg)
	t.Cleanup(func() { _ = m.Stop() })

	if err := m.Spawn(); err != nil {
		t.Fatalf("initial Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("initial handshake: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monDone := make(chan error, 1)
	go func() { monDone <- m.Monitor(ctx, func(e error) { t.Errorf("unexpected onGiveUp: %v", e) }) }()

	// 等自愈 respawn 落地（第 2 次 OnSpawned）；crash 500ms > 轮询+断言耗时，窗口充足
	deadline := time.Now().Add(3 * time.Second)
	for len(spawned.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	pids := spawned.snapshot()
	if len(pids) != 2 {
		t.Fatalf("OnSpawned calls = %d (%v), want exactly 2 (initial + self-heal respawn)", len(pids), pids)
	}
	if !processAlive(pids[1]) {
		t.Fatalf("respawned pid %d not alive — self-heal broken", pids[1])
	}
	cancel()
	select {
	case err := <-monDone:
		if err != nil {
			t.Fatalf("Monitor = %v, want nil on ctx cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Monitor did not return after ctx cancel")
	}
}
