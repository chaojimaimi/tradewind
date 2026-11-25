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
// 日志截获必须在起服务**之前**就位（见 captureServiceLog）。
func TestKernelRejectionReasonStaysOutOfTheLog(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	logPath := filepath.Join(dir, "service-stderr.log")
	captureServiceLog(t, logPath)
	startStubServerTuned(t, cfg, withCoreEnv("FAKE_CORE_MODE=reject-config"))
	cl := NewClient(cfg.SockPath, 30*time.Second)
	if r, err := cl.Start(1); err != nil || !r.OK {
		t.Fatalf("Start = %+v err %v, want ok", r, err)
	}

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

// captureServiceLog 把服务日志（代码里一律写 os.Stderr）引到 path，Cleanup 时还原。
// 必须在起服务之前调用。
//
// WHY 换变量而不是重定向 fd 2：`go test -json` 下 testing 包会把 os.Stderr 指到
// os.Stdout（testing.go，go.dev/issue/33419——两条流共用一把写锁，免得交错输出
// 搞乱 test2json）。此后服务经 os.Stderr 的写根本不走 fd 2，只在 fd 层重定向的
// 截获在这种模式下截不到任何东西——带 -json 跑时本用例曾必失败。
//
// WHY 这样换不构成数据竞争：只在没有任何服务协程存活的时刻触碰这个变量。本函数
// 的 Cleanup 先于 startStubServer* 的收尾登记，因而晚于它执行（LIFO），那时 Run
// 已经返回；本用例起的服务协程都在换完之后才创建。服务运行期间换变量才会与
// acceptLoop/monitorLoop 的读竞争——那是当初只在 fd 层做的原因。包内测试串行执行，
// 不与 t.Parallel 混用。
func captureServiceLog(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = prev
		if err := f.Close(); err != nil {
			t.Errorf("close captured service log: %v", err)
		}
	})
}
