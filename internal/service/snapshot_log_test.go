package service

// snapshot_log_test.go：两条回归（批⑥b c3b 主代理复查补）：L5 对非常规文件不阻塞；
// 内核的拒绝理由只回给调用方、不进服务日志（I6）。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRestoreRuntimeReplacesNonRegularFile：R 被换成 FIFO（<core_home> 里只有 root
// 能做）→ 当场按快照重写，且不得阻塞。裸 os.ReadFile 打开无写端的 FIFO 会永远等
// 下去，而这条路径在 start 时持着 s.mu——一次阻塞就是整个服务卡死。
func TestRestoreRuntimeReplacesNonRegularFile(t *testing.T) {
	dir := shortTempDir(t)
	conf := seedFixture(t, dir)
	srv := NewServer(ServerConfig{})
	srv.conf = conf
	snap := minimalProduct(t)
	srv.setSnapshot(snap)
	runtimePath := RuntimeIn(conf.CoreHome)
	if err := syscall.Mkfifo(runtimePath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.restoreRuntime(conf) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("restoreRuntime: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restoreRuntime blocked: a FIFO at the runtime path must be rejected, not read")
	}
	info, err := os.Lstat(runtimePath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("runtime path must be a regular file again, got %v err %v", info, err)
	}
	got, err := os.ReadFile(runtimePath)
	if err != nil || !bytes.Equal(got, snap) {
		t.Fatalf("R = %q err %v, want the snapshot", got, err)
	}
}

// TestKernelRejectionReasonStaysOutOfTheLog：内核 400 的理由可能回显配置值（yaml
// 类型错误会带出片段）——它回给调用方（那是用户自己的配置），但不进服务日志。
func TestKernelRejectionReasonStaysOutOfTheLog(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	startStubServerTuned(t, cfg, withCoreEnv("FAKE_CORE_MODE=reject-config"))
	cl := NewClient(cfg.SockPath, 30*time.Second)
	if r, err := cl.Start(1); err != nil || !r.OK {
		t.Fatalf("Start = %+v err %v, want ok", r, err)
	}
	logPath := filepath.Join(dir, "service-stderr.log")
	captureFd2(t, logPath)

	resp, err := cl.Reload()
	if err != nil || resp.OK || !resp.Rejected {
		t.Fatalf("Reload = %+v err %v, want rejected", resp, err)
	}
	if !strings.Contains(resp.Error, "fakecore: config rejected") {
		t.Fatalf("the caller must receive the kernel's reason, got %q", resp.Error)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "fakecore: config rejected") {
		t.Fatalf("kernel reason leaked into the service log:\n%s", log)
	}
	if !strings.Contains(string(log), "reload refused by kernel (status 400)") {
		t.Fatalf("the log must record the refusal by status code:\n%s", log)
	}
}

// captureFd2 把进程的 fd 2（服务日志的实际去向：launchd 把它接到 service.log）重定向
// 到 path，Cleanup 时还原。WHY 在 fd 层截获而不是替换 os.Stderr 变量：服务协程
// （acceptLoop/monitorLoop）随时可能读 os.Stderr 写日志，换变量与它们构成数据竞争；
// dup2 之后各协程照常经 fd 2 写入，落到文件。包内测试串行执行，不与 t.Parallel 混用。
func captureFd2(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := syscall.Dup2(int(f.Fd()), 2); err != nil {
		syscall.Close(saved)
		f.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Dup2(saved, 2)
		_ = syscall.Close(saved)
		_ = f.Close()
	})
}
