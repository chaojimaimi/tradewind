// broker.go：代理入口的监听/认证/转发（批⑥b §4.3，I4）——用户域访问内核 API 的
// 唯一入口。socket 属主为服务进程、权限 0666，所在锚目录 root 独占可写；每个连接
// 先过 peer-uid 白名单（与服务 socket 同款），每个请求过白名单路由（broker_allow.go），
// 只放行内核路由表里逐条列出的 29 条路由，转发不改写 URL（httputil.ReverseProxy
// 按请求拨号——不持有到内核的长连接，内核崩溃自愈期间不必重建）。
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// 代理入口的加固参数。
const (
	brokerReadHeaderTimeout = 15 * time.Second
	brokerIdleTimeout       = 120 * time.Second
	brokerMaxHeaderBytes    = 64 << 10
	brokerDialTimeout       = 5 * time.Second
	brokerRouteLogMax       = 64 // 被拒请求的方法/路径进日志时的截断长度
)

// brokerBodyReadTimeout 是有界读请求体的读截止（变量缝：测试缩到百毫秒级）。
var brokerBodyReadTimeout = 15 * time.Second

const (
	brokerHost = "mihomo-core"
	// 代理自产的应答体：全部是服务侧常量，不回显请求、不含 socket 路径。
	brokerBlockedBody    = `{"message":"blocked by tradewind privileged service"}`
	brokerUpstreamDown   = `{"message":"core unavailable"}`
	brokerTooLargeBody   = `{"message":"request body too large"}`
	brokerBadRequestBody = `{"message":"bad request"}`
)

// apiBroker 是按请求拨号的反向代理：不持有到内核的长连接，内核崩溃自愈期间不必重建。
type apiBroker struct {
	mu   sync.Mutex
	srv  *http.Server
	path string // 正在监听的 socket 路径（空 = 未监听）

	// hook 测试缝（生产恒 nil）：在四处各调一次，把 Serve 协程、close()、start() 之间
	// 的竞态交错钉成确定性时序（broker_race_test.go）。
	hook func(stage hookStage)
}

// hookStage 标记代理走到了哪一处。
type hookStage int

const (
	stageBeforeServe       hookStage = iota // serve 协程进 Serve 之前（listener 尚未登记进 http.Server）
	stageServeReturned                      // Serve 已返回、收敛登记之前
	stageServeFinished                      // serve 协程收尾（含收敛登记）之后
	stageCloseBeforeUnlink                  // close() 已关服务、尚未摘路径（仍持锁）
)

// newAPIBroker 构造未监听的代理。
func newAPIBroker() *apiBroker { return &apiBroker{} }

// start 让代理在 sockPath 上监听：已监听 → no-op；否则清残留 → Listen → chmod 0666
// → 起 http.Server。allowedUIDs 拷贝成白名单集合（后续改动锚不影响在用连接的判定）。
func (b *apiBroker) start(sockPath, kernelSock string, allowedUIDs []int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.srv != nil {
		return nil
	}
	if err := removeStaleBrokerSock(sockPath); err != nil {
		return err
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("service: broker listen %s: %w", sockPath, err)
	}
	// WHY 关掉 net 的"关闭即 unlink"：socket 路径的生杀由本类型自己管（close 在锁内
	// 摘）。不关有一个真实的竞态——close() 抢在 Serve 协程登记 listener 之前时，
	// http.Server.Close 立即返回、路径被摘、下一次 start 在同一路径上重新监听，随后
	// 才被调度的旧 Serve 一进门就因"已在关停"返回，它的 defer 关 listener 时按路径
	// unlink——摘掉的是新实例的 socket（net 源码自注 "there's a race here"）。
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	// WHY 监听后可以按路径 chmod：锚目录只有 root 可写，条目在 Listen 与 chmod 之间
	// 换不成符号链接；调用 start 的 spawnCore 此前已跑过 verifyHomeTrust（其祖先链
	// 覆盖锚目录）。chmod 失败即关 listener、摘掉刚建的 socket——绝不留下敞开的入口。
	if err := os.Chmod(sockPath, 0o666); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return fmt.Errorf("service: broker chmod: %w", err)
	}
	srv := &http.Server{
		Handler:           newBrokerHandler(kernelSock),
		ReadHeaderTimeout: brokerReadHeaderTimeout,
		IdleTimeout:       brokerIdleTimeout,
		MaxHeaderBytes:    brokerMaxHeaderBytes,
		// WHY 不设 ReadTimeout/WriteTimeout：它们会让被劫持的 WebSocket 连接
		//（/logs /traffic /connections）在超时后断流。慢请求由有界读兜底。
		// ErrorLog 不带时间戳：与 service.log 里其余行同一种格式。
		ErrorLog: log.New(os.Stderr, "", 0),
	}
	b.srv = srv
	b.path = sockPath
	// 白名单在 start 里就拷成集合：调用方（锚配置）后续的切片改动影响不到在用连接
	go b.serve(srv, &authListener{Listener: ln, allowed: uidSet(allowedUIDs)}, sockPath)
	return nil
}

// serve 在协程里跑 http.Server，并在它退出时收敛登记：close() 已处理过（它先摘了
// 登记）就什么都不做；否则是 Serve 意外退出——收回登记并摘除 socket，下次 start
// 可重新监听。WHY 认登记而不是直接摘：一次紧接的 close/start 里，旧协程醒来时新
// 实例可能已经在同一路径上监听，无脑摘除会删掉新实例的 socket。
func (b *apiBroker) serve(srv *http.Server, ln net.Listener, sockPath string) {
	if b.hook != nil {
		b.hook(stageBeforeServe)
	}
	err := srv.Serve(ln)
	if b.hook != nil {
		b.hook(stageServeReturned)
	}
	b.settle(srv, err, sockPath)
	if b.hook != nil {
		b.hook(stageServeFinished)
	}
}

// settle 是 Serve 退出后的收敛登记：close() 已处理过（它先摘了登记）就什么都不做；
// 否则是 Serve 意外退出——收回登记并摘除 socket，下次 start 可重新监听。WHY 认登记
// 而不是直接摘：一次紧接的 close/start 里，旧协程醒来时新实例可能已经在同一路径上
// 监听，无脑摘除会删掉新实例的 socket。
func (b *apiBroker) settle(srv *http.Server, err error, sockPath string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.srv != srv {
		return
	}
	b.srv, b.path = nil, ""
	_ = os.Remove(sockPath)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "service: broker serve: %v\n", err)
	}
}

// close 关闭代理并摘除 socket：未监听 → no-op。全程持锁：摘登记、关服务、摘路径对
// 并发的 start 是一个整体（中途插进来的 start 不得在旧路径还没摘掉时重新监听）。
// WHY 先摘登记再 Close：Serve 协程醒来后看到登记已不属于自己，就不会去删路径——
// 否则一次紧接的 stop/start 里，旧协程可能删掉新实例的 socket。WHY 用 Close 不用
// Shutdown：流式端点（/logs /traffic /connections 的 WebSocket）永远不会进入 idle，
// Shutdown 会一直等；被劫持的连接不在 http.Server 的登记里，Close 关不到它们——
// 它们随上游（内核）退出而结束。
func (b *apiBroker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	srv, path := b.srv, b.path
	if srv == nil {
		return
	}
	b.srv, b.path = nil, ""
	_ = srv.Close()
	if b.hook != nil {
		b.hook(stageCloseBeforeUnlink)
	}
	if path != "" {
		_ = os.Remove(path)
	}
}

// listening 返回正在监听的 socket 路径（未监听时 ok=false）。
func (b *apiBroker) listening() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.path, b.srv != nil
}

// removeStaleBrokerSock 清残留——比服务 socket 的裸 Remove 严一步（fail-closed）：
// Lstat 到的条目必须是属主等于本进程 euid 的 socket（上一实例的残留）才 Remove；
// 不存在 → 直接放行；是别的东西（普通文件、目录、符号链接、别人的 socket）→
// 返回错误、不动它。WHY：这条路径上 root 会做 unlink，目录若被换到攻击者选定的
// 位置，删除就落在别人头上——能删的只有自己名下的 socket。
func removeStaleBrokerSock(path string) error {
	return removeStaleSockIfOwned(path, os.Geteuid)
}

// removeStaleSockIfOwned 是 removeStaleBrokerSock 的本体；属主比对的基准 euid 由调用
// 方给——非特权测试造不出异属主的条目，只能从这里注入一个"不是本账号"的 euid。
func removeStaleSockIfOwned(path string, euid func() int) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("service: broker stale sock lstat: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("service: broker sock path is not a socket (mode %s); refusing to remove", info.Mode())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(euid()) {
		return fmt.Errorf("service: broker sock path is not owned by this process's euid; refusing to remove")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("service: broker stale sock remove: %w", err)
	}
	return nil
}

// authListener 包装 Accept：每个连接先 peerUID（peercred.go 既有实现）——读不到 →
// 记日志、关连接、继续 accept；uid 不在名单 → 关连接并记一行（只写 uid，格式对齐
// 服务 socket）。白名单外的连接对 http.Server 完全不可见。
type authListener struct {
	net.Listener
	allowed map[int]bool
}

// Accept 阻塞到拿到一个通过白名单的连接（或 listener 出错）。
func (l *authListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return conn, err
		}
		uid, uerr := peerUID(conn)
		if uerr != nil {
			fmt.Fprintf(os.Stderr, "service: %v\n", uerr)
			_ = conn.Close()
			continue
		}
		if !l.allowed[uid] {
			// 断连 + 计数日志：不回包不提示，直接断（服务 socket 同语义）
			_ = conn.Close()
			fmt.Fprintf(os.Stderr, "service: broker rejected uid %d (not in allowed_uids)\n", uid)
			continue
		}
		return conn, nil
	}
}

// uidSet 把白名单拷成集合（start 时一次性拷贝）。
func uidSet(uids []int) map[int]bool {
	set := make(map[int]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// brokerHandler 在转发前做两件事：白名单路由判定（decideRoute）、请求体的有界预读。
// 上游不可达与流式透传都交给 ReverseProxy。
type brokerHandler struct {
	rp *httputil.ReverseProxy
}

// newBrokerHandler 构造指向 kernelSock 的白名单代理。
func newBrokerHandler(kernelSock string) http.Handler {
	return &brokerHandler{rp: newBrokerReverseProxy(kernelSock)}
}

// ServeHTTP 处理器流程：判定 →（拒绝即 403）/（mode 闸）/（有界预读）→ 转发。
func (h *brokerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch decideRoute(r.Method, r.URL.RawPath, r.URL.Path) {
	case routeDeny:
		// I6：未受信输入不原样进日志——方法/路径都过 routeLogText。WHY 记解码后的
		// Path 而不是判定用的 RawPath：换行这类控制字符只有在解码形态里才现形、才会
		// 被丢弃（RawPath 里它们是 %0A，字母数字落在白名单内反而被保留）。
		fmt.Fprintf(os.Stderr, "service: broker denied %s %s\n",
			routeLogText(r.Method), routeLogText(r.URL.Path))
		writeBrokerJSON(w, http.StatusForbidden, brokerBlockedBody)
		return
	case routeModePatch:
		// PATCH /configs 超限按策略拦截应答（方案 §4.3：否则 403，不转发）
		body, ok := readBrokerBody(w, r, brokerMaxPatchBytes, http.StatusForbidden, brokerBlockedBody)
		if !ok {
			return
		}
		if !modePatchAllowed(body) {
			fmt.Fprintf(os.Stderr, "service: broker denied %s %s\n",
				routeLogText(r.Method), routeLogText(r.URL.Path))
			writeBrokerJSON(w, http.StatusForbidden, brokerBlockedBody)
			return
		}
		// 转发的是读出的原始字节，不是重新编码的（内核按字节解，重编码可能变义）
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	default:
		// 一律有界预读，WebSocket 握手也不例外：握手没有请求体，预读立即返回。
		// WHY 不给升级请求开豁免：豁免只凭客户端自报的两个头，任何放行路由上的请求
		// 加上它们就能让请求体绕过体积上限（评审 P1，实测 100 KiB 全部到达上游）。
		body, ok := readBrokerBody(w, r, brokerMaxBodyBytes, http.StatusRequestEntityTooLarge, brokerTooLargeBody)
		if !ok {
			return
		}
		if len(body) > 0 {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
	}
	h.rp.ServeHTTP(w, r)
}

// newBrokerReverseProxy 构造按请求拨号的反向代理。三要点：
//   - Rewrite（不用已弃用的 Director）只重定位 scheme/host：转发不改写 URL——
//     ReverseProxy 发出的请求行由 EscapedPath() 还原，与代理判定用的字符串恒等；
//     Rewrite 模式下不自动加 X-Forwarded-*——保持不加；
//   - FlushInterval=-1：/traffic /connections /logs 是无限流端点，默认缓冲会攒满
//     4KB 才下发，流式图表卡死（internal/panel 先例同参数）；
//   - WebSocket Upgrade 由 httputil 原生透传，不动 Upgrade/Connection 头。
func newBrokerReverseProxy(kernelSock string) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// 自定义拨号忽略 URL host，一律连内核原生 socket（仅 root 可达）
			d := net.Dialer{Timeout: brokerDialTimeout}
			return d.DialContext(ctx, "unix", kernelSock)
		},
		// WHY 关掉 keep-alive：本文件承诺"按请求拨号"，而默认的 Transport 会把到内核的
		// 空闲连接留在池里。内核自愈重启后池里是残连接——GET 会被透明重试，PUT/DELETE
		// 撞上却会得到一次瞬时 502。本机 unix socket 每请求拨一次的代价可以忽略，换来
		// 的是"自愈之后的请求一定落在活着的内核上"。
		DisableKeepAlives: true,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: brokerHost})
			pr.Out.Host = brokerHost // 仅为 URL 合法性；DialContext 忽略之
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// 客户端已断开（r.Context() 已取消）→ 只应答不记日志，防刷日志；
			// 其余（上游拨号失败等）记一行净化后的错误。不回显 socket 路径。
			if r.Context().Err() == nil {
				fmt.Fprintf(os.Stderr, "service: broker upstream: %s\n", sanitizeForLog(err.Error()))
			}
			writeBrokerJSON(w, http.StatusBadGateway, brokerUpstreamDown)
		},
	}
}

// readBrokerBody 有界读入请求体：超限以 tooLargeStatus + tooLargeBody 应答；读取设
// 截止时间——同账号进程可以"声明 64 KiB、逐字节慢慢发"占住一个协程，读取必须有界。
// WHY 预读而不是边转发边限流：MaxBytesReader 让转发的拷贝中途失败时，客户端拿到的是
// 连接被断而不是一个状态码。读出错应答 400（不回显错误细节）。读完必须收回读截止
// （原因见函数末尾）。
func readBrokerBody(w http.ResponseWriter, r *http.Request, limit int64, tooLargeStatus int, tooLargeBody string) ([]byte, bool) {
	if r.ContentLength > limit {
		writeBrokerJSON(w, tooLargeStatus, tooLargeBody)
		return nil, false
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(brokerBodyReadTimeout)); err != nil {
		writeBrokerJSON(w, http.StatusBadRequest, brokerBadRequestBody)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeBrokerJSON(w, http.StatusBadRequest, brokerBadRequestBody)
		return nil, false
	}
	if int64(len(body)) > limit {
		writeBrokerJSON(w, tooLargeStatus, tooLargeBody)
		return nil, false
	}
	// 读完收回读截止——这一行承重，不是可有可无的防御。http.Server 为了在处理器运行期间
	// 探测"客户端已断开"，会在请求体读到 EOF 的那一刻（没有请求体的请求则更早）在同一
	// 条连接上起一个后台读（net/http server.go 的 startBackgroundRead）。上面设的读截止
	// 因此也压在那个后台读上：不收回，截止一到后台读超时，服务端就取消本请求的 ctx，
	// 还在途的流式应答（/logs）与长耗时请求在这一刻被掐断。WebSocket 握手反而不受影响
	// （劫持会先中止后台读、再清截止）——所以钉住这一行的是非升级的流式应答用例
	//（TestBrokerStreamingResponseOutlivesBodyReadDeadline）。
	_ = rc.SetReadDeadline(time.Time{})
	return body, true
}

// writeBrokerJSON 应答一段固定 JSON（Content-Type + Content-Length 齐备；body 是
// 服务侧常量，不含任何请求回显——socket 路径等内部布局信息不出代理）。
func writeBrokerJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
