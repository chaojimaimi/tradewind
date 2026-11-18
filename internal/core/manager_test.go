package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSpawnHandshakeSuccessAndStop(t *testing.T) {
	dir := shortTempDir(t)
	m := NewManager(stubConfig(t, dir, "normal"))
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if m.PID() <= 0 {
		t.Fatalf("PID = %d, want > 0", m.PID())
	}
	// 握手成功后 sock 权限应收敛为 600（mihomo 建监听时为 666）
	info, err := os.Stat(m.cfg.SockPath)
	if err != nil {
		t.Fatalf("stat sock: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sock perm = %o, want 600", info.Mode().Perm())
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(m.cfg.SockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sock should be removed after Stop, err = %v", err)
	}
	if !m.exitClosed() {
		t.Fatal("process should have exited after Stop")
	}
}

func TestHandshakeFailFastOnEarlyExit(t *testing.T) {
	// 桩内核打印 "address already in use" 后立即退出 → 必须快速失败并给出端口占用提示
	dir := shortTempDir(t)
	m := NewManager(stubConfig(t, dir, "exit-immediately"))
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	start := time.Now()
	err := m.WaitReady(context.Background())
	if err == nil {
		t.Fatal("WaitReady should fail")
	}
	if elapsed := time.Since(start); elapsed > DefaultHandshakeTimeout {
		t.Fatalf("fail-fast took %s, should not wait full timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "address already in use") ||
		!strings.Contains(err.Error(), "hint") {
		t.Fatalf("err should contain log tail and port hint, got: %v", err)
	}
	_ = m.Stop()
}

func TestHandshakeTimeout(t *testing.T) {
	// 桩内核挂起但不建 sock → 握手超时报错（可配置短超时以加速测试）
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "no-sock")
	cfg.HandshakeTimeout = 400 * time.Millisecond
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	err := m.WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "handshake timeout") {
		t.Fatalf("err = %v, want handshake timeout", err)
	}
	_ = m.Stop()
}

func TestCrashRestartThenGiveUp(t *testing.T) {
	// 桩内核服务 300ms 后崩溃 → 3 次退避重启全部失败 → onGiveUp 被调用且 Monitor 返回错误
	//（300ms > 握手轮询间隔 150ms，保证每次重启后的握手都能成功、随后再崩）
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "crash-after-ms", "FAKE_CORE_CRASH_MS=300")
	cfg.RestartBackoff = []time.Duration{30 * time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond}
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("initial handshake: %v", err)
	}
	gaveUp := make(chan error, 1)
	err := m.Monitor(context.Background(), func(e error) { gaveUp <- e })
	if err == nil {
		t.Fatal("Monitor should return error after exhausting restarts")
	}
	select {
	case e := <-gaveUp:
		if !strings.Contains(e.Error(), "restart failed") {
			t.Fatalf("giveup err = %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onGiveUp not called")
	}
	_ = m.Stop()
}

func TestMonitorReturnsNilOnContextCancel(t *testing.T) {
	// 正常运行中收到 ctx 取消（模拟信号）→ Monitor 返回 nil，不触发 giveup
	dir := shortTempDir(t)
	m := NewManager(stubConfig(t, dir, "normal"))
	if err := m.Spawn(); err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Monitor(ctx, func(e error) { t.Errorf("unexpected giveup: %v", e) }) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Monitor = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Monitor did not return after cancel")
	}
	_ = m.Stop()
}
