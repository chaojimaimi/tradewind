package app

// nodes_pinned_test.go：pinned 标记的落盘用例（批⑥b §4.8）——Select 对自动组补读
// 内核的 fixed 并把 pinned 随节点一起写进记录、Unpin 成功后清该组的 pinned。

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// TestSelectRecordsPinnedForAutoGroups Select 的补读落盘（批⑥b §4.8）：fixed 非空的
// URLTest 组 → 记录 pinned=true；Selector 组 → false；补读失败（GET /proxies 500）→
// pinned 沿用旧记录的值且恰一行 stderr warning、命令仍成功（选择已经生效）。
func TestSelectRecordsPinnedForAutoGroups(t *testing.T) {
	readWarnings := captureReplayWarnings(t)
	getOK := true
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		if !getOK {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"proxies":{
			"auto":{"name":"auto","type":"URLTest","now":"节点X","fixed":"节点X"},
			"sel":{"name":"sel","type":"Selector","now":"节点S"}
		}}`))
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedIndex(t, home, nil)

	if err := Select(context.Background(), home, "auto", "节点X"); err != nil {
		t.Fatalf("Select (fixed url-test): %v", err)
	}
	if err := Select(context.Background(), home, "sel", "节点S"); err != nil {
		t.Fatalf("Select (selector): %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := idx.CurrentItem()
	if len(item.Selected) != 2 || !item.Selected[0].Pinned || item.Selected[0].Group != "auto" {
		t.Fatalf("selected = %+v, want auto pinned=true first", item.Selected)
	}
	if item.Selected[1].Pinned {
		t.Fatalf("selector record must not be pinned: %+v", item.Selected[1])
	}
	if readWarnings() != "" {
		t.Fatalf("successful follow-up reads must not warn: %q", readWarnings())
	}

	// 补读失败：GET /proxies 500 → pinned 沿用旧记录（auto 原为 true），恰一行 warning
	getOK = false
	if err := Select(context.Background(), home, "auto", "节点Y"); err != nil {
		t.Fatalf("Select (follow-up read failed): %v", err)
	}
	idx, _ = store.LoadIndex(home)
	item, _ = idx.CurrentItem()
	last := item.Selected[0]
	if last.Node != "节点Y" || !last.Pinned {
		t.Fatalf("record after failed follow-up = %+v, want node updated + pinned carried over", last)
	}
	if got := readWarnings(); strings.Count(got, "warning:") != 1 || !strings.Contains(got, "pinned 沿用旧记录") {
		t.Fatalf("warnings = %q, want exactly one fallback warning line", got)
	}
}

// TestUnpinClearsPinned unpin 清 pinned 落盘（批⑥b §4.8）：记录 pinned=true → unpin
// 成功后 false（同订阅其它组不动）；无 current 订阅 → nil；保存失败 → 返回错误
// （不清记录的下一次跨形态重放会把已解除的固定重新钉上）。
func TestUnpinClearsPinned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// A：清固定落盘
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedIndex(t, home, []store.ProxySelection{
		{Group: "auto", Node: "n", Pinned: true},
		{Group: "other", Node: "m", Pinned: true},
	})
	if err := Unpin(context.Background(), home, "auto"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := idx.CurrentItem()
	if item.Selected[0].Group != "auto" || item.Selected[0].Pinned {
		t.Fatalf("unpinned record = %+v, want pinned cleared", item.Selected[0])
	}
	if item.Selected[1].Group != "other" || !item.Selected[1].Pinned {
		t.Fatalf("sibling record = %+v, want untouched", item.Selected[1])
	}

	// B：无 current 订阅 → 无事可做
	home2, cleanup2 := newStubCore(t, mux)
	defer cleanup2()
	if err := Unpin(context.Background(), home2, "auto"); err != nil {
		t.Fatalf("Unpin without current profile: %v", err)
	}

	// C：保存失败 → 显式错误
	home3, cleanup3 := newStubCore(t, mux)
	defer cleanup3()
	seedIndex(t, home3, []store.ProxySelection{{Group: "auto", Node: "n", Pinned: true}})
	if err := os.Chmod(home3.String(), 0o555); err != nil {
		t.Fatal(err)
	}
	err = Unpin(context.Background(), home3, "auto")
	if chmodErr := os.Chmod(home3.String(), 0o700); chmodErr != nil {
		t.Fatalf("restore dir perms: %v", chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "unpin") {
		t.Fatalf("save failure must surface an unpin error; got %v", err)
	}
}
