package app

// replay_stub_test.go：节点选择跨形态接力用例共用的夹具（批⑥b §4.8，§6 第 8 条）——
// 记录型桩内核、预算与告警输出两个缝隙的注入、存活的假内核二进制、上一会话的
// 预置。用例在 replay_test.go（重放规则与跨形态判定）与 replay_start_test.go
// （两个启动路径）。全部无特权：桩在 unix socket 上记录每个 PUT；凡是会走到 TUN
// 形态的路径都先桩上 tunBrokerSockPath，绝不触真锚目录。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// minimalProfileYAML 是 current 订阅文件的最小内容（Synthesize 要读 current 订阅）。
const minimalProfileYAML = "proxies:\n  - name: n\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n"

// proxyStub 桩内核：GET /proxies 返回注入的代理表，PUT 记录（组=节点，到达序）。
// 可按组注入 400/挂起、整体挂 PUT 或挂 GET；release 在 cleanup 时关闭，立即释放
// 挂起的 handler（不留越过用例存活的 goroutine）。
type proxyStub struct {
	mu      sync.Mutex
	proxies map[string]core.Proxy
	puts    []string
	hits    int
	getErr  bool
	hangGet bool
	hangAll bool
	fail    map[string]int
	hang    map[string]bool
	release chan struct{}
}

// newReplayStubAt 在指定 socket 路径上起桩内核（记录 PUT）；t.Cleanup 关停并释放。
func newReplayStubAt(t *testing.T, sockPath string) *proxyStub {
	t.Helper()
	s := &proxyStub{
		proxies: map[string]core.Proxy{},
		fail:    map[string]int{},
		hang:    map[string]bool{},
		release: make(chan struct{}),
	}
	listenHandlerAt(t, sockPath, s.handler())
	t.Cleanup(func() { close(s.release) })
	return s
}

// handler 组装桩的 HTTP 面：/version、GET /proxies、PUT /proxies/{组}。
func (s *proxyStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.hits++
		getErr, hangGet, proxies := s.getErr, s.hangGet, s.proxies
		s.mu.Unlock()
		if hangGet { // 挂到测试预算耗尽为止（release 在 cleanup 关闭）
			select {
			case <-s.release:
			case <-time.After(60 * time.Second):
			}
			return
		}
		if getErr {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		body, err := json.Marshal(struct {
			Proxies map[string]core.Proxy `json:"proxies"`
		}{proxies})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(body)
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		if r.Method != http.MethodPut { // DELETE（unpin 链路）等一律 204
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := strings.TrimPrefix(r.URL.Path, "/proxies/")
		s.mu.Lock()
		doHang := s.hangAll || s.hang[name]
		forceFail := s.fail[name] > 0
		if forceFail {
			s.fail[name]--
		}
		if !doHang && !forceFail { // 挂起/强败的 PUT 内核侧未生效，不记录
			s.puts = append(s.puts, name+"="+body.Name)
		}
		s.mu.Unlock()
		if doHang {
			select {
			case <-s.release:
			case <-time.After(60 * time.Second):
			}
			return
		}
		if forceFail {
			http.Error(w, "nope", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (s *proxyStub) setProxies(m map[string]core.Proxy) { s.mu.Lock(); s.proxies = m; s.mu.Unlock() }
func (s *proxyStub) setGetErr(v bool)                   { s.mu.Lock(); s.getErr = v; s.mu.Unlock() }
func (s *proxyStub) setHangGet(v bool)                  { s.mu.Lock(); s.hangGet = v; s.mu.Unlock() }
func (s *proxyStub) setHangAll(v bool)                  { s.mu.Lock(); s.hangAll = v; s.mu.Unlock() }
func (s *proxyStub) failNext(group string)              { s.mu.Lock(); s.fail[group]++; s.mu.Unlock() }
func (s *proxyStub) hangNext(group string)              { s.mu.Lock(); s.hang[group] = true; s.mu.Unlock() }

// putsSnapshot 返回已记录 PUT 的快照（组=节点，到达序）。
func (s *proxyStub) putsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.puts...)
}

// hitsCount 返回桩收到的全部请求数（"另一形态路径零请求"断言用）。
func (s *proxyStub) hitsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// shortenReplayBudgets 缩短两个重放预算缝隙（超时/耗尽用例不等真实 2s/10s）。
func shortenReplayBudgets(t *testing.T, per, total time.Duration) {
	t.Helper()
	oldPer, oldTotal := replayPutTimeout, replayBudget
	replayPutTimeout, replayBudget = per, total
	t.Cleanup(func() { replayPutTimeout, replayBudget = oldPer, oldTotal })
}

// captureReplayWarnings 把告警输出缝隙换成 buffer 并返回读取函数。不在 fd 层重定向、
// 不替换 os.Stderr（go test -json 下 testing 包会把 os.Stderr 指到 os.Stdout）。
func captureReplayWarnings(t *testing.T) func() string {
	t.Helper()
	var buf strings.Builder
	old := replayWarnW
	replayWarnW = &buf
	t.Cleanup(func() { replayWarnW = old })
	return buf.String
}

// writeLingeringCoreBinary 写一个存活的假内核（while 循环壳）+ 匹配的 sha256
// sidecar（startCore→Spawn→VerifyBinary 需要可执行内核与校验文件；进程存活到
// WaitReady 在桩上轮询到 /version）。进程由 mgr.Stop() 回收。
func writeLingeringCoreBinary(t *testing.T, home paths.Home) {
	t.Helper()
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile :; do sleep 5; done\n"
	if err := os.WriteFile(home.Bin(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	if err := os.WriteFile(home.Sidecar(), []byte(hex.EncodeToString(sum[:])+"  "+home.Bin()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedFormSession 预置一次"上一会话"（state.last_form + 陈旧 Core 标记 + current
// 订阅及其 selected + 可合成的订阅文件）。
func seedFormSession(t *testing.T, lastForm string, coreTun bool, selected []store.ProxySelection) paths.Home {
	t.Helper()
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredStopped, PID: 0, Generation: 1, Tun: coreTun}
	st.LastForm = lastForm
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, home, selected)
	if err := store.SaveProfileFile(home, "old01.yaml", []byte(minimalProfileYAML)); err != nil {
		t.Fatal(err)
	}
	return home
}
