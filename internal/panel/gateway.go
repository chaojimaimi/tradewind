package panel

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// PACSource 实时取 PAC 参数（mixed 端口 + bypass 生效值，批④ §3.2）。
	// LoadSettings/Validate 失败由实现返回 error → /pac 显式 5xx（不兜底输出——
	// 兜底输出会让注入防护被静默旁路）。nil = 不挂 /pac（仅中间装配状态）。
	PACSource func() (mixedPort int, bypass []string, err error)
}

// Gateway 是面板网关 http.Handler：
//
//	GET /ui/<token>/   凭据入口 → Set-Cookie(Path=/) → 302 /ui/#/overview
//	GET /ui/*          cookie 校验 → 静态托管（SPA 回退，禁目录列表，index 防缓存）
//	/* 其余一切         cookie 校验 → 反代内核 API（根路径）
//
// WHY 挂 /ui/ 前缀：metacubexd history 路由含 /proxies 等与 API 同名页面路径，
// 挂根会在刷新/深链时把页面路由误送进 API 反代（M1-plan §5.2）。
type Gateway struct {
	cfg       Config
	proxy     http.Handler // 内核反代；nil 时非 /ui 请求按 404（仅中间装配状态）
	startedAt int64        // 网关进程启动时刻（UnixNano），index.html 的 config.js 缓存击穿参数
}

// NewGateway 构造网关（含内核反代装配）。
func NewGateway(cfg Config) *Gateway {
	return &Gateway{
		cfg:   cfg,
		proxy: newCoreProxy(cfg.SockPath, cfg.Secret),
		// WHY 缓存击穿参数（v0.4.9）：每次守护重启/升级必然变化，使 index.html 里
		// 改写出的 config.js?v=<startedAt> 形成新缓存键，浏览器必然重新拉取
		startedAt: time.Now().UnixNano(),
	}
}

// ServeHTTP 路由：先过纵深防御（Host/Origin），再按路径分流。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.allowHostOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/pac" {
		// 批④ §3.2：/pac 显式开洞——requireCookie 之前放行（无鉴权：内容只有
		// 127.0.0.1:mixed_port 与 bypass 域名，无 token/路径信息；loopback 绑定 +
		// allowHostOrigin 前置保持纵深防御）。macOS 系统代理按 URL 直接拉取，
		// 无浏览器会话可携带 cookie，鉴权会令 PAC 恒拉取失败。
		g.servePAC(w, r)
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

// servePAC 托管实时合成的 PAC 文件（批④ §3.2）。实时合成：settings 每请求重读，
// mixed_port/bypass_domains 变更后无需重启网关即反映到 PAC 内容。
// LoadSettings/Validate 失败 → 503 显式失败（不兜底输出：兜底会让 bypass 白名单
// 校验被静默旁路，注入面重新打开）。
func (g *Gateway) servePAC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r) // 非 GET 不泄露该路由存在性（同 404 语义）
		return
	}
	if g.cfg.PACSource == nil {
		http.NotFound(w, r)
		return
	}
	mixedPort, bypass, err := g.cfg.PACSource()
	if err != nil {
		http.Error(w, "pac unavailable", http.StatusServiceUnavailable)
		return
	}
	// WHY no-store：内容随 settings 实时变化且无版本化文件名，浏览器/系统缓存旧
	// PAC 会让端口/绕过变更延迟生效（config.js 同款缓存哲学）
	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(PACScript(mixedPort, bypass)))
}

// PACScript 实时合成 FindProxyForURL（纯函数，golden 测试锚）：
// bypass 命中 → DIRECT，其余 → PROXY 127.0.0.1:mixed_port。
func PACScript(mixedPort int, bypass []string) string {
	var b strings.Builder
	b.WriteString("// Tradewind PAC (auto-generated from settings; do not edit)\n")
	b.WriteString("function FindProxyForURL(url, host) {\n")
	for _, d := range bypass {
		fmt.Fprintf(&b, "  if (%s) return \"DIRECT\";\n", pacCondition(d))
	}
	fmt.Fprintf(&b, "  return \"PROXY 127.0.0.1:%d\";\n", mixedPort)
	b.WriteString("}\n")
	return b.String()
}

// pacCondition 单条 bypass pattern 的 JS 判定式。字符集白名单（store 校验层）
// 已排除引号/分号/空格/&，%q 再转义一道（纵深防御，注入拒绝由测试钉死）。
func pacCondition(pattern string) string {
	switch {
	case pattern == "<local>":
		return "isPlainHostName(host)"
	case strings.Contains(pattern, "/"): // CIDR 形态（如 169.254/16）
		if net, mask, ok := pacCIDR(pattern); ok {
			return fmt.Sprintf("isInNet(host, %q, %q)", net, mask)
		}
		return fmt.Sprintf("shExpMatch(host, %q)", pattern)
	default:
		// 通配（*.local）/域名/IP 字面量统一 shExpMatch：* ? 通配、字面量精确匹配
		return fmt.Sprintf("shExpMatch(host, %q)", pattern)
	}
}

// pacCIDR 解析 "169.254/16" 形态 → (网络地址, 子网掩码)。缺省八位组补零
// （169.254 → 169.254.0.0）；非法（>4 段/越界/位数越界）返回 ok=false 退化 shExpMatch。
func pacCIDR(pattern string) (string, string, bool) {
	ipStr, bitsStr, found := strings.Cut(pattern, "/")
	if !found {
		return "", "", false
	}
	bits, err := strconv.Atoi(bitsStr)
	if err != nil || bits < 0 || bits > 32 {
		return "", "", false
	}
	octets := strings.Split(ipStr, ".")
	if len(octets) == 0 || len(octets) > 4 {
		return "", "", false
	}
	var nums [4]int
	for i, o := range octets {
		n, err := strconv.Atoi(o)
		if err != nil || n < 0 || n > 255 {
			return "", "", false
		}
		nums[i] = n
	}
	ip := uint32(nums[0])<<24 | uint32(nums[1])<<16 | uint32(nums[2])<<8 | uint32(nums[3])
	// bits=0 → mask 0（Go 左移计数 ≥ 位宽得 0）；bits=32 → 全 1
	mask := uint32(0)
	if bits > 0 {
		mask = ^uint32(0) << (32 - bits)
	}
	net := ip & mask
	oct := func(v uint32) int { return int(v) }
	return fmt.Sprintf("%d.%d.%d.%d", oct(net>>24&255), oct(net>>16&255), oct(net>>8&255), oct(net&255)),
		fmt.Sprintf("%d.%d.%d.%d", oct(mask>>24&255), oct(mask>>16&255), oct(mask>>8&255), oct(mask&255)),
		true
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
	// WHY 落地 /ui/#/overview（v0.4.9，2026-11-19 用户指令"链接锁定为 /ui/#/overview"）：
	// 入口 token 本身即凭据（无 cookie 也走此路径设置新 cookie），重定向携带片段后
	// 浏览器落地即概览页，免手填后端/免二次导航。直接开 /ui/#/overview 对无 cookie
	// 浏览器是 404（防泄露语义），故锁定落在重定向目标而非入口形态。
	// WHY 落地页带 ?v=<startedAt>（v0.4.12）：文档缓存键 /ui/（无查询串）会被 v0.4.9
	// 之前的旧 index.html 长期占据——Last-Modified 启发式新鲜期内浏览器根本不回源，
	// 响应侧 no-store 管不到已缓存的历史条目（v0.4.9 上线后用户浏览器仍复现"设置屏"
	// 的残余机理）。落地 URL 每次守护重启换新缓存键（查询串是键的一部分），必回源；
	// 查询串不影响路由（serveStatic 按 Path 分流），片段 #/overview 照旧由 SPA 消费。
	http.Redirect(w, r, fmt.Sprintf("/ui/?v=%d#/overview", g.startedAt), http.StatusFound)
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
// 自行解析路径而非直接用 http.FileServer，目的有三：
//  1. 禁目录列表（目录请求一律走其 index.html，无 index.html 即 404）；
//  2. 不存在的无扩展名路径回退 index.html——history 路由（/ui/proxies 等）深链可刷新；
//  3. index.html 恒 no-store 输出，且把 config.js 引用改写为带 startedAt 的查询参数
//     （v0.4.9 缓存击穿，见 serveIndex）——不走 http.ServeFile 的 Last-Modified 启发式。
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
		// WHY no-store（v0.4.8）：内容随网关端口/安装而变且无版本化文件名，浏览器缓存旧值
		// 会复现"设置屏"症状（2026-11-19 自动化浏览器实测：缓存旧空配置须绕缓存强刷才收敛）
		w.Header().Set("Cache-Control", "no-store")
		// WHY 回显 r.Host 而非硬编码 127.0.0.1（审查 P3-1）：ServeHTTP 已先过 allowHostOrigin
		// 白名单（Host 仅可能是本机网关两种 loopback 形态，无注入面），回显可兼收
		// localhost 入口路径——浏览器按 Host 连后端，与入口地址一致避免混用回环名。
		fmt.Fprintf(w, "window.__METACUBEXD_CONFIG__ = { defaultBackendURL: %q };",
			fmt.Sprintf("http://%s", r.Host))
		return
	}
	if cleaned == "/sw.js" {
		// WHY kill-switch（v0.4.14，Safari 白屏终章配套）：metacubexd 是 Nuxt PWA，
		// ui/ 自带 sw.js + workbox-*.js；用户浏览器历史会话可能已安装旧 SW，其 precache
		// 会以旧 app shell（含旧 config.js）应答后续 /ui/* 导航——服务端 no-store/缓存
		// 击穿全部被绕过。标准解除注册脚本：浏览器在导航时并行做 SW 更新检查（绕过
		// HTTP 缓存），字节有变 → 安装本脚本 → 评估期 unregister → 注册即冗余 →
		// 之后所有 /ui/* 导航不再受 SW 控制、直走网络。
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, "self.registration.unregister();")
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
	// index.html 统一走 serveIndex（覆盖三种到达：目录请求 /ui/、显式 /ui/index.html、
	// SPA 回退——metacubexd 是 hash 路由实际只有 /ui/ 一个 HTML 入口，统一处理更稳）
	if strings.HasSuffix(full, "index.html") {
		g.serveIndex(w, r, full)
		return
	}
	http.ServeFile(w, r, full)
}

// serveIndex 托管 index.html：内容改写 + 防缓存头，不走 http.ServeFile。
// WHY（v0.4.9，2026-11-19 用户指令"链接锁定为 /ui/#/overview 确保正常打开"）：
// ServeFile 仅按 Last-Modified 启发式协商缓存，磁盘 mtime 老时浏览器本地新鲜期可达数周——
// 升级后浏览器仍用旧 index.html（引用旧 config.js），metacubexd 复现"设置屏"。
// index 恒 no-store + config.js 引用改写为 ?v=<startedAt>（每次重启变化 → 新缓存键，
// 且 config.js 拦截分支本身已 no-store），双层保证浏览器拿到的都是注入后的配置。
func (g *Gateway) serveIndex(w http.ResponseWriter, r *http.Request, full string) {
	raw, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 真实 index.html（metacubexd 1.275.2）的引用形态是 <script src="config.js"
	// （无 ./ 前缀、双引号、单次出现）。替换未命中时（如 metacubexd 升级改写法）
	// 按原样输出、不猜测其他形态——是否命中可由断言 body 含 config.js?v= 直接检验，
	// 防升级后静默失效。
	patched := strings.Replace(string(raw),
		`<script src="config.js"`,
		fmt.Sprintf(`<script src="config.js?v=%d"`, g.startedAt),
		1)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// WHY Clear-Site-Data（v0.4.14，SW kill 配套）：历史会话遗留的 SW/缓存存储在
	// 网络导航应答即清（旧 SW 失控后的首次网络导航即净态；若该导航本身被旧 SW
	// precache 拦截则头到不了浏览器——此一次性过渡由 kill 脚本收敛）。
	// "storage" 覆盖 localStorage/IndexedDB/SW 与 Cache Storage（不含 cookie——
	// 面板会话 cookie 独立指令，不受影响）；代价是 metacubexd UI 偏好每次重置，
	// 可接受（后端地址由 config.js 注入自动回填）。不支持该头的浏览器静默忽略。
	w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
	w.Write([]byte(patched))
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
