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

// TestSockPermParameterized SockPerm 参数化（批⑥ §3.1 评审 P1-4）：Config.SockPerm=0666
// → 握手成功后 sock 权限收敛为 0666（特权服务侧 root 属主 sock，用户态 CLI 直连必需）；
// 默认零值仍是 600（TestSpawnHandshakeSuccessAndStop 零回归锚）。
func TestSockPermParameterized(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	cfg.SockPerm = 0o666
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	info, err := os.Stat(m.cfg.SockPath)
	if err != nil {
		t.Fatalf("stat sock: %v", err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("sock perm = %o, want 666 (SockPerm parameterized)", info.Mode().Perm())
	}
	_ = m.Stop()
}

// TestMonitorRestartKeepsSockPerm Monitor 自愈路径 SockPerm 贯通（评审 P1-4 要求的
// "Monitor 重启后权限保持"断言）：Monitor 崩溃重启内部再走 Spawn+WaitReady
// （manager.go 同一代码路径）——本测试以"崩溃退出后二次 Spawn+WaitReady"确定性
// 复现该路径：同参数 SockPerm 必须保持 0666，否则服务端自愈后全线失联。
// WHY 不直接驱动 Monitor 循环观测：fakecore listen（原始权限）到 WaitReady chmod
// 之间存在 /version 已可达但权限未收敛的瞬态窗口，轮询断言会撞假阳性。
func TestMonitorRestartKeepsSockPerm(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "crash-after-ms", "FAKE_CORE_CRASH_MS=1000")
	cfg.SockPerm = 0o666
	m := NewManager(cfg)
	if err := m.Spawn(); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("first handshake: %v", err)
	}
	assertSockPerm(t, m.cfg.SockPath, 0o666)
	// 等待崩溃退出（crash-after-ms 模式 1s 后自退，exit 通道由 cmd.Wait 关闭）
	deadline := time.Now().Add(3 * time.Second)
	for !m.exitClosed() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !m.exitClosed() {
		t.Fatal("stub core did not crash within 3s")
	}
	// 二次 Spawn+WaitReady = Monitor 重启内部路径（同 Manager 同参数）
	if err := m.Spawn(); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if err := m.WaitReady(context.Background()); err != nil {
		t.Fatalf("respawn handshake: %v", err)
	}
	assertSockPerm(t, m.cfg.SockPath, 0o666)
	_ = m.Stop()
}

func assertSockPerm(t *testing.T, sockPath string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat sock: %v", err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("sock perm = %o, want %o", info.Mode().Perm(), want)
	}
}
