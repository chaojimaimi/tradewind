package panel

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newStubBackend 在 unix socket 上起桩内核，返回 (sock 路径, 清理函数)。
func newStubBackend(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "twpx")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "core.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck // 测试桩
	return sock, func() { srv.Close(); ln.Close(); os.RemoveAll(dir) }
}

// newLiveGateway 构造指向桩内核、经 httptest 起真实监听的网关。
func newLiveGateway(t *testing.T, sock string) (*Gateway, *httptest.Server) {
	t.Helper()
	g := NewGateway(Config{Port: 39097, UIDir: t.TempDir(), Token: testToken, SockPath: sock, Secret: "gw-secret"})
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func TestProxyOverwritesAuthorizationUnconditionally(t *testing.T) {
	// 审计③核心：无论 UI 传什么（伪造 Bearer / 不传），内核侧恒见网关注入的 secret
	var mu sync.Mutex
	var gotAuth []string
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{"meta":true,"version":"stub"}`))
	})
	sock, cleanup := newStubBackend(t, mux)
	defer cleanup()
	_, srv := newLiveGateway(t, sock)

	for _, extra := range []map[string]string{
		{"Authorization": "Bearer forged-by-ui"},
		{},
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:39097"
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		// 走真实 server（经 cookie jar 语义由单测 header 模拟，Path=/ 由 gateway_test 回归）
		req.Header.Set("Cookie", cookieName+"="+testToken)
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status = %d", res.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotAuth) != 2 {
		t.Fatalf("backend hits = %d", len(gotAuth))
	}
	for i, a := range gotAuth {
		if a != "Bearer gw-secret" {
			t.Fatalf("hit %d Authorization = %q, want overwritten Bearer gw-secret", i, a)
		}
	}
}

func TestProxyStreams(t *testing.T) {
	// FlushInterval=-1 语义断言 + 功能断言：分块 flush 的响应能完整到达客户端
	mux := http.NewServeMux()
	mux.HandleFunc("/traffic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"a":1}`))
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte(`{"b":2}`))
	})
	sock, cleanup := newStubBackend(t, mux)
	defer cleanup()
	g, srv := newLiveGateway(t, sock)

	rp, ok := g.proxy.(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("proxy type = %T", g.proxy)
	}
	if rp.FlushInterval != -1 {
		t.Fatalf("FlushInterval = %v, want -1 (streaming endpoints)", rp.FlushInterval)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/traffic", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:39097"
	req.Header.Set("Cookie", cookieName+"="+testToken)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	body := readAll(t, res.Body)
	if !strings.Contains(body, `{"a":1}`) || !strings.Contains(body, `{"b":2}`) {
		t.Fatalf("streamed body = %q", body)
	}
}

// rawRequest 经真实 TCP 连接发送原始 HTTP/1.1 请求（WebSocket 场景无法用 Recorder 模拟）。
// 读到对端关闭（EOF）为止——101 与普通响应都以此收尾。
func rawRequest(t *testing.T, addr, request string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

func TestProxyWSPassthrough(t *testing.T) {
	// WS Upgrade 请求必须原样到达内核（Upgrade/Connection 头 + Authorization 覆写），
	// 101 响应原样返回浏览器——httplib 反代的原生行为，此测试锁定之
	var mu sync.Mutex
	var gotUpgrade, gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/connections", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUpgrade = r.Header.Get("Upgrade")
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, buf, _ := hj.Hijack()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		buf.Flush()
		conn.Close()
	})
	sock, cleanup := newStubBackend(t, mux)
	defer cleanup()
	_, srv := newLiveGateway(t, sock)

	req := "GET /connections HTTP/1.1\r\n" +
		"Host: 127.0.0.1:39097\r\n" +
		"Cookie: " + cookieName + "=" + testToken + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	resp := rawRequest(t, srv.Listener.Addr().String(), req)
	if !strings.Contains(resp, "101 Switching Protocols") {
		t.Fatalf("ws handshake response = %q", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotUpgrade != "websocket" {
		t.Fatalf("backend Upgrade header = %q", gotUpgrade)
	}
	if gotAuth != "Bearer gw-secret" {
		t.Fatalf("backend Authorization = %q, want overwritten", gotAuth)
	}
}

func TestForgedOriginWSRejected(t *testing.T) {
	// 跨站 WebSocket 场景：伪造 Origin 的 Upgrade 请求在网关即被拒（403），不到内核
	backendHit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/connections", func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
		w.WriteHeader(200)
	})
	sock, cleanup := newStubBackend(t, mux)
	defer cleanup()
	_, srv := newLiveGateway(t, sock)

	req := "GET /connections HTTP/1.1\r\n" +
		"Host: 127.0.0.1:39097\r\n" +
		"Cookie: " + cookieName + "=" + testToken + "\r\n" +
		"Origin: http://evil.example\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" +
		// 403 拒绝响应走 keep-alive；Connection: close 让网关应答后关连接，raw 读到 EOF
		"Connection: close\r\n\r\n"
	resp := rawRequest(t, srv.Listener.Addr().String(), req)
	if !strings.Contains(resp, "403") {
		t.Fatalf("forged-origin ws response = %q, want 403", resp)
	}
	if backendHit {
		t.Fatal("forged-origin request must not reach the backend")
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}
