package panel

import (
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
	if err := os.WriteFile(filepath.Join(ui, "index.html"), []byte("<!doctype html><div id=app>INDEX</div>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ui, "_nuxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ui, "_nuxt", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
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
}

func TestTokenEntryWithoutTrailingSlash(t *testing.T) {
	g, host := newTestGateway(t)
	rec := get(g, host, "/ui/"+testToken, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
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

func TestAPIPathsWithoutProxyReturn404(t *testing.T) {
	// 中间装配状态：cookie 合法但反代未装配 → 404（不 panic）
	g, host := newTestGateway(t)
	auth := map[string]string{"Cookie": cookieName + "=" + testToken}
	for _, target := range []string{"/version", "/proxies", "/ui/../"} {
		if rec := get(g, host, target, auth); rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 (proxy not wired yet)", target, rec.Code)
		}
	}
}
