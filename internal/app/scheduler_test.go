package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

func TestNormalizeUpdateInterval(t *testing.T) {
	// 0/缺失/负值 → 86400（防 0 恒真每 30 分钟轰击端点，M2-plan §2.2）
	for _, in := range []int{0, -1} {
		if got := NormalizeUpdateInterval(in); got != 86400 {
			t.Fatalf("NormalizeUpdateInterval(%d) = %d, want 86400", in, got)
		}
	}
	if got := NormalizeUpdateInterval(43200); got != 43200 {
		t.Fatalf("explicit interval got %d, want 43200", got)
	}
}

func TestDueForUpdateSemantics(t *testing.T) {
	// allow_auto_update 指针：nil=true / 显式 false 关闭；remote 限定；interval 归一参与判据
	now := time.Unix(1000000, 0)
	old := now.Add(-25 * time.Hour).Unix()
	mk := func(item store.ProfileItem) store.ProfileItem {
		item.Type = store.ProfileTypeRemote
		item.Updated = old
		return item
	}
	if !dueForUpdate(mk(store.ProfileItem{UpdateInterval: 0}), now) {
		t.Fatal("nil allow + zero interval (normalized 86400) + stale must be due")
	}
	off := false
	if dueForUpdate(mk(store.ProfileItem{AllowAutoUpdate: &off}), now) {
		t.Fatal("explicit allow=false must never be due")
	}
	on := true
	if !dueForUpdate(mk(store.ProfileItem{AllowAutoUpdate: &on}), now) {
		t.Fatal("explicit allow=true + stale must be due")
	}
	fresh := mk(store.ProfileItem{})
	fresh.Updated = now.Add(-time.Hour).Unix()
	if dueForUpdate(fresh, now) {
		t.Fatal("fresh profile must not be due")
	}
	file := mk(store.ProfileItem{})
	file.Type = store.ProfileTypeFile
	if dueForUpdate(file, now) {
		t.Fatal("file-type profile must never be due")
	}
}

// subServer 按脚本依次返回订阅内容（每次调用取下一段；耗尽后重复最后一段）。
func subServer(t *testing.T, hits *int, bodies ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			*hits++
		}
		body := bodies[len(bodies)-1]
		if n := *hits - 1; n < len(bodies) {
			body = bodies[n]
		}
		w.Write([]byte(body))
	}))
}

func seedRemoteProfile(t *testing.T, home paths.Home, url string, updated int64) store.ProfileItem {
	t.Helper()
	item := store.ProfileItem{
		UID: "rmt01", Type: store.ProfileTypeRemote, Name: "remote", File: "rmt01.yaml",
		URL: url, Updated: updated,
	}
	if err := store.SaveProfileFile(home, item.File, []byte("proxies: []\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIndex(home, store.Index{Current: "rmt01", Items: []store.ProfileItem{item}}); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestAutoUpdateRoundRefreshesUpdatedAndReloadsOnChange(t *testing.T) {
	// v1.2 关键语义：成功拉取即刷新 updated（无论内容变更）；sha256 变更才热重载
	var hits int
	bodyA := "proxies:\n  - name: n1\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n"
	bodyB := strings.Replace(bodyA, "1080", "1081", 1)
	srv := subServer(t, &hits, bodyB, bodyB) // 两轮内容相同
	defer srv.Close()

	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	stale := time.Now().Add(-48 * time.Hour).Unix()
	seedRemoteProfile(t, home, srv.URL, stale)

	// 第一轮：内容 1080→1081 变更 → 刷 updated + 热重载
	if err := autoUpdateRound(context.Background(), home); err != nil {
		t.Fatalf("round1: %v", err)
	}
	idx, _ := store.LoadIndex(home)
	if idx.Items[0].Updated == stale {
		t.Fatal("successful fetch must refresh Updated even when unchanged (v1.2)")
	}
	if len(reloads) != 1 || reloads[0] != http.MethodPut {
		t.Fatalf("content changed → exactly one PUT expected; got %v", reloads)
	}

	// 第二轮：内容不变 → updated 仍要刷新，但零热重载
	idx.Items[0].Updated = stale // 重新置为到期
	if err := store.SaveIndex(home, idx); err != nil {
		t.Fatal(err)
	}
	if err := autoUpdateRound(context.Background(), home); err != nil {
		t.Fatalf("round2: %v", err)
	}
	idx2, _ := store.LoadIndex(home)
	if idx2.Items[0].Updated == stale {
		t.Fatal("round2 must refresh Updated too (unchanged content)")
	}
	if len(reloads) != 1 {
		t.Fatalf("unchanged content must not hot reload; reloads = %v", reloads)
	}
}

func TestFetchForUpdateProxyFallback(t *testing.T) {
	// 两层回退（M2-plan §2.2）：经自身代理失败 → 直连重试一次
	var proxyHits, directHits int
	directBody := "proxies: []\n"
	direct := subServer(t, &directHits, directBody)
	defer direct.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits++
		http.Error(w, "proxy tunnel down", http.StatusBadGateway)
	}))
	defer proxy.Close()

	var reloads []string
	home, cleanup := stubFullCore(t, &reloads) // 内核可达 → 走代理层
	defer cleanup()

	// 代理桩本身即 mixed 端口的监听者（返回 502 模拟隧道坏）：
	// proxyconnect 成功但响应失败 → 回退直连（比拨号拒绝更能覆盖"代理层失败"语义）
	settings := store.DefaultSettings()
	settings.MixedPort = portOf(t, proxy)

	res, err := fetchForUpdate(context.Background(), home, settings, direct.URL)
	if err != nil {
		t.Fatalf("fallback fetch: %v", err)
	}
	if string(res.Body) != directBody {
		t.Fatalf("fallback body = %q", res.Body)
	}
	if proxyHits != 1 || directHits != 1 {
		t.Fatalf("proxyHits=%d directHits=%d, want one each (proxy fail → direct retry)", proxyHits, directHits)
	}
}

func TestFetchForUpdateViaProxyWhenHealthy(t *testing.T) {
	// 内核可达 + 代理健康：必须经代理层拉取（不打直连）
	var proxyHits, directHits int
	body := "proxies: []\n"
	direct := subServer(t, &directHits, body)
	defer direct.Close()
	proxy := subServer(t, &proxyHits, body)
	defer proxy.Close()

	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	settings := store.DefaultSettings()
	settings.MixedPort = portOf(t, proxy)

	res, err := fetchForUpdate(context.Background(), home, settings, direct.URL)
	if err != nil {
		t.Fatalf("proxied fetch: %v", err)
	}
	if string(res.Body) != body {
		t.Fatalf("body = %q", res.Body)
	}
	if proxyHits != 1 || directHits != 0 {
		t.Fatalf("proxyHits=%d directHits=%d, want proxy-only", proxyHits, directHits)
	}
}

// portOf 提取 httptest server 的端口。
func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr := srv.URL
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		t.Fatalf("bad server url %q", addr)
	}
	var p int
	if _, err := fmt.Sscanf(addr[i+1:], "%d", &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFetchForUpdateSkipsProxyWhenDisabledOrUnreachable(t *testing.T) {
	// update_via_proxy 显式 false → 直连（即使内核可达）
	var proxyHits, directHits int
	body := "proxies: []\n"
	direct := subServer(t, &directHits, body)
	defer direct.Close()
	proxy := subServer(t, &proxyHits, body)
	defer proxy.Close()

	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	settings := store.DefaultSettings()
	settings.MixedPort = portOf(t, proxy)
	off := false
	settings.UpdateViaProxy = &off

	if _, err := fetchForUpdate(context.Background(), home, settings, direct.URL); err != nil {
		t.Fatalf("direct fetch: %v", err)
	}
	if proxyHits != 0 || directHits != 1 {
		t.Fatalf("disabled proxy must not be hit; proxyHits=%d directHits=%d", proxyHits, directHits)
	}
}
