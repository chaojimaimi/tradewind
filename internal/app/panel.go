package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/chaojimaimi/tradewind/internal/panel"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// panelServer 持有网关的 http.Server 与监听器，供 start 生命周期管理。
type panelServer struct {
	srv *http.Server
	ln  net.Listener
}

// startGateway 启动面板网关（仅绑定 127.0.0.1），返回入口 URL。
// 降级哲学（M1-plan §5.2）：panel_enabled=false / ui/ 缺失 / 端口被占 → 打印警告
// 返回 (nil, "", nil)，start 继续运行；token 生成失败等数据目录错误 → 显式失败。
func startGateway(home paths.Home, settings store.Settings) (*panelServer, string, error) {
	if !settings.PanelEnabled {
		fmt.Println("panel: disabled (panel_enabled=false)")
		return nil, "", nil
	}
	if !uiDirReady(home) {
		fmt.Printf("panel: ui/ missing at %s, gateway disabled\n", home.UIDir())
		return nil, "", nil
	}
	token, err := panel.EnsurePanelToken(home)
	if err != nil {
		return nil, "", err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", settings.GatewayPort))
	if err != nil {
		fmt.Printf("panel: port %d unavailable (%v), gateway disabled\n", settings.GatewayPort, err)
		return nil, "", nil
	}
	gw := panel.NewGateway(panel.Config{
		Port:     settings.GatewayPort,
		UIDir:    home.UIDir(),
		Token:    token,
		SockPath: home.SockFile(),
		Secret:   store.ReadSecret(home),
	})
	ps := &panelServer{srv: &http.Server{Handler: gw}, ln: ln}
	go func() {
		// Serve 在 Close 时返回 ErrServerClosed——网关停止路径不视为错误
		_ = ps.srv.Serve(ps.ln)
	}()
	return ps, fmt.Sprintf("http://127.0.0.1:%d/ui/%s/", settings.GatewayPort, token), nil
}

// close 停止网关。WHY Close 而非 Shutdown：浏览器 WS 长连接会让 Shutdown 一直等待
// （需 ctx 超时兜底）；此刻内核即将停止，长连接已无意义，立即断开是正确语义。
func (p *panelServer) close() {
	_ = p.srv.Close()
}

// uiDirReady 校验 metacubexd 落位形态：index.html 位于 ui/ 直下（M1-plan §1.3）。
func uiDirReady(home paths.Home) bool {
	info, err := os.Stat(filepath.Join(home.UIDir(), "index.html"))
	return err == nil && !info.IsDir()
}

// PanelEntryURL 前置校验并返回凭据入口 URL（`panel` 与 `panel --url` 共用）。
// 网关降级场景（panel_enabled=false / ui 缺失 / 未运行）明确 fatal（M1-plan §5.2）。
// WHY ensure：网关可达时 token 必已存在，正常走只读路径；异常缺失时补齐以自愈。
func PanelEntryURL(home paths.Home) (string, error) {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return "", err
	}
	if !settings.PanelEnabled {
		return "", errors.New("app: panel disabled by settings (panel_enabled=false)")
	}
	if !uiDirReady(home) {
		return "", fmt.Errorf("app: panel ui missing at %s (install metacubexd dist there)", home.UIDir())
	}
	if !gatewayReachable(settings.GatewayPort) {
		return "", errors.New("app: panel gateway not reachable; run `tradewind start` first")
	}
	token := panel.ReadPanelToken(home)
	if token == "" {
		token, err = panel.EnsurePanelToken(home)
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/ui/%s/", settings.GatewayPort, token), nil
}

// Panel 编排 `tradewind panel`：前置校验 → 打印凭据入口 URL → open 打开浏览器。
func Panel(home paths.Home) error {
	url, err := PanelEntryURL(home)
	if err != nil {
		return err
	}
	fmt.Println(url)
	if err := exec.Command("open", url).Run(); err != nil {
		return fmt.Errorf("app: open browser: %w", err)
	}
	return nil
}

// PanelURLOnly 编排 `tradewind panel --url`：仅打印入口 URL 不开浏览器
// （壳"网页面板"按钮复用，M2-plan §2.5）。
func PanelURLOnly(home paths.Home) error {
	url, err := PanelEntryURL(home)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}

// gatewayReachable 探测网关端口是否有监听者（1s 超时）。
func gatewayReachable(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
