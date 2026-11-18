package panel

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
)

// newCoreProxy 构造指向内核 unix socket 的反向代理（M1-plan §5.2 API 反代）。
//
// 三要点：
//   - Authorization 无条件覆写：无论 UI 传什么凭据，内核只认 secret——
//     它由网关注入，永不出网关进程（#7 审计③的一半）；
//   - FlushInterval=-1：/traffic /connections /logs 是无限流端点，
//     默认缓冲会攒满 4KB 才下发，流式图表会卡死（DESIGN §7.5 教训）；
//   - WebSocket Upgrade 由 httputil 原生透传（Director 仅改 URL/Authorization 头）。
func newCoreProxy(sockPath, secret string) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// 自定义拨号忽略 URL host，一律连 unix socket（浏览器无法直连 socket 的修复点）
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "mihomo-core" // 仅为 URL 合法性；DialContext 忽略之
			req.Header.Set("Authorization", "Bearer "+secret)
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// WHY 不回显 err：细节可能包含 sock 路径等内部布局信息（#7 审计③）
			http.Error(w, "core gateway error", http.StatusBadGateway)
		},
	}
}
