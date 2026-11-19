package panel

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// cookieName 是面板会话 cookie 名；值即 panel token（等价凭据，由 HttpOnly+loopback+Lax 保护）。
const cookieName = "tradewind_panel"

// Config 是网关构造参数。
type Config struct {
	Port     int    // 监听端口（Host/Origin 白名单以此为准）
	UIDir    string // metacubexd 静态资源目录
	Token    string // panel token（凭据）
	SockPath string // 内核 unix socket（反代目标）
	Secret   string // 内核 secret（反代覆写 Authorization 用，永不出网关进程）
}

// Gateway 是面板网关 http.Handler：
//
//	GET /ui/<token>/   凭据入口 → Set-Cookie(Path=/) → 302 /ui/
//	GET /ui/*          cookie 校验 → 静态托管（SPA 回退，禁目录列表）
//	/* 其余一切         cookie 校验 → 反代内核 API（根路径）
//
// WHY 挂 /ui/ 前缀：metacubexd history 路由含 /proxies 等与 API 同名页面路径，
// 挂根会在刷新/深链时把页面路由误送进 API 反代（M1-plan §5.2）。
type Gateway struct {
	cfg   Config
	proxy http.Handler // 内核反代；nil 时非 /ui 请求按 404（仅中间装配状态）
}

// NewGateway 构造网关（含内核反代装配）。
func NewGateway(cfg Config) *Gateway {
	return &Gateway{cfg: cfg, proxy: newCoreProxy(cfg.SockPath, cfg.Secret)}
}

// ServeHTTP 路由：先过纵深防御（Host/Origin），再按路径分流。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.allowHostOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/ui" {
		// 无尾斜杠：凭据校验通过后再规范化到 /ui/
		// WHY 先校验：未认证的 301 会向本机其他进程泄露面板路径的存在性
		g.requireCookie(w, r, func(ww http.ResponseWriter, rr *http.Request) {
			http.Redirect(ww, rr, "/ui/", http.StatusMovedPermanently)
		})
		return
	}
	if seg, ok := isTokenEntry(r.URL.Path); ok {
		g.handleTokenEntry(w, r, seg)
		return
	}
	if r.URL.Path == "/ui/" || strings.HasPrefix(r.URL.Path, "/ui/") {
		g.requireCookie(w, r, g.serveStatic)
		return
	}
	g.requireCookie(w, r, g.serveAPI)
}

// serveAPI 把根路径请求交给内核反代（装配见 proxy 装配点）。
func (g *Gateway) serveAPI(w http.ResponseWriter, r *http.Request) {
	if g.proxy == nil {
		http.NotFound(w, r)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// isTokenEntry 判断路径是否凭据入口形态：/ui/<64hex>（带或不带尾斜杠）。
// SPA 深链（如 /ui/proxies）形状不符，不会被误判为入口。
func isTokenEntry(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/ui/")
	if !ok {
		return "", false
	}
	seg := strings.TrimSuffix(rest, "/")
	if seg == "" || strings.Contains(seg, "/") || !IsTokenShape(seg) {
		return "", false
	}
	return seg, true
}

// handleTokenEntry 校验 token 并下发会话 cookie（seg 为入口段，调用方已做过形状判断）。
// WHY 404 而非 401/403：不向本机其他进程泄露网关/凭据机制的存在性（M1-plan §5.2）。
func (g *Gateway) handleTokenEntry(w http.ResponseWriter, r *http.Request, seg string) {
	if !VerifyToken(seg, g.cfg.Token) {
		http.NotFound(w, r)
		return
	}
	// Path=/ 而非 /ui：API 反代在根路径，RFC 6265 下 Path=/ui 的 cookie
	// 不会随 /proxies 等根路径请求发送，面板将全 404（M1-plan §5.2）
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    g.cfg.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 无 Domain 属性 → host-only cookie，仅本机回环主机名可见
	})
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

// requireCookie 会话校验中间件：无 cookie 或值不符一律 404（同入口的"不泄露"语义）。
func (g *Gateway) requireCookie(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	c, err := r.Cookie(cookieName)
	if err != nil || !VerifyToken(c.Value, g.cfg.Token) {
		http.NotFound(w, r)
		return
	}
	next(w, r)
}

// allowHostOrigin 纵深防御（M1-plan §5.2 ②）：
//   - Host 必须是本机网关地址——防 DNS rebinding（恶意域名 A 记录指向 127.0.0.1 后，
//     浏览器发送的 Host 是攻击者域名）；
//   - Origin 头存在时必须同源——防跨站 WebSocket 读流量；缺失放行
//     （curl/CLI 等非浏览器客户端不带 Origin，不能误拒 smoke 与 CLI）。
func (g *Gateway) allowHostOrigin(r *http.Request) bool {
	loopHost := []string{
		fmt.Sprintf("127.0.0.1:%d", g.cfg.Port),
		fmt.Sprintf("localhost:%d", g.cfg.Port),
	}
	hostOK := false
	for _, h := range loopHost {
		if r.Host == h {
			hostOK = true
			break
		}
	}
	if !hostOK {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		originOK := false
		for _, h := range loopHost {
			if o == "http://"+h {
				originOK = true
				break
			}
		}
		if !originOK {
			return false
		}
	}
	return true
}

// serveStatic 托管 ui/ 静态资源（metacubexd）。
// 自行解析路径而非直接用 http.FileServer，目的有二：
//  1. 禁目录列表（目录请求一律走其 index.html，无 index.html 即 404）；
//  2. 不存在的无扩展名路径回退 index.html——history 路由（/ui/proxies 等）深链可刷新。
//
// 路径包含构造：Clean(rest) 恒为根锚定路径，Join(uiDir, ·) 后不可能逃出 uiDir
// （纵深防御外加显式拒绝 ".."）。
func (g *Gateway) serveStatic(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ui")
	if strings.Contains(rest, "..") {
		http.NotFound(w, r)
		return
	}
	cleaned := filepath.Clean(rest)
	// WHY 动态生成而非读磁盘（v0.4.7，2026-11-19 自动化浏览器截图真机实锤）：metacubexd
	// 渲染后停在"未检测到后端 http://127.0.0.1:9090"设置屏——它读部署定制入口 config.js
	// 的 window.__METACUBEXD_CONFIG__.defaultBackendURL，而 ui/ 磁盘文件该值为空串 →
	// metacubexd 去扫裸 Clash 默认 9090（本网关在 39097），用户每次都要手填后端地址。
	// 注入网关自身 loopback origin 后，任意浏览器首次打开即自动连接；
	// secret 仍由网关反代在服务端注入（覆写 Authorization），浏览器侧无凭据。
	if cleaned == "/config.js" {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		// WHY 回显 r.Host 而非硬编码 127.0.0.1（审查 P3-1）：ServeHTTP 已先过 allowHostOrigin
		// 白名单（Host 仅可能是本机网关两种 loopback 形态，无注入面），回显可兼收
		// localhost 入口路径——浏览器按 Host 连后端，与入口地址一致避免混用回环名。
		fmt.Fprintf(w, "window.__METACUBEXD_CONFIG__ = { defaultBackendURL: %q };",
			fmt.Sprintf("http://%s", r.Host))
		return
	}
	full := filepath.Join(g.cfg.UIDir, cleaned)
	switch {
	case isDir(full):
		// 目录仅允许经 index.html 呈现（禁目录列表，M1-plan §5.2 ④）；无 index.html 即 404
		full = filepath.Join(full, "index.html")
	case !exists(full) && filepath.Ext(cleaned) == "":
		// SPA 回退：不存在且无扩展名 → history 路由深链，回 index.html
		full = filepath.Join(g.cfg.UIDir, "index.html")
	}
	if !exists(full) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
