package app

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// freePort 探测一个空闲端口（listen :0 后关闭；测试内小窗口重用，竞态可接受）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// seedUIDir 造一个最小可服务的 ui/（index.html 直下）。
func seedUIDir(t *testing.T, home paths.Home) {
	t.Helper()
	if err := os.MkdirAll(home.UIDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.UIDir(), "index.html"), []byte("<html>ui</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStartGatewayDisabled(t *testing.T) {
	home := paths.Home(t.TempDir())
	gw, url, err := startGateway(home, store.Settings{PanelEnabled: false, GatewayPort: store.DefaultGatewayPort})
	if err != nil || gw != nil || url != "" {
		t.Fatalf("disabled: gw=%v url=%q err=%v, want nil/empty/nil", gw, url, err)
	}
}

func TestStartGatewayUIDirMissing(t *testing.T) {
	home := paths.Home(t.TempDir())
	gw, url, err := startGateway(home, store.Settings{PanelEnabled: true, GatewayPort: store.DefaultGatewayPort})
	if err != nil || gw != nil || url != "" {
		t.Fatalf("ui missing: gw=%v url=%q err=%v, want nil/empty/nil", gw, url, err)
	}
}

func TestStartGatewayPortBusyDegrades(t *testing.T) {
	port := freePort(t)
	occupant, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()

	home := paths.Home(t.TempDir())
	seedUIDir(t, home)
	gw, url, err := startGateway(home, store.Settings{PanelEnabled: true, GatewayPort: port})
	if err != nil || gw != nil || url != "" {
		t.Fatalf("port busy: gw=%v url=%q err=%v, want degraded nil/empty/nil", gw, url, err)
	}
}

func TestStartGatewayServes(t *testing.T) {
	port := freePort(t)
	home := paths.Home(t.TempDir())
	seedUIDir(t, home)
	gw, url, err := startGateway(home, store.Settings{PanelEnabled: true, GatewayPort: port})
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	if gw == nil || !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.Contains(url, "/ui/") {
		t.Fatalf("gw=%v url=%q", gw, url)
	}
	// 入口 URL 可达（302）。WHY 不跟随重定向：后续 /ui/ 需 cookie，无 jar 的 client
	// 跟随过去必得 404——那正是网关的正确行为（本断言只验入口本身）
	noredirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noredirect.Get(url)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("entry status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("entry Location = %q, want /ui/", loc)
	}
	if len(resp.Cookies()) != 1 || resp.Cookies()[0].Path != "/" {
		t.Fatalf("entry cookies = %+v, want one Path=/ cookie", resp.Cookies())
	}
	gw.close()
	// close 后端口释放
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !gatewayReachable(port) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("gateway port still reachable after close")
}

func TestPanelFatalOnDegradedPaths(t *testing.T) {
	// 三种降级场景必须 fatal（M1-plan §5.2）：ui 缺失 / 未运行 / disabled
	// WHY 显式 settings：不写则探测默认端口 39097——真机实例恰在跑该端口时本测试
	// 会误判"可达"。测试不得依赖外部机器状态（写空闲高位端口隔离）。
	home := paths.Home(t.TempDir())
	writeIsolatedSettings(t, home)
	if err := Panel(home); err == nil || !strings.Contains(err.Error(), "ui missing") {
		t.Fatalf("Panel(ui missing) err = %v, want ui missing fatal", err)
	}
	seedUIDir(t, home)
	if err := Panel(home); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("Panel(not running) err = %v, want unreachable fatal", err)
	}
}

// writeIsolatedSettings 向临时 home 写入带空闲端口的 settings，隔离真机默认端口。
func writeIsolatedSettings(t *testing.T, home paths.Home) {
	t.Helper()
	settings := store.DefaultSettings()
	settings.GatewayPort = freePort(t)
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
}
