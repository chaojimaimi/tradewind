package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// reload_service_test.go：TUN 形态热重载走服务 reload 动作（批⑥b §4.4）。
// 全部无特权：代理入口/锚/客户端工厂三个缝隙都指向短路径桩与可编程假客户端。

// progTunService 可编程假客户端：记录被请求的超时档与调用序列，Reload 应答与
// 传输错误可注入（Stop/Start 记录供降级路径断言）。
type progTunService struct {
	mu        sync.Mutex
	timeouts  []time.Duration
	calls     []string
	starts    []int
	stops     int
	reloadErr error
	reloadRes service.Response
}

// client 是 tunNewClient 缝隙的实现：记录超时档，返回自身。
func (p *progTunService) client(timeout time.Duration) (tunServiceClientT, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.timeouts = append(p.timeouts, timeout)
	return p, nil
}

func (p *progTunService) Start(gen int) (service.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "start")
	p.starts = append(p.starts, gen)
	return service.Response{OK: true, PID: 43000}, nil
}

func (p *progTunService) Stop() (service.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "stop")
	p.stops++
	return service.Response{OK: true}, nil
}

func (p *progTunService) Status() (service.Response, error) {
	return service.Response{OK: true, PID: 43000, Version: "fake"}, nil
}

func (p *progTunService) Reload() (service.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "reload")
	if p.reloadErr != nil {
		return service.Response{}, p.reloadErr
	}
	return p.reloadRes, nil
}

func (p *progTunService) reloadCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.countOf("reload")
}

func (p *progTunService) stopCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stops
}

func (p *progTunService) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.starts)
}

func (p *progTunService) countOf(step string) int {
	n := 0
	for _, c := range p.calls {
		if c == step {
			n++
		}
	}
	return n
}

func (p *progTunService) callSequence() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.calls, ",")
}

func (p *progTunService) timeoutsRecorded() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Duration{}, p.timeouts...)
}

// stubReloadSeams 装好 reload 用例的三个缝隙：锚→存在的临时文件、代理入口→短路径
// 桩（是否监听由用例决定）、客户端工厂→可编程假客户端。
func stubReloadSeams(t *testing.T, svc *progTunService) {
	t.Helper()
	anchor := filepath.Join(stubSocketDir(t), "service.json")
	if err := os.WriteFile(anchor, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pointTunAnchorAt(t, anchor)
	stubBrokerSockPath(t)
	old := tunNewClient
	tunNewClient = svc.client
	t.Cleanup(func() { tunNewClient = old })
}

// seedTunFormSession 写 TUN 会话基线：settings 与 state.Core.Tun 均为 true。
func seedTunFormSession(t *testing.T, home paths.Home) {
	t.Helper()
	seedRuntimeInputs(t, home)
	settings := store.DefaultSettings()
	settings.TunEnabled = true
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	saveTunState(t, home, true)
}

// hitCountingStub 在 sockPath 上起一个记录请求数的 /version 桩，返回读计数的函数
// （断言"用户态 sock 上没有任何请求"用）。
func hitCountingStub(t *testing.T, sockPath string) func() int {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(`{"version":"stub"}`))
	})
	listenHandlerAt(t, sockPath, mux)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// TestReloadViaServicePushesAndWarnsIgnored 正向流：假客户端 Reload 恰一次、请求的
// 超时档恰为 service.ReloadTimeout；Ignored 以 warning 行带键名输出 + core
// hot-reloaded；用户态 sock 上零请求。
func TestReloadViaServicePushesAndWarnsIgnored(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunFormSession(t, home)
	svc := &progTunService{reloadRes: service.Response{OK: true, Ignored: []string{"proxy-providers.sub1.path"}}}
	stubReloadSeams(t, svc)
	listenVersionStubAt(t, tunBrokerSockPath) // 代理入口可达 = 内核在跑
	userHits := hitCountingStub(t, home.SockFile())

	var progress strings.Builder
	if err := ReloadIfRunningW(context.Background(), home, &progress); err != nil {
		t.Fatalf("ReloadIfRunningW: %v", err)
	}
	if got := svc.reloadCount(); got != 1 {
		t.Fatalf("reload calls = %d, want exactly 1", got)
	}
	timeouts := svc.timeoutsRecorded()
	if len(timeouts) != 1 || timeouts[0] != service.ReloadTimeout {
		t.Fatalf("client timeouts = %v, want exactly [%v]", timeouts, service.ReloadTimeout)
	}
	out := progress.String()
	for _, want := range []string{"warning:", "proxy-providers.sub1.path", "core hot-reloaded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("progress = %q, want it to carry %q", out, want)
		}
	}
	if got := userHits(); got != 0 {
		t.Fatalf("user-mode sock hits = %d, want 0 (TUN form never dials the user sock)", got)
	}
}

// TestReloadViaServiceRejectedDoesNotDegrade rejected → 报错带「配置被 TUN 模式拒绝」
// 与原因；零 Stop、零 Start（同配置重启也会被拒，重启只会把内核弄停）。
func TestReloadViaServiceRejectedDoesNotDegrade(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunFormSession(t, home)
	svc := &progTunService{reloadRes: service.Response{
		OK: false, Rejected: true,
		Error: "service: config rejected at external-ui: is not allowed in TUN mode",
	}}
	stubReloadSeams(t, svc)
	listenVersionStubAt(t, tunBrokerSockPath)

	err := ReloadIfRunningW(context.Background(), home, os.Stdout)
	if err == nil {
		t.Fatal("a rejected reload must return an error")
	}
	for _, want := range []string{"配置被 TUN 模式拒绝", "external-ui", "is not allowed in TUN mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to carry %q", err, want)
		}
	}
	if svc.stopCount() != 0 || svc.startCount() != 0 {
		t.Fatalf("rejected reload must not degrade: stops=%d starts=%d", svc.stopCount(), svc.startCount())
	}
}

// TestReloadViaServiceOtherFailureDoesNotDegrade 其它 !OK（服务侧问题）→ 报错、
// 零 Stop/Start（重启无助于服务侧问题）。
func TestReloadViaServiceOtherFailureDoesNotDegrade(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunFormSession(t, home)
	svc := &progTunService{reloadRes: service.Response{OK: false, Error: "service: runtime: disk full"}}
	stubReloadSeams(t, svc)
	listenVersionStubAt(t, tunBrokerSockPath)

	err := ReloadIfRunningW(context.Background(), home, os.Stdout)
	if err == nil || !strings.Contains(err.Error(), "tun service reload failed: service: runtime: disk full") {
		t.Fatalf("err = %v, want the service-side failure surfaced verbatim", err)
	}
	if svc.stopCount() != 0 || svc.startCount() != 0 {
		t.Fatalf("service-side failure must not degrade: stops=%d starts=%d", svc.stopCount(), svc.startCount())
	}
}

// TestReloadViaServiceTransportFailureDegrades 传输层失败 → 既有降级：先 Stop 后
// Start（generation+1），输出含 degrading to core restart。
func TestReloadViaServiceTransportFailureDegrades(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunFormSession(t, home) // state.Core.Generation=1
	svc := &progTunService{reloadErr: errors.New("service: dial: connection refused")}
	stubReloadSeams(t, svc)
	listenVersionStubAt(t, tunBrokerSockPath) // 降级重启后的 /version 轮询在此收口

	var progress strings.Builder
	if err := ReloadIfRunningW(context.Background(), home, &progress); err != nil {
		t.Fatalf("transport failure must degrade to a successful restart: %v", err)
	}
	if seq := svc.callSequence(); seq != "reload,stop,start" {
		t.Fatalf("call sequence = %q, want reload,stop,start (degrade after transport failure)", seq)
	}
	if len(svc.starts) != 1 || svc.starts[0] != 2 {
		t.Fatalf("degrade start gens = %v, want [2] (state generation 1 + 1)", svc.starts)
	}
	if out := progress.String(); !strings.Contains(out, "degrading to core restart") {
		t.Fatalf("progress = %q, want the degrade notice", out)
	}
}

// TestReloadViaServiceSkipsWhenCoreDown 代理入口无监听（内核未运行）→ 返回 nil
// （仅落盘语义）、零 Reload。
func TestReloadViaServiceSkipsWhenCoreDown(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunFormSession(t, home)
	svc := &progTunService{}
	stubReloadSeams(t, svc) // 桩路径无监听 = 内核未运行

	if err := ReloadIfRunningW(context.Background(), home, os.Stdout); err != nil {
		t.Fatalf("ReloadIfRunningW with core down: %v", err)
	}
	if got := svc.reloadCount(); got != 0 {
		t.Fatalf("reload calls = %d, want 0 (core down → only persist)", got)
	}
}

// TestReloadUserFormNeverCallsService 用户态会话（state.Core.Tun=false）→ 假客户端
// 零 Reload（非 TUN 路径零回归，靠既有用例全绿共同佐证）。
func TestReloadUserFormNeverCallsService(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home) // settings/state 均默认（非 TUN 形态），无 sock = 未运行
	svc := &progTunService{}
	stubReloadSeams(t, svc)

	if err := ReloadIfRunningW(context.Background(), home, os.Stdout); err != nil {
		t.Fatalf("ReloadIfRunningW (user form, core down): %v", err)
	}
	if got := svc.reloadCount(); got != 0 {
		t.Fatalf("reload calls = %d, want 0 (user form never asks the service)", got)
	}
}
