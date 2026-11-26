package service

// broker_lifecycle_test.go：代理入口与服务生命周期的编排用例（批⑥b §4.3）——随
// start 起落（spawnCore 里内核握手成功才监听）、stop/Run 关停摘除、内核崩溃自愈
// 不必重建代理（按请求拨号）、broker 起不来时整体失败不带病运行。用例走既有的
// writeServiceFixture / startStubServer / 桩内核 fakecore。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
)

// TestServiceStartBringsUpBrokerAndStopRemovesIt Start 后代理入口在 conf.BrokerSock
// 上监听（0666）、/version 经它得桩内核版本、PUT /configs 被闸拒绝；Stop 后路径
// 不存在；服务 status 照常应答。
func TestServiceStartBringsUpBrokerAndStopRemovesIt(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	if resp, err := cl.Start(1); err != nil || !resp.OK || resp.PID <= 0 {
		t.Fatalf("Start = %+v err %v, want ok", resp, err)
	}
	info, err := os.Stat(conf.BrokerSock)
	if err != nil {
		t.Fatalf("broker sock must exist after start: %v", err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("broker sock perm = %o, want 0666", info.Mode().Perm())
	}
	kc := core.NewClient(conf.BrokerSock, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	v, err := kc.Version(ctx)
	cancel()
	if err != nil || v != "fake-v1.0.0" {
		t.Fatalf("Version via broker = %q err %v, want fake-v1.0.0", v, err)
	}
	status, body := brokerDo(t, conf.BrokerSock, "PUT", "/configs", []byte(`{"port":7890}`))
	if status != http.StatusForbidden || body != brokerBlockedBody {
		t.Fatalf("PUT /configs via broker = %d %q, want 403 %q", status, body, brokerBlockedBody)
	}

	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(conf.BrokerSock); !os.IsNotExist(err) {
		t.Fatalf("broker sock must be removed after stop (err = %v)", err)
	}
	if st, err := cl.Status(); err != nil || !st.OK || st.PID != 0 {
		t.Fatalf("Status after stop = %+v err %v, want ok pid=0", st, err)
	}
}

// TestServiceBrokerSurvivesSelfHeal SIGKILL 桩内核（只杀自己拉起的）→ Monitor 自愈
// respawn → 代理路径仍在（按请求拨号，自愈不重建）、/version 经它仍应答。
func TestServiceBrokerSurvivesSelfHeal(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK || first.PID <= 0 {
		t.Fatalf("initial Start = %+v err %v", first, err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub core: %v", err)
	}
	// 等自愈 respawn：core.pid 指向新的存活内核（判据同 TestSelfHealRestoresRuntimeFromSnapshot）
	healed := false
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(cfg.PIDPath); err == nil {
			var pid int
			if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); perr == nil &&
				pid > 0 && pid != first.PID && processAlive(pid) && probeAlive(conf.KernelSock) {
				healed = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healed {
		t.Fatal("self-heal respawn did not complete within 12s")
	}
	if _, err := os.Stat(conf.BrokerSock); err != nil {
		t.Fatalf("broker sock must survive the self-heal: %v", err)
	}
	// /version 经代理仍应答（新内核的原生 socket 在同一路径重新监听，代理按请求拨号）
	kc := core.NewClient(conf.BrokerSock, "")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		v, err := kc.Version(ctx)
		cancel()
		if err == nil && v == "fake-v1.0.0" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("/version via the broker did not answer after the self-heal respawn")
}

// TestServiceBrokerStartFailureStopsCore 代理入口路径上预放一个普通文件 →
// Start 整体失败（OK:false、错误点名 broker）、内核已停、core.pid 清理、文件原封
// 不动（fail-closed，不带病运行）；清掉障碍后同代际重试成功。
func TestServiceBrokerStartFailureStopsCore(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	if err := os.WriteFile(conf.BrokerSock, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	resp, err := cl.Start(1)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "broker") {
		t.Fatalf("Start = %+v err %v, want ok:false with an error naming the broker", resp, err)
	}
	if probeAlive(conf.KernelSock) {
		t.Fatal("no core may stay alive when the broker cannot start")
	}
	if _, err := os.Stat(cfg.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("core.pid must be removed (err = %v)", err)
	}
	if data, rerr := os.ReadFile(conf.BrokerSock); rerr != nil || string(data) != "sentinel" {
		t.Fatalf("pre-existing file = %q err %v, want the sentinel untouched", data, rerr)
	}
	// 失败路径不消耗代际（curGen 只在 spawn 成功后前进）：清掉障碍后同一代际可重试
	if err := os.Remove(conf.BrokerSock); err != nil {
		t.Fatal(err)
	}
	if resp, err := cl.Start(1); err != nil || !resp.OK || resp.PID <= 0 {
		t.Fatalf("Start after clearing the obstacle = %+v err %v, want ok", resp, err)
	}
}

// TestServiceShutdownRemovesBroker 自己起 srv.Run(ctx)（写法同 startStubServerTuned）：
// Start 后代理路径存在；取消 ctx、Run 返回后代理路径不存在。
func TestServiceShutdownRemovesBroker(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(cfg)
	allowTrust(srv)
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx) }()
	// 用例中途失败也要收干净：取消并等 Run 返回（它负责停掉桩内核）
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("service did not shut down within 15s")
		}
	})
	waitForFile(t, cfg.SockPath, 5*time.Second)

	cl := NewClient(cfg.SockPath, 30*time.Second)
	if resp, err := cl.Start(1); err != nil || !resp.OK || resp.PID <= 0 {
		t.Fatalf("Start = %+v err %v, want ok", resp, err)
	}
	waitForFile(t, conf.BrokerSock, 5*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("service did not shut down within 15s")
	}
	if _, err := os.Stat(conf.BrokerSock); !os.IsNotExist(err) {
		t.Fatalf("broker sock must be removed after shutdown (err = %v)", err)
	}
}
