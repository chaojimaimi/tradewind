package app

// replay_test.go：节点选择跨形态接力的重放规则与跨形态判定（批⑥b §4.8，§6 第 8
// 条）。全部无特权：桩内核在 unix socket 上记录每个 PUT；只有走挂起/超时路径的
// 用例才缩短重放预算（其余用例用生产预算——缩短了反而会在机器负载高时误报）。
// 夹具在 replay_stub_test.go，启动路径的用例在 replay_start_test.go。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// TestReplaySelectedFollowsGroupRules 重放的组规则（严格按序）：Selector → PUT；带
// pinned 的自动组 → PUT；未固定的自动组、无 pinned 的旧记录作用于自动组、组不存在、
// Relay/LoadBalance/未知类型 → 零 PUT；全部成功 → 无告警输出。
func TestReplaySelectedFollowsGroupRules(t *testing.T) {
	sock := filepath.Join(stubSocketDir(t), "core.sock")
	stub := newReplayStubAt(t, sock)
	stub.setProxies(map[string]core.Proxy{
		"sel":        {Name: "sel", Type: "Selector", Now: "x"},
		"auto-fixed": {Name: "auto-fixed", Type: "URLTest", Now: "x", Fixed: "x"},
		"fb-fixed":   {Name: "fb-fixed", Type: "Fallback", Now: "x", Fixed: "x"},
		"fb-pinned":  {Name: "fb-pinned", Type: "Fallback", Now: "x", Fixed: "x"},
		"auto-free":  {Name: "auto-free", Type: "URLTest", Now: "x"},
		"relay":      {Name: "relay", Type: "Relay", Now: "x"},
		"lb":         {Name: "lb", Type: "LoadBalance", Now: "x"},
		"weird":      {Name: "weird", Type: "Weird", Now: "x"},
	})
	readWarnings := captureReplayWarnings(t)
	replaySelected(context.Background(), core.NewClient(sock, "s"), []store.ProxySelection{
		{Group: "sel", Node: "n1"},                      // Selector → PUT
		{Group: "auto-fixed", Node: "n2", Pinned: true}, // 带 pinned 的 URLTest → PUT
		{Group: "fb-pinned", Node: "n9", Pinned: true},  // 带 pinned 的 Fallback → PUT（两类自动组各一例）
		{Group: "auto-free", Node: "n3"},                // 未固定的自动组（无 pinned）→ 零 PUT
		{Group: "fb-fixed", Node: "n4"},                 // 无 pinned 的旧记录作用于自动组 → 零 PUT
		{Group: "gone", Node: "n5", Pinned: true},       // 组不存在 → 跳过
		{Group: "relay", Node: "n6", Pinned: true},      // Relay → 零 PUT
		{Group: "lb", Node: "n7", Pinned: true},         // LoadBalance → 零 PUT
		{Group: "weird", Node: "n8", Pinned: true},      // 未知类型 → 零 PUT
	})
	got := stub.putsSnapshot()
	want := []string{"sel=n1", "auto-fixed=n2", "fb-pinned=n9"}
	if len(got) != len(want) {
		t.Fatalf("puts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("puts[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	if readWarnings() != "" {
		t.Fatalf("all-success replay must stay silent: %q", readWarnings())
	}
}

// TestReplaySelectedSurvivesFailures 单条失败/超时继续、总预算耗尽即停、读失败一行
// 告警后返回；replaySelected 绝不返回错误、绝不 panic（签名即无错误，用例不炸即证）。
func TestReplaySelectedSurvivesFailures(t *testing.T) {
	shortenReplayBudgets(t, 50*time.Millisecond, 400*time.Millisecond)

	t.Run("400 then continue", func(t *testing.T) {
		sock := filepath.Join(stubSocketDir(t), "core.sock")
		stub := newReplayStubAt(t, sock)
		stub.setProxies(map[string]core.Proxy{
			"bad":  {Name: "bad", Type: "Selector"},
			"good": {Name: "good", Type: "Selector"},
		})
		stub.failNext("bad")
		readWarnings := captureReplayWarnings(t)
		replaySelected(context.Background(), core.NewClient(sock, "s"), []store.ProxySelection{
			{Group: "bad", Node: "n"},
			{Group: "good", Node: "m"},
		})
		if got := stub.putsSnapshot(); len(got) != 1 || got[0] != "good=m" {
			t.Fatalf("puts = %v, want only good=m (400 must not stop the replay)", got)
		}
		if out := readWarnings(); !strings.Contains(out, "warning:") || !strings.Contains(out, "1/2 条未重放") {
			t.Fatalf("warnings = %q, want the 1/2 summary line", out)
		}
	})

	t.Run("single hang times out within budget", func(t *testing.T) {
		sock := filepath.Join(stubSocketDir(t), "core.sock")
		stub := newReplayStubAt(t, sock)
		stub.setProxies(map[string]core.Proxy{
			"hang":  {Name: "hang", Type: "Selector"},
			"after": {Name: "after", Type: "Selector"},
		})
		stub.hangNext("hang")
		readWarnings := captureReplayWarnings(t)
		replaySelected(context.Background(), core.NewClient(sock, "s"), []store.ProxySelection{
			{Group: "hang", Node: "n"},
			{Group: "after", Node: "m"},
		})
		if got := stub.putsSnapshot(); len(got) != 1 || got[0] != "after=m" {
			t.Fatalf("puts = %v, want only after=m (timed-out entry skipped, replay continued)", got)
		}
		if out := readWarnings(); !strings.Contains(out, "1/2 条未重放") {
			t.Fatalf("warnings = %q, want the 1/2 summary line", out)
		}
	})

	t.Run("budget exhausted stops early", func(t *testing.T) {
		sock := filepath.Join(stubSocketDir(t), "core.sock")
		stub := newReplayStubAt(t, sock)
		table := map[string]core.Proxy{}
		var selected []store.ProxySelection
		for i := 0; i < 60; i++ {
			name := fmt.Sprintf("g%02d", i)
			table[name] = core.Proxy{Name: name, Type: "Selector"}
			selected = append(selected, store.ProxySelection{Group: name, Node: "n"})
		}
		stub.setProxies(table)
		stub.setHangAll(true)
		readWarnings := captureReplayWarnings(t)
		start := time.Now()
		replaySelected(context.Background(), core.NewClient(sock, "s"), selected)
		elapsed := time.Since(start)
		// 朴素全跑 = 60 × 50ms = 3s；预算 400ms 耗尽即停必须显著快于它
		if elapsed >= 1500*time.Millisecond {
			t.Fatalf("replay took %v, want it to stop when the budget is exhausted", elapsed)
		}
		if got := stub.putsSnapshot(); len(got) != 0 {
			t.Fatalf("hanging stub must record no puts; got %v", got)
		}
		if out := readWarnings(); !strings.Contains(out, "60/60 条未重放") {
			t.Fatalf("warnings = %q, want the exhausted-budget summary", out)
		}
	})

	t.Run("read failure warns once and returns", func(t *testing.T) {
		sock := filepath.Join(stubSocketDir(t), "core.sock")
		stub := newReplayStubAt(t, sock)
		stub.setHangGet(true)
		readWarnings := captureReplayWarnings(t)
		replaySelected(context.Background(), core.NewClient(sock, "s"), []store.ProxySelection{
			{Group: "sel", Node: "n"},
		})
		if got := stub.putsSnapshot(); len(got) != 0 {
			t.Fatalf("puts = %v, want none after read failure", got)
		}
		out := readWarnings()
		if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); len(lines) != 1 || !strings.Contains(out, "读取组类型失败") {
			t.Fatalf("warnings = %q, want exactly one read-failure line", out)
		}
	})
}

// TestReplayOnlyHappensAcrossForms 跨形态判定：last_form 与本次形态相同 / 为空
// （首次启动）/ 读不出 / 无 current 订阅 / selected 为空 → 零 PUT；不同 → 重放。
func TestReplayOnlyHappensAcrossForms(t *testing.T) {
	sock := filepath.Join(stubSocketDir(t), "core.sock")
	stub := newReplayStubAt(t, sock)
	stub.setProxies(map[string]core.Proxy{"sel": {Name: "sel", Type: "Selector"}})
	client := core.NewClient(sock, "s")
	selected := []store.ProxySelection{{Group: "sel", Node: "n1"}}
	// 桩的 PUT 记录跨用例累积：mark/check 按增量断言（want 是相对上一个用例的新增）
	mark := 0
	assertPuts := func(stage string, want []string) {
		t.Helper()
		got := stub.putsSnapshot()[mark:]
		if len(got) != len(want) {
			t.Fatalf("%s: new puts = %v, want %v", stage, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: puts[%d] = %q, want %q", stage, i, got[i], want[i])
			}
		}
		mark += len(want)
	}

	// 同形态：零 PUT
	home := seedFormSession(t, formNameUser, false, selected)
	replayIfFormChanged(context.Background(), home, client, formNameUser)
	assertPuts("same form", nil)

	// 跨形态：重放
	home = seedFormSession(t, formNameTun, true, selected)
	replayIfFormChanged(context.Background(), home, client, formNameUser)
	assertPuts("cross form", []string{"sel=n1"})

	// 首次启动（last_form 空）：零 PUT
	home = seedFormSession(t, "", false, selected)
	replayIfFormChanged(context.Background(), home, client, formNameUser)
	assertPuts("first start", nil)

	// 读不出（state.json 损坏）：零 PUT
	home = seedFormSession(t, formNameTun, true, selected)
	if err := os.WriteFile(home.StateFile(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	replayIfFormChanged(context.Background(), home, client, formNameUser)
	assertPuts("unreadable state", nil)

	// 无 current 订阅：零 PUT
	home = seedFormSession(t, formNameTun, true, selected)
	if err := os.Remove(home.IndexFile()); err != nil {
		t.Fatal(err)
	}
	replayIfFormChanged(context.Background(), home, client, formNameUser)
	assertPuts("no current profile", nil)

	// selected 为空：零 PUT
	home = seedFormSession(t, formNameTun, true, nil)
	replayIfFormChanged(context.Background(), home, client, formNameUser)
	assertPuts("empty selected", nil)
}

// TestPostHandshakeWritesLastFormAndMarkStoppedKeepsIt postHandshake 写 last_form
// （两种形态各一例）；markStopped 只重建 core，LastForm 必须保留。
func TestPostHandshakeWritesLastFormAndMarkStoppedKeepsIt(t *testing.T) {
	stubBrokerSockPath(t) // postHandshake 的版本读取按 settings 拨路径：TUN 形态是代理入口，绝不触真锚
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)

	settings := store.DefaultSettings()
	settings.TunEnabled = true
	if _, err := postHandshake(context.Background(), home, nil, 4242, StartOptions{ApplySysProxy: false}, settings, ""); err != nil {
		t.Fatalf("postHandshake (tun): %v", err)
	}
	st, err := store.LoadState(home)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastForm != formNameTun || !st.Core.Tun {
		t.Fatalf("state after tun postHandshake = %+v, want last_form=tun + Core.Tun", st)
	}
	if err := markStopped(home); err != nil {
		t.Fatalf("markStopped: %v", err)
	}
	st, _ = store.LoadState(home)
	if st.LastForm != formNameTun || st.Core.Desired != store.DesiredStopped {
		t.Fatalf("state after markStopped = %+v, want last_form kept + core stopped", st)
	}

	settings = store.DefaultSettings() // TunEnabled=false：用户态
	if _, err := postHandshake(context.Background(), home, nil, 0, StartOptions{ApplySysProxy: false}, settings, ""); err != nil {
		t.Fatalf("postHandshake (user): %v", err)
	}
	st, _ = store.LoadState(home)
	if st.LastForm != formNameUser || st.Core.Tun {
		t.Fatalf("state after user postHandshake = %+v, want last_form=user + Core.Tun cleared", st)
	}
}
