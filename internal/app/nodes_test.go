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
	sels := upsertSelection(nil, "G1", "n1")
	sels = upsertSelection(sels, "G2", "n2")
	sels = upsertSelection(sels, "G1", "n9")
	if len(sels) != 2 || sels[0].Group != "G1" || sels[0].Node != "n9" {
		t.Fatalf("upsert result = %+v, want G1 updated in place", sels)
	}
}

func TestSelectionsFromFiltersAndSorts(t *testing.T) {
	proxies := map[string]core.Proxy{
		"GLOBAL": {Name: "GLOBAL", Type: "Selector", Now: "x"}, // 聚合组必须排除
		"B组":     {Name: "B组", Type: "Selector", Now: "node-b"},
		"A组":     {Name: "A组", Type: "URLTest", Now: ""}, // now 空：未选择不记录
		"node-a": {Name: "node-a", Type: "Socks5"},
		"C组":     {Name: "C组", Type: "Fallback", Now: "node-c"},
	}
	sels := selectionsFrom(proxies)
	if len(sels) != 2 || sels[0].Group != "B组" || sels[0].Node != "node-b" {
		t.Fatalf("selections = %+v, want B组/C组 sorted without GLOBAL", sels)
	}
	if sels[1].Group != "C组" {
		t.Fatalf("selections[1] = %+v", sels[1])
	}
}

func TestSelectRecordsOnSuccessNotOnFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
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
	if len(got) != 2 || got[0].Group != "组A" || got[0].Node != "JP-1" || got[1].Group != "组B" {
		t.Fatalf("snapshot record = %+v, want 组A/组B without GLOBAL and without stale", got)
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
