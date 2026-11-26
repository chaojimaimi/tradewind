package service

// broker_test.go：代理处理器的处理器级用例（批⑥b §4.3）与共用的进程内记录型桩内核
// （/tmp 短路径 unix socket 上的 http.Server）——转发保真、拒绝路由零上游命中、
// PATCH mode 键闸、请求体上限、上游不可达。连接准入与监听生命周期在
// broker_conn_test.go，竞态交错在 broker_race_test.go，流式与请求体在
// broker_stream_test.go，与服务编排的联动在 broker_lifecycle_test.go。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// brokerHit 记录桩内核收到的一个请求（顺序即到达顺序）。
type brokerHit struct {
	Method  string
	Path    string // r.URL.EscapedPath()
	Query   string
	Host    string
	Upgrade string // Upgrade 头（WebSocket 透传断言用）
	Body    []byte
}

// stubKernel 进程内记录型桩内核：非升级请求回 200 `{"stub":N}`（N 是到达序号）；
// 升级请求劫持后回 101 并写一段字节（透传测试）。
type stubKernel struct {
	mu   sync.Mutex
	hits []brokerHit
	ln   net.Listener
	srv  *http.Server
	// lateAfter > 0：升级劫持后写完首段字节，隔这么久再写一段（长连接用例）。
	// 须在任何请求到达之前设置。
	lateAfter time.Duration
	// streamPath 非空：该路径按流式应答——写一段、隔 lateAfter 再写一段（长应答用例）。
	// 同样须在任何请求到达之前设置。
	streamPath string
}

// newStubKernel 在 /tmp 短路径的 unix socket 上起桩内核（macOS sun_path 上限 104 字符）。
func newStubKernel(t *testing.T) *stubKernel {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "upstream.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("stub kernel listen: %v", err)
	}
	sk := &stubKernel{ln: &countingListener{Listener: ln}}
	sk.srv = &http.Server{Handler: http.HandlerFunc(sk.handle)}
	go sk.srv.Serve(sk.ln) //nolint:errcheck // 测试桩（经计数包装器）
	t.Cleanup(func() { sk.srv.Close() })
	return sk
}

// handle 记录请求后按升级/普通两种形态应答。
func (s *stubKernel) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.hits = append(s.hits, brokerHit{
		Method:  r.Method,
		Path:    r.URL.EscapedPath(),
		Query:   r.URL.RawQuery,
		Host:    r.Host,
		Upgrade: r.Header.Get("Upgrade"),
		Body:    body,
	})
	n := len(s.hits)
	s.mu.Unlock()
	if s.streamPath != "" && r.URL.Path == s.streamPath {
		_, _ = w.Write([]byte("stream-bytes"))
		w.(http.Flusher).Flush()
		if s.lateAfter > 0 {
			time.Sleep(s.lateAfter)
			_, _ = w.Write([]byte("late-stream-bytes"))
			w.(http.Flusher).Flush()
		}
		return
	}
	if r.Header.Get("Upgrade") != "" {
		// WebSocket 握手：手写 101 + 一段字节（无 websocket 库，panel 先例同款）
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, _ := hj.Hijack()
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		buf.WriteString("upstream-bytes")
		buf.Flush()
		if s.lateAfter > 0 {
			time.Sleep(s.lateAfter)
			buf.WriteString("late-upstream-bytes")
			buf.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"stub":%d}`, n)
}

// sock 返回桩内核的 socket 路径（UnixListener Addr 即路径）。
func (s *stubKernel) sock() string { return s.ln.Addr().String() }

// recorded 返回已收到请求的副本。
func (s *stubKernel) recorded() []brokerHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]brokerHit(nil), s.hits...)
}

// stop 关闭桩内核的监听（上游不可达用例用）。
func (s *stubKernel) stop() { s.srv.Close() }

// startBroker 起一个指向上游 socket 的代理实例。
func startBroker(t *testing.T, sockPath, kernelSock string, uids []int) *apiBroker {
	t.Helper()
	b := newAPIBroker()
	if err := b.start(sockPath, kernelSock, uids); err != nil {
		t.Fatalf("broker start: %v", err)
	}
	t.Cleanup(b.close)
	return b
}

// brokerDo 经 unix socket 发一个任意方法/路径的请求（core.NewClient 同款 Transport，
// 但方法/路径任意），返回状态码与应答体文本。
func brokerDo(t *testing.T, sock, method, target string, body []byte) (int, string) {
	t.Helper()
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	req, err := http.NewRequest(method, "http://mihomo-core"+target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, target, err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, target, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response of %s %s: %v", method, target, err)
	}
	return res.StatusCode, string(data)
}

// rawBrokerRequest 经 unix socket 写一段原始请求字节、读回全部原始应答字节（读到
// EOF——101、普通应答、对端断连都以此收尾；被拒请求须带 Connection: close）。对端
// 在本端写之前就把连接关了（uid 白名单外的断连就是这种时序）→ 按"零应答"处理而不是
// 用例失败（peerAlreadyClosed）：写没写进去由各用例的应答断言去判。
func rawBrokerRequest(t *testing.T, sock, request string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil && !peerAlreadyClosed(err) {
		t.Fatalf("write: %v", err)
	}
	data, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

// TestBrokerForwardsAllowedRequestsUnchanged 转发保真：桩上游看到的 method /
// EscapedPath / RawQuery / Host / 请求体字节与发出的逐一相同（转发不改写 URL），
// 客户端拿到上游的状态码与应答体。
func TestBrokerForwardsAllowedRequestsUnchanged(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	cases := []struct {
		method string
		target string
		body   []byte
	}{
		{"GET", "/version", nil},
		{"GET", "/configs", nil},
		{"GET", "/proxies", nil},
		{"PUT", "/proxies/HK-01", []byte(`{"name":"HK-01"}`)},
		{"DELETE", "/proxies/a%2Fb", nil},               // 名字里带 / 的节点（段内编码原样）
		{"GET", "/proxies/%E8%8A%82%E7%82%B9%20A", nil}, // 带空格编码的名字
		{"GET", "/proxies/HK-01/delay?timeout=5000&url=http%3A%2F%2Fexample.org", nil},
		{"GET", "/providers/proxies/sub/node-1/healthcheck?timeout=5000", nil},
		{"DELETE", "/connections/abc-123", nil},
		{"PATCH", "/configs", []byte(`{"mode":"global"}`)},
	}
	sent := 0
	for _, tc := range cases {
		status, respBody := brokerDo(t, brokerSock, tc.method, tc.target, tc.body)
		if status != http.StatusOK {
			t.Fatalf("%s %s status = %d, want 200", tc.method, tc.target, status)
		}
		if want := fmt.Sprintf(`{"stub":%d}`, sent+1); respBody != want {
			t.Fatalf("%s %s response = %q, want upstream body %q", tc.method, tc.target, respBody, want)
		}
		sent++
	}
	hits := sk.recorded()
	if len(hits) != len(cases) {
		t.Fatalf("upstream hits = %d, want %d", len(hits), len(cases))
	}
	for i, tc := range cases {
		u, err := url.Parse(tc.target)
		if err != nil {
			t.Fatalf("parse target %s: %v", tc.target, err)
		}
		if hits[i].Method != tc.method {
			t.Errorf("hit %d method = %q, want %q", i, hits[i].Method, tc.method)
		}
		if hits[i].Path != u.EscapedPath() {
			t.Errorf("hit %d path = %q, want %q (unchanged escape)", i, hits[i].Path, u.EscapedPath())
		}
		if hits[i].Query != u.RawQuery {
			t.Errorf("hit %d query = %q, want %q", i, hits[i].Query, u.RawQuery)
		}
		if hits[i].Host != brokerHost {
			t.Errorf("hit %d host = %q, want %q", i, hits[i].Host, brokerHost)
		}
		if !bytes.Equal(hits[i].Body, tc.body) {
			t.Errorf("hit %d body = %q, want %q (forwarded byte-for-byte)", i, hits[i].Body, tc.body)
		}
	}
}

// TestBrokerDeniesBlockedRoutes 逐条发被拒路由：403、应答体恰为 brokerBlockedBody、
// Content-Type: application/json、应答中不出现请求路径、桩上游零命中。
func TestBrokerDeniesBlockedRoutes(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	cases := []struct{ method, target string }{
		// 11 条明确拒绝的内核固定路由
		{"GET", "/"},
		{"PUT", "/configs"},
		{"POST", "/configs/geo"},
		{"PATCH", "/rules/disable"},
		{"GET", "/storage/k"},
		{"PUT", "/storage/k"},
		{"DELETE", "/storage/k"},
		{"POST", "/restart"},
		{"POST", "/upgrade"},
		{"POST", "/upgrade/ui"},
		{"POST", "/upgrade/geo"},
		// 条件挂载的形状
		{"GET", "/ui"},
		{"GET", "/ui/index.html"},
		{"GET", "/debug/pprof/"},
		// 方法 × 形状不在表内
		{"POST", "/version"},
		{"DELETE", "/proxies"},
		{"PUT", "/connections"},
		{"PATCH", "/proxies/x"},
	}
	for _, tc := range cases {
		status, respBody := brokerDo(t, brokerSock, tc.method, tc.target, nil)
		if status != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, want 403", tc.method, tc.target, status)
		}
		if respBody != brokerBlockedBody {
			t.Fatalf("%s %s body = %q, want exactly %q", tc.method, tc.target, respBody, brokerBlockedBody)
		}
		if strings.Contains(respBody, tc.target) {
			t.Fatalf("%s %s: the request path must not appear in the response", tc.method, tc.target)
		}
	}
	// 代理自产应答头（writeBrokerJSON）经原始应答字节钉死
	raw := "GET /upgrade HTTP/1.1\r\nHost: mihomo-core\r\nConnection: close\r\n\r\n"
	resp := rawBrokerRequest(t, brokerSock, raw)
	if !strings.Contains(resp, "403") || !strings.Contains(resp, "Content-Type: application/json") {
		t.Fatalf("denied raw response = %q, want 403 with application/json", resp)
	}
	if hits := sk.recorded(); len(hits) != 0 {
		t.Fatalf("upstream hits = %d, want 0 (denied routes must not reach the core)", len(hits))
	}
}

// TestBrokerPatchConfigsModeFilter PATCH /configs：合法 mode 逐字节到达上游；非法键
// / 超限请求体 → 403、上游零命中。
func TestBrokerPatchConfigsModeFilter(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	status, _ := brokerDo(t, brokerSock, "PATCH", "/configs", []byte(`{"mode":"rule"}`))
	if status != http.StatusOK {
		t.Fatalf("PATCH /configs {mode:rule} status = %d, want 200", status)
	}
	hits := sk.recorded()
	if len(hits) != 1 || !bytes.Equal(hits[0].Body, []byte(`{"mode":"rule"}`)) {
		t.Fatalf("upstream hit = %+v, want the exact body {\"mode\":\"rule\"}", hits)
	}

	status, respBody := brokerDo(t, brokerSock, "PATCH", "/configs", []byte(`{"port":7890}`))
	if status != http.StatusForbidden || respBody != brokerBlockedBody {
		t.Fatalf("PATCH with port = %d %q, want 403 %q", status, respBody, brokerBlockedBody)
	}
	big := append([]byte(`{"mode":"`), bytes.Repeat([]byte("a"), brokerMaxPatchBytes)...)
	big = append(big, []byte(`"}`)...)
	status, _ = brokerDo(t, brokerSock, "PATCH", "/configs", big)
	if status != http.StatusForbidden {
		t.Fatalf("over-limit PATCH status = %d, want 403", status)
	}
	if hits := sk.recorded(); len(hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1 (denied PATCHes must not reach the core)", len(hits))
	}
}

// TestBrokerBodyLimit 非 WebSocket 请求体上限：64 KiB 恰好全量转发；64 KiB + 1 与
// 分块传输超限 → 413、上游零命中。
func TestBrokerBodyLimit(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	body := bytes.Repeat([]byte("a"), brokerMaxBodyBytes)
	status, _ := brokerDo(t, brokerSock, "PUT", "/proxies/x", body)
	if status != http.StatusOK {
		t.Fatalf("PUT %d bytes status = %d, want 200", len(body), status)
	}
	hits := sk.recorded()
	if len(hits) != 1 || !bytes.Equal(hits[0].Body, body) {
		t.Fatalf("upstream must receive all %d bytes, got %d hits", len(body), len(hits))
	}

	status, respBody := brokerDo(t, brokerSock, "PUT", "/proxies/x", append(body, 'a'))
	if status != http.StatusRequestEntityTooLarge || respBody != brokerTooLargeBody {
		t.Fatalf("%d bytes = %d %q, want 413 %q", len(body)+1, status, respBody, brokerTooLargeBody)
	}

	// 分块传输（ContentLength 未知）超限 → 同样 413
	var raw strings.Builder
	raw.WriteString("PUT /proxies/x HTTP/1.1\r\nHost: mihomo-core\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n")
	for i := 0; i < 70; i++ { // 70 × 1 KiB = 70 KiB > 64 KiB
		fmt.Fprintf(&raw, "400\r\n%s\r\n", strings.Repeat("b", 1024))
	}
	raw.WriteString("0\r\n\r\n")
	resp := rawBrokerRequest(t, brokerSock, raw.String())
	if !strings.Contains(resp, "413") {
		t.Fatalf("chunked over-limit response = %q, want 413", resp)
	}
	if hits := sk.recorded(); len(hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1 (rejected bodies must not reach the core)", len(hits))
	}
}

// TestBrokerUpstreamUnreachableIs502 上游不可达：502、应答体为 brokerUpstreamDown、
// 应答文本不含任何 socket 路径。
func TestBrokerUpstreamUnreachableIs502(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})
	sk.stop()

	status, respBody := brokerDo(t, brokerSock, "GET", "/version", nil)
	if status != http.StatusBadGateway || respBody != brokerUpstreamDown {
		t.Fatalf("upstream down = %d %q, want 502 %q", status, respBody, brokerUpstreamDown)
	}
	for _, secret := range []string{dir, sk.sock(), brokerSock} {
		if strings.Contains(respBody, secret) {
			t.Fatalf("response %q must not contain the socket path %q", respBody, secret)
		}
	}
}
