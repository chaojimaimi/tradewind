package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// PreSpawn 钩子（batch6 review P2）：特权服务以它复核执行目录归属。要害在第二条
// spawn 路径——Monitor 崩溃自愈在 Manager 内部直接 respawn，不经过服务的
// spawnCore；钩子保证初始 spawn 与自愈 respawn 走同一道闸。

// TestSpawnPreSpawnHookRejects 钩子拒绝 → Spawn 不拉子进程，且先于日志打开等
// 一切副作用；错误携带钩子原因。
func TestSpawnPreSpawnHookRejects(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	cfg.PreSpawn = func() error { return errors.New("exec dir tampered") }
	m := NewManager(cfg)

	err := m.Spawn()
	if err == nil || !strings.Contains(err.Error(), "exec dir tampered") {
		t.Fatalf("Spawn with a refusing PreSpawn = %v, want the hook error surfaced", err)
	}
	if pid := m.PID(); pid != 0 {
		_ = m.Stop()
		t.Fatalf("no core may be spawned when PreSpawn refuses (pid %d)", pid)
	}
	if _, err := os.Stat(cfg.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hook must run before any side effect such as opening the log (err = %v)", err)
	}
}

// TestSpawnPreSpawnHookPasses 钩子放行 → 照常 spawn + 握手；每次 Spawn 恰好调用一次。
func TestSpawnPreSpawnHookPasses(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	var calls atomic.Int32
	cfg.PreSpawn = func() error { calls.Add(1); return nil }
	m := NewManager(cfg)

	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("PreSpawn calls = %d, want exactly 1 per Spawn", calls.Load())
	}
	_ = m.Stop()
}

// TestMonitorRespawnRunsPreSpawnHook 崩溃自愈的内部 respawn 同样先过钩子：内核崩溃
// 后钩子拒绝 → 放弃重启并回调 onGiveUp（fail-closed），绝不经被篡改的路径执行。
func TestMonitorRespawnRunsPreSpawnHook(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "crash-after-ms", "FAKE_CORE_CRASH_MS=800")
	cfg.RestartBackoff = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}
	var trusted atomic.Bool
	trusted.Store(true)
	var calls atomic.Int32
	cfg.PreSpawn = func() error {
		calls.Add(1)
		if !trusted.Load() {
			return errors.New("exec dir tampered")
		}
		return nil
	}
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("initial Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("initial handshake: %v", err)
	}
	// 初始握手后执行目录被换掉（评审 P2 场景），内核随后崩溃
	trusted.Store(false)
	gaveUp := make(chan error, 1)
	err := m.Monitor(context.Background(), func(e error) { gaveUp <- e })
	if err == nil || !strings.Contains(err.Error(), "pre-spawn check") || !strings.Contains(err.Error(), "exec dir tampered") {
		t.Fatalf("Monitor = %v, want give-up carrying the PreSpawn refusal", err)
	}
	select {
	case <-gaveUp:
	case <-time.After(5 * time.Second):
		t.Fatal("onGiveUp not called")
	}
	if calls.Load() != 2 {
		t.Fatalf("PreSpawn calls = %d, want 2 (initial spawn + the refused respawn attempt)", calls.Load())
	}
}
