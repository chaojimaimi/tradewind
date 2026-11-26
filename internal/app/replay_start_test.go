package app

// replay_start_test.go：节点选择跨形态接力挂在两个启动路径上（批⑥b §4.8，§6 第 8
// 条）——用户态 startCore 拨自己的 socket、TUN startCoreViaService 拨代理入口；
// state 陈旧（Core.Tun 与本次形态相反）也不误拨。夹具在 replay_stub_test.go。

import (
	"context"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// TestStartUserCoreReplaysOnItsOwnSocket 用户态启动路径（lifecycle.startCore）：
// state 陈旧（last_form="tun"、Core.Tun 预置成相反的 true）也不误拨——PUT 落在
// <data_dir>/core.sock 的桩上、代理路径零请求；last_form 为 user（同形态）时不重放。
func TestStartUserCoreReplaysOnItsOwnSocket(t *testing.T) {
	broker := stubBrokerSockPath(t) // 防线：本用例绝不触真锚目录
	selected := []store.ProxySelection{
		{Group: "sel", Node: "n1"},
		{Group: "auto", Node: "n2", Pinned: true},
	}
	proxyTable := map[string]core.Proxy{
		"sel":  {Name: "sel", Type: "Selector", Now: "x"},
		"auto": {Name: "auto", Type: "URLTest", Now: "x", Fixed: "x"},
	}
	start := func(t *testing.T, home paths.Home) {
		t.Helper()
		writeLingeringCoreBinary(t, home)
		secret, err := store.EnsureSecret(home)
		if err != nil {
			t.Fatal(err)
		}
		mgr, err := startCore(home, secret)
		if err != nil {
			t.Fatalf("startCore: %v", err)
		}
		t.Cleanup(func() { _ = mgr.Stop() }) // 回收本用例启动的假内核进程
	}

	t.Run("cross form replays on user sock", func(t *testing.T) {
		home := seedFormSession(t, formNameTun, true, selected) // 陈旧：Core.Tun=true 与本次用户态相反
		brokerStub := newReplayStubAt(t, broker)                // 代理路径上的计数桩（应零命中）
		rec := newReplayStubAt(t, home.SockFile())
		rec.setProxies(proxyTable)
		start(t, home)
		got := rec.putsSnapshot()
		if len(got) != 2 || got[0] != "sel=n1" || got[1] != "auto=n2" {
			t.Fatalf("puts on user sock = %v, want [sel=n1 auto=n2]", got)
		}
		if brokerStub.hitsCount() != 0 {
			t.Fatalf("broker path hits = %d, want 0 (user start never dials the broker path)", brokerStub.hitsCount())
		}
	})

	t.Run("same form does not replay", func(t *testing.T) {
		home := seedFormSession(t, formNameUser, true, selected)
		rec := newReplayStubAt(t, home.SockFile())
		rec.setProxies(proxyTable)
		start(t, home)
		if got := rec.putsSnapshot(); len(got) != 0 {
			t.Fatalf("puts = %v, want none when last_form matches the form", got)
		}
	})
}

// TestStartViaServiceReplaysOnBrokerPath TUN 启动路径（startCoreViaService）：state
// 陈旧（last_form="user"、Core.Tun 预置成相反的 false）→ PUT 落在 tunBrokerSockPath
// 指向的桩上、<data_dir>/core.sock 零请求；last_form 为 tun（同形态）时不重放。
func TestStartViaServiceReplaysOnBrokerPath(t *testing.T) {
	selected := []store.ProxySelection{
		{Group: "sel", Node: "n1"},
		{Group: "auto", Node: "n2", Pinned: true},
	}
	proxyTable := map[string]core.Proxy{
		"sel":  {Name: "sel", Type: "Selector", Now: "x"},
		"auto": {Name: "auto", Type: "URLTest", Now: "x", Fixed: "x"},
	}

	t.Run("cross form replays on broker path", func(t *testing.T) {
		home := seedFormSession(t, formNameUser, false, selected) // 陈旧：Core.Tun=false 与本次 TUN 相反
		svc := &fakeTunService{startOK: true, startPID: 4242}
		stubTunSeams(t, svc)
		rec := newReplayStubAt(t, tunBrokerSockPath)
		rec.setProxies(proxyTable)
		userStub := newReplayStubAt(t, home.SockFile()) // 用户态 sock：应零命中

		pid, err := startCoreViaService(context.Background(), home, "s")
		if err != nil {
			t.Fatalf("startCoreViaService: %v", err)
		}
		if pid != 4242 {
			t.Fatalf("pid = %d, want 4242", pid)
		}
		got := rec.putsSnapshot()
		if len(got) != 2 || got[0] != "sel=n1" || got[1] != "auto=n2" {
			t.Fatalf("puts on broker path = %v, want [sel=n1 auto=n2]", got)
		}
		if userStub.hitsCount() != 0 {
			t.Fatalf("user sock hits = %d, want 0 (TUN start never dials the user sock)", userStub.hitsCount())
		}
	})

	t.Run("same form does not replay", func(t *testing.T) {
		home := seedFormSession(t, formNameTun, false, selected)
		svc := &fakeTunService{startOK: true, startPID: 4243}
		stubTunSeams(t, svc)
		rec := newReplayStubAt(t, tunBrokerSockPath)

		if _, err := startCoreViaService(context.Background(), home, "s"); err != nil {
			t.Fatalf("startCoreViaService: %v", err)
		}
		if got := rec.putsSnapshot(); len(got) != 0 {
			t.Fatalf("puts = %v, want none when last_form matches the form", got)
		}
	})
}
