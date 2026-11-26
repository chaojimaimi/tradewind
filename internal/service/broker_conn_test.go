package service

// broker_conn_test.go：代理入口的连接准入、监听与生命周期的基本用例（批⑥b §4.3）——
// uid 白名单、残留 socket 清理（属主/类型/符号链接三道 fail-closed）、已监听再 start
// 的 no-op、socket 权限与关停摘除、到内核按请求拨号、被拒日志的净化。夹具（桩内核、startBroker、
// brokerDo、rawBrokerRequest）在 broker_test.go；竞态交错的用例在 broker_race_test.go，
// 流式与请求体的用例在 broker_stream_test.go。

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingListener 统计桩内核收到的连接数（"到内核按请求拨号"的断言用）。
type countingListener struct {
	net.Listener
	accepts atomic.Int64
}

// Accept 计数后转交内层 listener。
func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

// kernelConns 返回桩内核至今收到的连接数。
func (s *stubKernel) kernelConns() int64 {
	return s.ln.(*countingListener).accepts.Load()
}

// TestBrokerDialsKernelPerRequest 到内核按请求拨号、不复用连接：连续三个请求，桩内核
// 恰好收到三条连接。WHY 要钉：内核自愈重启后，Transport 池里的残连接会让紧接着的
// PUT/DELETE 得到一次瞬时 502——关掉 keep-alive 后每个请求都落在活着的内核上。
func TestBrokerDialsKernelPerRequest(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	for i := 1; i <= 3; i++ {
		if status, _ := brokerDo(t, brokerSock, "GET", "/version", nil); status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, status)
		}
	}
	if got := sk.kernelConns(); got != 3 {
		t.Fatalf("kernel connections = %d, want 3 (one per request, no keep-alive to the kernel)", got)
	}
}

// TestBrokerRejectsUIDOutsideAllowList 白名单外的 uid 拿不到任何应答（连接被关）；
// 白名单内的 uid 正常。测试造不出别的 uid 的连接——这条钉的是"监听器确实做了白名单"。
func TestBrokerRejectsUIDOutsideAllowList(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	foreign := filepath.Join(dir, "broker-foreign.sock")
	startBroker(t, foreign, sk.sock(), []int{os.Getuid() + 1})

	resp := rawBrokerRequest(t, foreign, "GET /version HTTP/1.1\r\nHost: mihomo-core\r\nConnection: close\r\n\r\n")
	if strings.Contains(resp, "HTTP/1.1") {
		t.Fatalf("foreign uid must get no response at all, got %q", resp)
	}
	own := filepath.Join(dir, "broker-own.sock")
	startBroker(t, own, sk.sock(), []int{os.Getuid()})
	status, _ := brokerDo(t, own, "GET", "/version", nil)
	if status != http.StatusOK {
		t.Fatalf("allowed uid status = %d, want 200", status)
	}
}

// TestBrokerStaleSocketCleanup 清残留比服务 socket 严一步：(a) 本账号的残留 socket
// → 清掉并成功监听；(b) 普通文件 → 报错、内容原封不动、未监听；(c) 目录 → 同样报错
// 不动它；(d) 符号链接 → 拒；(e) 异属主 socket → 拒且不删（euid 注入模拟）。
func TestBrokerStaleSocketCleanup(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)

	// (a) 起一个监听后只关 listener 且不 unlink——留下本账号的 socket 残留
	stale := filepath.Join(dir, "broker-stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(stale); err != nil {
		t.Fatalf("stale socket must be on the path: %v", err)
	}
	b := newAPIBroker()
	if err := b.start(stale, sk.sock(), []int{os.Getuid()}); err != nil {
		t.Fatalf("start over an own-uid stale socket = %v, want success", err)
	}
	status, _ := brokerDo(t, stale, "GET", "/version", nil)
	if status != http.StatusOK {
		t.Fatalf("after stale cleanup status = %d, want 200", status)
	}
	b.close()

	// (b) 普通文件
	plain := filepath.Join(dir, "broker-plain.sock")
	if err := os.WriteFile(plain, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	b = newAPIBroker()
	if err := b.start(plain, sk.sock(), []int{os.Getuid()}); err == nil {
		t.Fatal("start over a plain file must fail (fail-closed)")
	}
	if data, err := os.ReadFile(plain); err != nil || string(data) != "sentinel" {
		t.Fatalf("plain file = %q err %v, want the sentinel untouched", data, err)
	}
	if _, ok := b.listening(); ok {
		t.Fatal("broker must not be listening after the failed start")
	}

	// (c) 目录
	adir := filepath.Join(dir, "broker-dir.sock")
	if err := os.Mkdir(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	b = newAPIBroker()
	if err := b.start(adir, sk.sock(), []int{os.Getuid()}); err == nil {
		t.Fatal("start over a directory must fail (fail-closed)")
	}
	if info, err := os.Stat(adir); err != nil || !info.IsDir() {
		t.Fatalf("directory must stay untouched, got %v err %v", info, err)
	}
	if _, ok := b.listening(); ok {
		t.Fatal("broker must not be listening after the failed start")
	}

	// (d) 符号链接（指向的还是本账号的 socket）：Lstat 看到的是链接不是 socket → 拒，
	// 链接与目标都不动
	real := filepath.Join(dir, "broker-real.sock")
	rln, err := net.Listen("unix", real)
	if err != nil {
		t.Fatal(err)
	}
	rln.(*net.UnixListener).SetUnlinkOnClose(false)
	rln.Close()
	link := filepath.Join(dir, "broker-link.sock")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	b = newAPIBroker()
	if err := b.start(link, sk.sock(), []int{os.Getuid()}); err == nil {
		t.Fatal("start over a symlink must fail (fail-closed)")
	}
	if _, ok := b.listening(); ok {
		t.Fatal("broker must not be listening after the refused start")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink must stay untouched: %v", err)
	}
	if _, err := os.Lstat(real); err != nil {
		t.Fatalf("symlink target must stay untouched: %v", err)
	}

	// (e) 别人的 socket：非特权测试造不出异属主条目，经注入的 euid 模拟——拒，且不删
	foreign := filepath.Join(dir, "broker-foreign.sock")
	fln, err := net.Listen("unix", foreign)
	if err != nil {
		t.Fatal(err)
	}
	fln.(*net.UnixListener).SetUnlinkOnClose(false)
	fln.Close()
	notMe := func() int { return os.Getuid() + 1 }
	if err := removeStaleSockIfOwned(foreign, notMe); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("a socket owned by another uid must be refused with an ownership error, got %v", err)
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatalf("foreign socket must stay untouched: %v", err)
	}
	// 同一个 socket，以真实 euid 判定则确属本账号（证明上面拒的确是属主这一道）
	if err := removeStaleSockIfOwned(foreign, os.Getuid); err != nil {
		t.Fatalf("own-uid socket must be removable: %v", err)
	}
	if _, err := os.Lstat(foreign); !os.IsNotExist(err) {
		t.Fatalf("own-uid socket must be removed (err = %v)", err)
	}
}

// TestBrokerStartIsIdempotent 已监听时再 start 是 no-op：自愈预算耗尽后代理仍挂着，
// 新的 start 重新拉核时走这里——不得报错、不得换路径、在用的白名单不被改写。
func TestBrokerStartIsIdempotent(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	b := startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	if err := b.start(brokerSock, sk.sock(), []int{os.Getuid() + 1}); err != nil {
		t.Fatalf("second start while listening = %v, want a no-op", err)
	}
	if path, ok := b.listening(); !ok || path != brokerSock {
		t.Fatalf("listening = (%q, %v), want (%q, true)", path, ok, brokerSock)
	}
	if status, _ := brokerDo(t, brokerSock, "GET", "/version", nil); status != http.StatusOK {
		t.Fatalf("/version after the no-op start = %d, want 200 (own uid must still be allowed)", status)
	}
}

// TestBrokerSocketModeAndLifecycle 监听后 socket 权限恰为 0666；close 后路径不存在、
// 未监听；close 幂等；再次 start 能正常服务。
func TestBrokerSocketModeAndLifecycle(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	b := startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	info, err := os.Lstat(brokerSock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("broker sock perm = %o, want 0666", info.Mode().Perm())
	}
	if path, ok := b.listening(); !ok || path != brokerSock {
		t.Fatalf("listening = (%q, %v), want (%q, true)", path, ok, brokerSock)
	}

	b.close()
	if _, err := os.Lstat(brokerSock); !os.IsNotExist(err) {
		t.Fatalf("socket must be removed after close (err = %v)", err)
	}
	if _, ok := b.listening(); ok {
		t.Fatal("listening must be false after close")
	}
	b.close() // 幂等
	if _, ok := b.listening(); ok {
		t.Fatal("listening must stay false after a second close")
	}

	// 再次 start：正常服务
	if err := b.start(brokerSock, sk.sock(), []int{os.Getuid()}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	status, _ := brokerDo(t, brokerSock, "GET", "/version", nil)
	if status != http.StatusOK {
		t.Fatalf("after restart status = %d, want 200", status)
	}
	b.close()
}

// TestBrokerDeniedLogIsSanitized 被拒请求的日志净化（I6）：解码路径里带换行的请求，
// 日志仍是一行且 evil/injected 相邻；200 字符的被拒路径在日志里不超过 64 字节。
func TestBrokerDeniedLogIsSanitized(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	logPath := filepath.Join(dir, "service-stderr.log")
	// 必须在起代理之前截获（captureServiceLog 换的是 os.Stderr 变量——只能在没有任何
	// 服务协程存活时触碰，原因见该函数的注释）
	captureServiceLog(t, logPath)
	startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	raw := "GET /..//evil%0Ainjected HTTP/1.1\r\nHost: mihomo-core\r\nConnection: close\r\n\r\n"
	resp := rawBrokerRequest(t, brokerSock, raw)
	if !strings.Contains(resp, "403") {
		t.Fatalf("denied response = %q, want 403", resp)
	}
	// deny 日志写在应答之前（同协程），读完应答即已落盘；轮询只为防抖
	var logData []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logData, _ = os.ReadFile(logPath)
		if strings.Contains(string(logData), "broker denied GET") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(logData), "broker denied GET") {
		t.Fatalf("log must record the denial:\n%s", logData)
	}
	if !strings.Contains(string(logData), "evilinjected") {
		t.Fatalf("evil/injected must be adjacent on one line (newline dropped):\n%s", logData)
	}
	for _, line := range strings.Split(string(logData), "\n") {
		if strings.HasPrefix(line, "injected") {
			t.Fatalf("no log line may start with 'injected' (the newline must not split lines):\n%s", logData)
		}
	}

	// 200 字符的被拒路径：日志里该路径截到 ≤ 64 字节（方法也在净化函数里过闸）
	long := "/" + strings.Repeat("x", 199)
	raw2 := "GET " + long + " HTTP/1.1\r\nHost: mihomo-core\r\nConnection: close\r\n\r\n"
	if resp := rawBrokerRequest(t, brokerSock, raw2); !strings.Contains(resp, "403") {
		t.Fatalf("long denied path response = %q, want 403", resp)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logData, _ = os.ReadFile(logPath)
		if strings.Contains(string(logData), "broker denied GET /") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 行格式 service: broker denied %s %s：前缀 27 字节 + 方法 GET(3) + 空格 + ≤64 字节路径
	wantLine := "service: broker denied GET " + routeLogText(long)
	if !strings.Contains(string(logData), wantLine) {
		t.Fatalf("log must contain the truncated line %q:\n%s", wantLine, logData)
	}
	// 行首带时间戳（c8 ①）：路径长度只对 "service: broker denied GET " 之后、换行
	// 之前的部分计量——时间戳与 "service: " 前缀不在计量内
	for _, line := range strings.Split(string(logData), "\n") {
		rest, ok := strings.CutPrefix(line, "service: broker denied GET ")
		if ok && len(rest) > brokerRouteLogMax {
			t.Fatalf("denied path in the log exceeds %d bytes: %q (%d bytes)", brokerRouteLogMax, line, len(rest))
		}
	}
}
