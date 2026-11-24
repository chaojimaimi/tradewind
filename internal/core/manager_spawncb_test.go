package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// LogOwner / OnSpawned（批⑥b §4.1）：
//   - LogOwner：openLogFile 之后对新打开的日志 fd 做 fchown(uid)+fchmod(0600)——
//     新 core.log 只在 Spawn() 里产生（初次启动或自愈 respawn 轮转，F21），收敛放
//     Manager 里两条 spawn 路径都覆盖；服务侧不再有按路径 chown。
//   - OnSpawned：cmd.Start 成功后回调 pid——服务在回调里重写 core.pid（自愈 respawn
//     不经过 spawnCore，F20）。两者为零值时行为与现状一致（既有用例全绿即证）。

func TestLogOwnerConvergesFreshAndPresetLog(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// 预置一份 0644 的旧日志：Spawn 以 O_APPEND 打开后必须收敛为 0600 + 当前 uid
	if err := os.WriteFile(cfg.LogPath, []byte("preset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	cfg.LogOwner = &uid
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	info, err := os.Stat(cfg.LogPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log perm = %o, want 0600 after LogOwner convergence", info.Mode().Perm())
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != uid {
		t.Fatalf("log owner = %v, want uid %d", sys, uid)
	}
	_ = m.Stop()
}

func TestLogOwnerForeignUIDFailsSpawn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chown to any uid; the EPERM path only exists non-root")
	}
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	foreign := os.Getuid() + 1 // 非本进程属主——fchown 必然 EPERM
	cfg.LogOwner = &foreign
	m := NewManager(cfg)
	if err := m.Spawn(); err == nil {
		_ = m.Stop()
		t.Fatal("Spawn must fail when the log fd cannot be chowned to the target uid")
	} else if !strings.Contains(err.Error(), "chown log") {
		t.Fatalf("err = %v, want a chown-log failure", err)
	}
	if pid := m.PID(); pid != 0 {
		t.Fatalf("no core may spawn behind a failed log convergence (pid %d)", pid)
	}
}

// TestOnSpawnedFiresPerSpawn 初始 Spawn 与 Monitor 自愈 respawn 各回调一次，pid
// 与 mgr.PID() 一致。
func TestOnSpawnedFiresPerSpawn(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "crash-after-ms", "FAKE_CORE_CRASH_MS=300")
	cfg.RestartBackoff = []time.Duration{30 * time.Millisecond, 30 * time.Millisecond}
	var mu sync.Mutex
	var pids []int
	cfg.OnSpawned = func(pid int) {
		mu.Lock()
		defer mu.Unlock()
		pids = append(pids, pid)
	}
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("initial Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("initial handshake: %v", err)
	}
	mu.Lock()
	if len(pids) != 1 || pids[0] != m.PID() {
		got := append([]int{}, pids...)
		mu.Unlock()
		t.Fatalf("OnSpawned after initial spawn = %v, want exactly [%d]", got, m.PID())
	}
	mu.Unlock()
	// 崩溃 → 退避 respawn → OnSpawned 再次回调（预算 2 次耗尽后 Monitor 返回错误）
	err := m.Monitor(context.Background(), func(error) {})
	if err == nil {
		t.Fatal("Monitor should exhaust the restart budget (stub crashes every 300ms)")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pids) < 2 {
		t.Fatalf("OnSpawned calls = %d, want >= 2 (initial + self-heal respawn)", len(pids))
	}
	if pids[len(pids)-1] != m.PID() {
		t.Fatalf("last OnSpawned pid = %d, want mgr.PID() = %d", pids[len(pids)-1], m.PID())
	}
	for i := 1; i < len(pids); i++ {
		if pids[i] == pids[i-1] {
			t.Fatalf("pids must differ across respawns: %v", pids)
		}
	}
	_ = m.Stop()
}

// TestZeroValueLogOwnerAndOnSpawnedKeepLegacyBehavior 两者为零值 → 与现状一致：
// 日志以 openLogFile 的 0600 创建、无收敛动作、无回调。既有用例全绿即证，此处
// 显式钉一条。
func TestZeroValueLogOwnerAndOnSpawnedKeepLegacyBehavior(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	info, err := os.Stat(cfg.LogPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fresh log perm = %v err = %v, want 0600", info.Mode().Perm(), err)
	}
	_ = m.Stop()
}
