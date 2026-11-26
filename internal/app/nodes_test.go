package app

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// stubSocketDir 返回短路径临时目录（macOS unix socket sun_path 上限 104 字节）。
func stubSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "twap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newStubCore 在 unix socket 上起桩内核 API，返回 (sock 路径, 清理函数)。
func newStubCore(t *testing.T, handler http.Handler) (paths.Home, func()) {
	t.Helper()
	dir := stubSocketDir(t)
	home := paths.Home(dir)
	sock := filepath.Join(dir, "core.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck // 测试桩，Close 时返回 ErrServerClosed
	// secret 文件就位（coreClient 只读获取）
	if err := os.WriteFile(home.SecretFile(), []byte("stub-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, func() { srv.Close(); ln.Close() }
}

// seedIndex 写入一个含 current 订阅的索引（可带初始 selected）。
func seedIndex(t *testing.T, home paths.Home, selected []store.ProxySelection) store.Index {
	t.Helper()
	idx := store.Index{Current: "old01", Items: []store.ProfileItem{{
		UID: "old01", Type: store.ProfileTypeFile, Name: "old", File: "old01.yaml", Selected: selected,
	}}}
	if err := store.SaveIndex(home, idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestUpsertSelection(t *testing.T) {
	sels := upsertSelection(nil, "G1", "n1", false)
	sels = upsertSelection(sels, "G2", "n2", true)
	sels = upsertSelection(sels, "G1", "n9", true)
	if len(sels) != 2 || sels[0].Group != "G1" || sels[0].Node != "n9" {
		t.Fatalf("upsert result = %+v, want G1 updated in place", sels)
	}
	if !sels[0].Pinned || !sels[1].Pinned {
		t.Fatalf("pinned flags = %+v, want carried into the records", sels)
	}
}

// TestSelectionsFromRecordsPinnedAndTrailingGlobal 快照记 pinned + GLOBAL 追加在末尾
// （批⑥b §4.8）：Selector 记 now、fixed 非空的 URLTest/Fallback 记 pinned=true、
// 未固定的自动组无标记；其余组的排序不变；对得到的新列表，resolveDelayGroup 的
// 取组结果与改动前相同（首条，GLOBAL 在末尾不抢首条）。
func TestSelectionsFromRecordsPinnedAndTrailingGlobal(t *testing.T) {
	proxies := map[string]core.Proxy{
		"GLOBAL": {Name: "GLOBAL", Type: "Selector", Now: "组A"}, // 聚合组：只记在末尾
		"B组":     {Name: "B组", Type: "Selector", Now: "node-b"},
		"A组":     {Name: "A组", Type: "URLTest", Now: "", Fixed: "node-a"}, // now 空：未选择不记录
		"C组":     {Name: "C组", Type: "URLTest", Now: "node-c", Fixed: "node-c"},
		"D组":     {Name: "D组", Type: "Fallback", Now: "node-d", Fixed: "node-d"},
		"E组":     {Name: "E组", Type: "URLTest", Now: "node-e"}, // 未固定自动组：无标记
		"node-b": {Name: "node-b", Type: "Socks5"},
	}
	sels := selectionsFrom(proxies)
	want := []store.ProxySelection{
		{Group: "B组", Node: "node-b"},
		{Group: "C组", Node: "node-c", Pinned: true},
		{Group: "D组", Node: "node-d", Pinned: true},
		{Group: "E组", Node: "node-e"},
		{Group: "GLOBAL", Node: "组A"},
	}
	if len(sels) != len(want) {
		t.Fatalf("selections = %+v, want %+v", sels, want)
	}
	for i := range want {
		if sels[i] != want[i] {
			t.Fatalf("selections[%d] = %+v, want %+v (full: %+v)", i, sels[i], want[i], sels)
		}
	}
	// 新列表上 resolveDelayGroup 取首条的语义不变
	home := paths.Home(stubSocketDir(t))
	seedIndex(t, home, sels)
	g, err := resolveDelayGroup(home, proxies, "")
	if err != nil || g != "B组" {
		t.Fatalf("delay group = %q err %v, want first entry 组名 B组", g, err)
	}
}

func TestSelectRecordsOnSuccessNotOnFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		// Select 的补读（批⑥b §4.8）：组是 Selector → pinned=false
		w.Write([]byte(`{"proxies":{"🚀 组":{"name":"🚀 组","type":"Selector","now":"节点-B"}}}`))
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			http.Error(w, "proxy not found", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedIndex(t, home, nil)

	if err := Select(context.Background(), home, "🚀 组", "节点-B"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := idx.CurrentItem()
	if len(item.Selected) != 1 || item.Selected[0].Group != "🚀 组" || item.Selected[0].Node != "节点-B" {
		t.Fatalf("selected record = %+v, want emoji group recorded", item.Selected)
	}

	if err := Select(context.Background(), home, "bad", "n"); err == nil {
		t.Fatal("kernel failure: want error")
	}
	idx, _ = store.LoadIndex(home)
	item, _ = idx.CurrentItem()
	if len(item.Selected) != 1 {
		t.Fatalf("kernel failure must not write profiles; got %+v", item.Selected)
	}
}

// TestUnpinSendsDeleteAndValidatesArgs 锁定 v0.4.16 unpin 链路：DELETE /proxies/{group}
// 且组名 PathEscape（emoji 组名）；空组名显式报错。本用例 home 无 profiles.yaml
// （无 current 订阅），回写 pinned 的分支无事可做（批⑥b §4.8 的回写用例见
// TestUnpinClearsPinned）。
func TestUnpinSendsDeleteAndValidatesArgs(t *testing.T) {
	var gotMethod, gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()

	if err := Unpin(context.Background(), home, "♻️ Auto"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", gotMethod)
	}
	// mux 侧 r.URL.Path 已解码：emoji 组名完整到达且为单段（client 侧 PathEscape
	// 保证传输层编码；EscapedPath 校验见 core/api_test）
	if gotPath != "/proxies/♻️ Auto" {
		t.Fatalf("path = %q, want decoded emoji group path", gotPath)
	}
	if err := Unpin(context.Background(), home, ""); err == nil {
		t.Fatal("empty group: want error")
	}
}

func TestSelectWithoutCurrentProfile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	// 无 profiles.yaml：不应报错，也不应崩
	if err := Select(context.Background(), home, "g", "n"); err != nil {
		t.Fatalf("Select without current: %v", err)
	}
}

func TestSnapshotSelectedSkipWhenUnreachable(t *testing.T) {
	// sock 不存在：必须跳过（nil error）且不改 profiles（M1-plan §8 竞态语义）
	home := paths.Home(stubSocketDir(t))
	seedIndex(t, home, nil)
	idx, _ := store.LoadIndex(home)
	item, _ := idx.CurrentItem()
	if err := snapshotSelected(context.Background(), home, &idx, item); err != nil {
		t.Fatalf("unreachable snapshot: %v", err)
	}
	idx2, _ := store.LoadIndex(home)
	if idx2.Items[0].Selected != nil {
		t.Fatalf("unreachable snapshot must not write; got %+v", idx2.Items[0].Selected)
	}
}

func TestSnapshotSelectedWritesGroups(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"proxies":{
			"组B":{"name":"组B","type":"Selector","now":"HK-1"},
			"组A":{"name":"组A","type":"URLTest","now":"JP-1"},
			"GLOBAL":{"name":"GLOBAL","type":"Selector","now":"组A"},
			"HK-1":{"name":"HK-1","type":"Socks5","now":""}
		}}`))
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedIndex(t, home, []store.ProxySelection{{Group: "stale", Node: "old"}})

	idx, _ := store.LoadIndex(home)
	item, _ := idx.CurrentItem()
	if err := snapshotSelected(context.Background(), home, &idx, item); err != nil {
		t.Fatalf("snapshotSelected: %v", err)
	}
	idx2, _ := store.LoadIndex(home)
	got := idx2.Items[0].Selected
	// GLOBAL 存在且 now 非空：批⑥b §4.8 起追加在末尾（组A 是无 fixed 的 URLTest、组B 是
	// Selector——都不带 pinned）
	if len(got) != 3 || got[0].Group != "组A" || got[0].Node != "JP-1" || got[0].Pinned ||
		got[1].Group != "组B" || got[1].Pinned ||
		got[2].Group != "GLOBAL" || got[2].Node != "组A" || got[2].Pinned {
		t.Fatalf("snapshot record = %+v, want 组A/组B sorted + GLOBAL trailing, none pinned, no stale", got)
	}
}

func TestResolveDelayGroup(t *testing.T) {
	proxies := map[string]core.Proxy{
		"组A": {Name: "组A", Type: "Selector", All: []string{"n1"}},
		"组B": {Name: "组B", Type: "Selector", All: []string{"n2"}},
	}
	home := paths.Home(stubSocketDir(t))

	// 无记录：回退字典序第一个策略组
	g, err := resolveDelayGroup(home, proxies, "")
	if err != nil || g != "组A" {
		t.Fatalf("fallback group = %q err %v, want 组A", g, err)
	}
	// selected 首条记录优先
	seedIndex(t, home, []store.ProxySelection{{Group: "组B", Node: "n2"}})
	g, err = resolveDelayGroup(home, proxies, "")
	if err != nil || g != "组B" {
		t.Fatalf("selected-first group = %q err %v, want 组B", g, err)
	}
	// selected 指向不存在的组：回退字典序
	seedIndex(t, home, []store.ProxySelection{{Group: "gone", Node: "x"}})
	g, err = resolveDelayGroup(home, proxies, "")
	if err != nil || g != "组A" {
		t.Fatalf("stale selected fallback = %q err %v, want 组A", g, err)
	}
	// 显式指定但不存在：报错
	if _, err := resolveDelayGroup(home, proxies, "nope"); err == nil {
		t.Fatal("unknown explicit group: want error")
	}
}

func TestAddSnapshotsOldSubscriptionBeforeSwitch(t *testing.T) {
	// 触发三：add 切换 current 前，旧订阅（仍在内核运行）获得快照回写
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"proxies":{"组A":{"name":"组A","type":"Selector","now":"HK-1"}}}`))
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		// add 尾部的 ReloadIfRunning 会热重载，桩内核接受即可
		w.WriteHeader(http.StatusNoContent)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedIndex(t, home, nil)
	if err := store.SaveProfileFile(home, "old01.yaml", []byte("proxies:\n  - name: n\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n")); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(stubSocketDir(t), "new.yaml")
	if err := os.WriteFile(sub, []byte("proxies:\n  - name: m\n    type: socks5\n    server: 127.0.0.1\n    port: 1081\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Add(context.Background(), home, AddSource{FilePath: sub, Name: "new"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Current != idx.Items[len(idx.Items)-1].UID {
		t.Fatalf("current = %q, want switched to new item", idx.Current)
	}
	old := idx.Items[0]
	if len(old.Selected) != 1 || old.Selected[0].Group != "组A" || old.Selected[0].Node != "HK-1" {
		t.Fatalf("old subscription selected = %+v, want snapshot before switch", old.Selected)
	}
}
