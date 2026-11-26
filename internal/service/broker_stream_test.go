package service

// broker_stream_test.go：代理入口的流式与请求体用例（批⑥b §4.3）——WebSocket 透传与
// 长连接、请求体的上限/截断/慢速、自报升级头不得绕过体积上限。夹具（桩内核、
// startBroker、brokerDo）在 broker_test.go。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBrokerWSPassthrough 桩上游劫持 /connections 回 101 并写一段字节：经代理发原始
// 升级请求 → 收到 101 且那段字节到达；上游看到 Upgrade: websocket。
func TestBrokerWSPassthrough(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t) // 默认劫持一切升级请求
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	req := "GET /connections HTTP/1.1\r\n" +
		"Host: mihomo-core\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	resp := rawBrokerRequest(t, brokerSock, req)
	if !strings.Contains(resp, "101 Switching Protocols") {
		t.Fatalf("ws handshake response = %q", resp)
	}
	if !strings.Contains(resp, "upstream-bytes") {
		t.Fatalf("upstream bytes must reach the client through the proxy, got %q", resp)
	}
	hits := sk.recorded()
	if len(hits) != 1 || hits[0].Upgrade != "websocket" || hits[0].Host != brokerHost {
		t.Fatalf("upstream hit = %+v, want the upgrade request with Host %q", hits, brokerHost)
	}
}

// peerAlreadyClosed 报告一次写失败是否只是"对端在本端写之前就把连接关了"（uid 白名单
// 外的断连、超限 413 之后的排空都是这种时序）：先关再写，本端的写得到 EPIPE /
// ECONNRESET；对端关得足够早时，macOS 上是 ENOTCONN。三种都按"零应答"处理。
func peerAlreadyClosed(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENOTCONN)
}

// rawBrokerExchange 写一段原始请求头与请求体、读回全部原始应答（读到 EOF）。halfClose
// 为真时写完即半关闭写侧：HTTP/1.1 默认 keep-alive，服务端应答后会把没读完的请求体
// 排空再等下一个请求，不半关闭就读不到 EOF（升级请求不能半关闭——长连接要靠它收发）。
// 对端在本端写完之前就关了连接 → 写得到 EPIPE / ECONNRESET，照常读回应答。
func rawBrokerExchange(t *testing.T, sock, head string, body []byte, halfClose bool) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if _, err := conn.Write(body); err != nil && !peerAlreadyClosed(err) {
		t.Fatalf("write body: %v", err)
	}
	if halfClose {
		_ = conn.(*net.UnixConn).CloseWrite()
	}
	data, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

// TestBrokerUpgradeHeaderDoesNotBypassBodyLimit 自报升级头的普通请求，请求体照常有界
// 预读：超限 413、上游零命中。评审 P1 实锤：升级豁免只凭客户端自报的两个头，任何
// 放行路由加上它们，请求体就能不经上限直达 root 内核。PUT 与 GET 各一例。
func TestBrokerUpgradeHeaderDoesNotBypassBodyLimit(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	body := bytes.Repeat([]byte("u"), 100<<10) // 100 KiB > 64 KiB 上限
	const handshake = "Upgrade: websocket\r\nConnection: Upgrade\r\n"
	for _, target := range []string{"PUT /proxies/x", "GET /version"} {
		head := fmt.Sprintf("%s HTTP/1.1\r\nHost: mihomo-core\r\n%sContent-Length: %d\r\n\r\n",
			target, handshake, len(body))
		resp := rawBrokerExchange(t, brokerSock, head, body, true)
		if !strings.Contains(resp, "413") || !strings.Contains(resp, brokerTooLargeBody) {
			t.Fatalf("%s with self-declared upgrade headers and a %d-byte body: response = %q, want 413 %q",
				target, len(body), resp, brokerTooLargeBody)
		}
	}
	if hits := sk.recorded(); len(hits) != 0 {
		t.Fatalf("upstream hits = %d, want 0 (a body over the cap never reaches the core)", len(hits))
	}
}

// TestBrokerWSSurvivesBodyReadDeadline 属性用例：升级后的长连接活得比请求体的读截止
// 久——有界预读在连接上设过读截止，面板挂着的 /traffic /logs /connections 不能因此
// 断流。说明：单独拿掉 readBrokerBody 末尾"收回读截止"那一步，本用例并不会失败——
// 劫持会先中止服务端的后台读、再清截止；钉住那一步的是非升级的流式应答用例
// （TestBrokerStreamingResponseOutlivesBodyReadDeadline）。这里钉的是 WS 长连接不断流
// 这条性质本身。
func TestBrokerWSSurvivesBodyReadDeadline(t *testing.T) {
	old := brokerBodyReadTimeout
	brokerBodyReadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { brokerBodyReadTimeout = old })
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	sk.lateAfter = 500 * time.Millisecond // 远在读截止之后才下发的字节
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	req := "GET /connections HTTP/1.1\r\n" +
		"Host: mihomo-core\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	resp := rawBrokerExchange(t, brokerSock, req, nil, false)
	for _, want := range []string{"101 Switching Protocols", "upstream-bytes", "late-upstream-bytes"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("ws response = %q, want it to carry %q (the stream must outlive the body read deadline)", resp, want)
		}
	}
}

// TestBrokerStreamingResponseOutlivesBodyReadDeadline 非升级的流式应答活得比请求体的
// 读截止久（/logs 是无限流端点）：截止 150ms，上游隔 500ms 才下发的字节必须到达客户端。
// 这一条钉住的正是 readBrokerBody 读完收回读截止那一步：服务端为探测客户端断开，在
// 请求体读到 EOF 时就在同一条连接上起了后台读——读截止不收回，截止一到后台读超时，
// 服务端取消本请求的 ctx，在途的流式应答在这一刻被掐断（变异验证：拿掉收回即失败）。
func TestBrokerStreamingResponseOutlivesBodyReadDeadline(t *testing.T) {
	old := brokerBodyReadTimeout
	brokerBodyReadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { brokerBodyReadTimeout = old })
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	sk.streamPath = "/logs"
	sk.lateAfter = 500 * time.Millisecond
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	status, respBody := brokerDo(t, brokerSock, "GET", "/logs?level=info", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /logs status = %d, want 200", status)
	}
	for _, want := range []string{"stream-bytes", "late-stream-bytes"} {
		if !strings.Contains(respBody, want) {
			t.Fatalf("streamed response = %q, want it to carry %q (the response must outlive the body read deadline)",
				respBody, want)
		}
	}
}

// TestBrokerTruncatedBodyIs400 声明的 Content-Length 大于实际发出的字节、客户端随即
// 半关闭 → 读请求体出错 → 400 + 固定应答体（不是连接被断），上游零命中。
func TestBrokerTruncatedBodyIs400(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	conn, err := net.DialTimeout("unix", brokerSock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	truncated := "PUT /proxies/x HTTP/1.1\r\nHost: mihomo-core\r\nContent-Length: 10\r\nConnection: close\r\n\r\nabc"
	if _, err := conn.Write([]byte(truncated)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("close write side: %v", err)
	}
	data, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v", err)
	}
	resp := string(data)
	if !strings.Contains(resp, "400") || !strings.Contains(resp, brokerBadRequestBody) {
		t.Fatalf("truncated body response = %q, want 400 %q", resp, brokerBadRequestBody)
	}
	if hits := sk.recorded(); len(hits) != 0 {
		t.Fatalf("upstream hits = %d, want 0 (a body that could not be read is never forwarded)", len(hits))
	}
}

// TestBrokerSlowBodyHitsReadDeadline 声明了 Content-Length 却不把请求体发完、连接
// 也不关（慢速占位）：读截止到期 → 400 + 固定应答体，上游零命中——读取必须有界。
func TestBrokerSlowBodyHitsReadDeadline(t *testing.T) {
	old := brokerBodyReadTimeout
	brokerBodyReadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { brokerBodyReadTimeout = old })
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	conn, err := net.DialTimeout("unix", brokerSock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	slow := "PUT /proxies/x HTTP/1.1\r\nHost: mihomo-core\r\nContent-Length: 10\r\n\r\nabc"
	if _, err := conn.Write([]byte(slow)); err != nil {
		t.Fatalf("write: %v", err)
	}
	began := time.Now()
	data, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read: %v (want the broker to answer and close once the read deadline fires)", err)
	}
	resp := string(data)
	if !strings.Contains(resp, "400") || !strings.Contains(resp, brokerBadRequestBody) {
		t.Fatalf("slow body response = %q, want 400 %q", resp, brokerBadRequestBody)
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("answer took %v, want the shortened read deadline to fire", elapsed)
	}
	if hits := sk.recorded(); len(hits) != 0 {
		t.Fatalf("upstream hits = %d, want 0 (an unfinished body is never forwarded)", len(hits))
	}
}
