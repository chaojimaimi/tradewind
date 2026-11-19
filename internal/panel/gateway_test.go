package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// newTestGateway 构造带临时 ui/ 目录的网关与测试主机名。
func newTestGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	ui := t.TempDir()
	// v0.4.9：fixture 沿用真实 metacubexd index.html 的 config.js 引用形态
	// （<script src="config.js"，无 ./ 前缀、双引号），供断言改写为 config.js?v=<startedAt>
	if err := os.WriteFile(filepath.Join(ui, "index.html"),
		[]byte(`<!doctype html><html><head><script src="config.js"></script></head><body><div id=app>INDEX</div></body></html>`),
		0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ui, "_nuxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ui, "_nuxt", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	// v0.4.7：磁盘 config.js（metacubexd 构建 TTL 空配置形态）保留写入——
	// 证明 /ui/config.js 走网关动态生成而非透传磁盘文件
	if err := os.WriteFile(filepath.Join(ui, "config.js"), []byte("window.config={defaultBackendURL:\"\"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// v0.4.14：磁盘 sw.js（真实 metacubexd 的 Workbox SW 形态）保留写入——
	// 证明 /ui/sw.js 走网关 kill-switch 而非透传磁盘 Workbox 脚本
	if err := os.WriteFile(filepath.Join(ui, "sw.js"),
		[]byte("importScripts(\"workbox-38b98e80.js\");workbox.precaching.precacheAndRoute([]);"),
		0o644); err != nil {
		t.Fatal(err)
	}
	g := NewGateway(Config{Port: 39097, UIDir: ui, Token: testToken, SockPath: "/tmp/none.sock", Secret: "gw-secret"})
	return g, "127.0.0.1:39097"
}

// get 发起带正确 Host 的测试请求（httptest.NewRequest 默认 Host=example.com，必须覆写）。
func get(g *Gateway, host, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func TestTokenEntrySetsCookieAndRedirects(t *testing.T) {
	g, host := newTestGateway(t)
	rec := get(g, host, "/ui/"+testToken+"/", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	// v0.4.12：落地页 = /ui/?v=<startedAt>#/overview——查询串换新文档缓存键（防 v0.4.9
	// 前旧 index.html 的启发式缓存残留），片段锁定概览页
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/?v=") || !strings.HasSuffix(loc, "#/overview") {
		t.Fatalf("entry Location = %q, want /ui/?v=<startedAt>#/overview", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != cookieName || c.Value != testToken {
		t.Fatalf("cookie = %+v", c)
	}
	// Path=/ 是硬性断言：RFC 6265 下 Path=/ui 的 cookie 到不了根路径 API（M1-plan §5.2）
	if c.Path != "/" {
		t.Fatalf("cookie Path = %q, want /", c.Path)
	}
	if !c.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatal("cookie must be SameSite=Lax")
	}
	// 原始 Set-Cookie 头：无 Domain 属性（host-only）
	raw := rec.Header().Get("Set-Cookie")
	if strings.Contains(raw, "Domain=") {
		t.Fatalf("Set-Cookie %q must be host-only (no Domain)", raw)
	}
	// v0.4.14：生产入口恒带 ?nonce=（点击路径 nonce 击穿 page cache）——路由对
	// RawQuery 必须不敏感（isTokenEntry 只消费 Path），带查询串的入口形态不变
	recQ := get(g, host, "/ui/"+testToken+"/?nonce=1750000000000000000", nil)
	if recQ.Code != http.StatusFound {
		t.Fatalf("entry-with-nonce status = %d, want 302", recQ.Code)
	}
	if loc := recQ.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/?v=") || !strings.HasSuffix(loc, "#/overview") {
		t.Fatalf("entry-with-nonce Location = %q", loc)
	}
	if c := recQ.Result().Cookies(); len(c) != 1 || c[0].Value != testToken {
		t.Fatalf("entry-with-nonce cookies = %+v", c)
	}
}

func TestTokenEntryWithoutTrailingSlash(t *testing.T) {
	g, host := newTestGateway(t)
	rec := get(g, host, "/ui/"+testToken, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	// v0.4.12：无尾斜杠入口同样落地带缓存键的概览页
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/?v=") || !strings.HasSuffix(loc, "#/overview") {
		t.Fatalf("entry Location = %q, want /ui/?v=<startedAt>#/overview", loc)
	}
	if rec.Result().Cookies()[0].Value != testToken {
		t.Fatal("cookie not set for slash-less entry")
	}
}

func TestWrongTokenEntryIs404(t *testing.T) {
	g, host := newTestGateway(t)
	wrong := strings.Repeat("f", 64) // 形状合法但值错误——必须走入口校验失败分支
	if rec := get(g, host, "/ui/"+wrong+"/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong token status = %d, want 404", rec.Code)
	}
	// 非 token 形状的单段路径不是入口，也不能作为无凭据静态访问通过
	if rec := get(g, host, "/ui/proxies/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("shape-mismatch entry status = %d, want 404", rec.Code)
	}
}

func TestNoCookieIs404Everywhere(t *testing.T) {
	g, host := newTestGateway(t)
	for _, target := range []string{"/", "/ui/", "/ui", "/version", "/proxies", "/configs"} {
		if rec := get(g, host, target, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("no-cookie %s status = %d, want 404", target, rec.Code)
		}
	}
	// 认证后 /ui 才允许 301 规范化（未认证的 301 会泄露面板路径存在性）
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	if rec := get(g, host, "/ui", auth); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("authed /ui status = %d, want 301", rec.Code)
	}
}

func TestBadCookieIs404(t *testing.T) {
	g, host := newTestGateway(t)
	rec := get(g, host, "/ui/", map[string]string{"Cookie": cookieName + "=wrong"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad cookie status = %d, want 404", rec.Code)
	}
}

func TestStaticServingAndSPAFallback(t *testing.T) {
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}

	if rec := get(g, host, "/ui/", auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), "INDEX") {
		t.Fatalf("/ui/ = %d %q", rec.Code, rec.Body.String())
	}
	// history 路由深链：无扩展名不存在路径回退 index.html
	if rec := get(g, host, "/ui/proxies", auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), "INDEX") {
		t.Fatalf("/ui/proxies deep link = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(g, host, "/ui/_nuxt/app.js", auth); rec.Code != 200 || rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset = %d %q", rec.Code, rec.Body.String())
	}
	// 有扩展名但不存在 → 404（不回退）
	if rec := get(g, host, "/ui/_nuxt/missing.js", auth); rec.Code != 404 {
		t.Fatalf("missing asset = %d, want 404", rec.Code)
	}
	// 目录请求禁列表：_nuxt/ 无 index.html → 404
	if rec := get(g, host, "/ui/_nuxt/", auth); rec.Code != 404 {
		t.Fatalf("directory listing status = %d, want 404", rec.Code)
	}
	// /ui 无尾斜杠 → 301 规范化
	if rec := get(g, host, "/ui", auth); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("/ui redirect = %d, want 301", rec.Code)
	}
}

func TestStaticRejectsTraversal(t *testing.T) {
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	// ../ 试图逃出 uiDir：必须 404 且绝不回内容
	rec := get(g, host, "/ui/..%2Fsecret.txt", auth)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatal("traversal leaked file content")
	}
}

func TestHostHeaderMustBeLoopback(t *testing.T) {
	g, _ := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	// DNS rebinding 场景：Host 是攻击者域名 → 403
	if rec := get(g, "evil.example:39097", "/ui/", auth); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host status = %d, want 403", rec.Code)
	}
	// 同地址不同端口也不行（网关端口绑定语义）
	if rec := get(g, "127.0.0.1:9999", "/ui/", auth); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-port host status = %d, want 403", rec.Code)
	}
}

func TestOriginHeaderMustBeSameSource(t *testing.T) {
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	// 伪造 Origin（跨站 WS 场景的 HTTP 形态）→ 403
	if rec := get(g, host, "/version", map[string]string{"Origin": "http://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("forged origin status = %d, want 403", rec.Code)
	}
	// 同源 Origin 放行
	if rec := get(g, host, "/ui/", map[string]string{
		"Cookie": auth["Cookie"], "Origin": "http://127.0.0.1:39097",
	}); rec.Code != 200 {
		t.Fatalf("same origin status = %d, want 200", rec.Code)
	}
	// localhost 同源放行
	if rec := get(g, "localhost:39097", "/ui/", map[string]string{
		"Cookie": auth["Cookie"], "Origin": "http://localhost:39097",
	}); rec.Code != 200 {
		t.Fatalf("localhost origin status = %d, want 200", rec.Code)
	}
}

func TestAPIPathsWithDeadBackendAre502(t *testing.T) {
	// 反代已装配但内核不可达：502 且错误体是固定文案（审计③：不回显 sock 路径/secret）
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	for _, target := range []string{"/version", "/proxies"} {
		rec := get(g, host, target, auth)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s status = %d, want 502", target, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "gw-secret") || strings.Contains(body, "none.sock") {
			t.Fatalf("%s error body leaks internals: %q", target, body)
		}
	}
	// /ui/../ 是静态分支的穿越拒绝，保持 404
	if rec := get(g, host, "/ui/../", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d, want 404", rec.Code)
	}
}

func TestConfigJSDynamicallyGenerated(t *testing.T) {
	// v0.4.7：/ui/config.js 不读磁盘（ui/ 磁盘文件 defaultBackendURL 为空串，
	// metacubexd 因此停在"未检测到后端"设置屏），网关按自身端口动态生成
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	rec := get(g, host, "/ui/config.js", auth)
	if rec.Code != 200 {
		t.Fatalf("config.js status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// 动态注入的部署定制入口：defaultBackendURL 按白名单内请求 Host 回显（P3-1）
	if !strings.Contains(body, "__METACUBEXD_CONFIG__") || !strings.Contains(body, "defaultBackendURL") {
		t.Fatalf("config.js body missing dynamic config: %q", body)
	}
	if !strings.Contains(body, "http://"+host) {
		t.Fatalf("config.js body missing backend URL for test host: %q", body)
	}
	// P3-1：localhost 入口路径同样按其 Host 回显（非硬编码 127.0.0.1）
	if rec := get(g, "localhost:39097", "/ui/config.js", auth); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), "http://localhost:39097") {
		t.Fatalf("localhost config.js = %d %q", rec.Code, rec.Body.String())
	}
	// 磁盘文件内容不得透传（newTestGateway 写入的空 defaultBackendURL 构建配置）
	if strings.Contains(body, "window.config") {
		t.Fatalf("config.js served disk file instead of dynamic body: %q", body)
	}
	// Content-Type 必须是 JS：metacubexd 以 <script src> 引入
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/javascript") {
		t.Fatalf("config.js Content-Type = %q, want application/javascript", ct)
	}
	// v0.4.8：no-store 防浏览器缓存旧配置复现"设置屏"（内容随端口/安装而变且无版本化文件名）
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("config.js Cache-Control = %q, want no-store", cc)
	}
	// 审计④：动态生成体同样不得携带 secret（secret 仅服务端反代注入）
	if strings.Contains(body, "gw-secret") {
		t.Fatal("config.js leaked secret")
	}
	// 防泄露语义不变：无 cookie 访问 config.js 仍 404
	if rec := get(g, host, "/ui/config.js", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no-cookie config.js status = %d, want 404", rec.Code)
	}
}

func TestSWKillSwitch(t *testing.T) {
	// v0.4.14：ui/sw.js 不透传磁盘 Workbox SW（历史会话已安装的旧 SW 会以 precache
	// 旧 app shell 应答 /ui/* 导航，绕过服务端全部缓存语义）——改发标准解除注册脚本。
	// 浏览器导航时并行做 SW 更新检查（绕过 HTTP 缓存），字节有变即安装本脚本并使
	// 注册冗余，此后 /ui/* 导航直走网络。
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	rec := get(g, host, "/ui/sw.js", auth)
	if rec.Code != 200 {
		t.Fatalf("sw.js status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "unregister") {
		t.Fatalf("sw.js body missing unregister kill switch: %q", body)
	}
	// 磁盘 Workbox 脚本不得透传（fixture 已写入含 workbox/precache 字样的磁盘 sw.js）
	if strings.Contains(body, "workbox") || strings.Contains(body, "precach") {
		t.Fatalf("sw.js leaked disk workbox script: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/javascript") {
		t.Fatalf("sw.js Content-Type = %q, want application/javascript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("sw.js Cache-Control = %q, want no-store (SW 更新检查必须拉到新脚本)", cc)
	}
	// 防泄露语义不变：无 cookie 访问仍 404
	if rec := get(g, host, "/ui/sw.js", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no-cookie sw.js status = %d, want 404", rec.Code)
	}
}

func TestIndexNoStoreAndConfigCacheBust(t *testing.T) {
	// v0.4.9：index.html 恒 no-store（防浏览器按 Last-Modified 启发式缓存旧 index——
	// 旧 index 引用旧 config.js 会复现"设置屏"），且 config.js 引用改写为
	// ?v=<startedAt>（每次守护重启变化 → 新缓存键，缓存击穿）。
	// 覆盖 index 的三种到达：目录请求 /ui/、显式 /ui/index.html、SPA 回退 /ui/proxies。
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	wantSrc := fmt.Sprintf("config.js?v=%d", g.startedAt)
	for _, target := range []string{"/ui/", "/ui/index.html", "/ui/proxies"} {
		rec := get(g, host, target, auth)
		if rec.Code != 200 {
			t.Fatalf("%s status = %d, want 200", target, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", target, cc)
		}
		body := rec.Body.String()
		// 替换必须命中：防 metacubexd 升级改写法后静默失效（serveIndex 按原样回退）
		if !strings.Contains(body, wantSrc) {
			t.Fatalf("%s body missing rewritten script src %q: %q", target, wantSrc, body)
		}
		if strings.Contains(body, `<script src="config.js"`) {
			t.Fatalf("%s body still has unrewritten config.js reference: %q", target, body)
		}
		// 原有内容不丢失
		if !strings.Contains(body, "INDEX") {
			t.Fatalf("%s body lost original content: %q", target, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s Content-Type = %q, want text/html", target, ct)
		}
		// v0.4.14：导航应答即清本源 SW/缓存存储（历史遗留 SW precache 绕过服务端缓存
		// 语义的清障）；cookie 不在 "storage" 指令范围内，会话不受影响
		if csd := rec.Header().Get("Clear-Site-Data"); !strings.Contains(csd, "storage") {
			t.Fatalf("%s Clear-Site-Data = %q, want storage directive", target, csd)
		}
	}
	// config.js 拦截分支语义不变：no-store + 动态生成体（双层防缓存的第二层）
	rec := get(g, host, "/ui/config.js", auth)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(rec.Body.String(), "__METACUBEXD_CONFIG__") {
		t.Fatalf("config.js = %d cc=%q body=%q", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	// 非 index 的静态资源不受影响（仍走 http.ServeFile 原样透传）
	if rec := get(g, host, "/ui/_nuxt/app.js", auth); rec.Code != 200 || rec.Body.String() != "console.log(1)" {
		t.Fatalf("asset = %d %q", rec.Code, rec.Body.String())
	}
}
