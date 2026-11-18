package app

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// stubFullCore 起一个覆盖 /version、/proxies、/configs（PUT 热重载 + PATCH 模式）的桩内核。
// reloads 记录 /configs 收到的 HTTP method（断言热重载发生与否）。
func stubFullCore(t *testing.T, reloads *[]string) (paths.Home, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"proxies":{"组A":{"name":"组A","type":"Selector","now":"HK-1"},
			"GLOBAL":{"name":"GLOBAL","type":"Selector","now":"组A"}}}`))
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		*reloads = append(*reloads, r.Method)
		w.WriteHeader(http.StatusNoContent)
	})
	return newStubCore(t, mux)
}

// seedTwoProfiles 写入两条订阅（含原文件），current 指向第一条。
func seedTwoProfiles(t *testing.T, home paths.Home) {
	t.Helper()
	profileBody := func(name string, port int) []byte {
		return []byte("proxies:\n  - name: " + name + "\n    type: socks5\n    server: 127.0.0.1\n    port: " +
			itoa(port) + "\n")
	}
	items := []store.ProfileItem{
		{UID: "old01", Type: store.ProfileTypeFile, Name: "first sub", File: "old01.yaml"},
		{UID: "new02", Type: store.ProfileTypeFile, Name: "second sub", File: "new02.yaml"},
	}
	for _, item := range items {
		if err := store.SaveProfileFile(home, item.File, profileBody(item.Name, 1080)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveIndex(home, store.Index{Current: "old01", Items: items}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureSecret(home); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestUseSwitchesCurrentSnapshotsAndReloads(t *testing.T) {
	// M2-plan §6：快照回写、合成热重载、按 name 匹配
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedTwoProfiles(t, home)

	if err := Use(context.Background(), home, "second sub"); err != nil {
		t.Fatalf("Use by name: %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Current != "new02" {
		t.Fatalf("current = %q, want new02", idx.Current)
	}
	old := idx.Items[0]
	if len(old.Selected) != 1 || old.Selected[0].Group != "组A" || old.Selected[0].Node != "HK-1" {
		t.Fatalf("old subscription snapshot = %+v, want 组A/HK-1 recorded before switch", old.Selected)
	}
	// 热重载发生：PUT /configs；且 runtime.yaml 已按新订阅重合成
	if len(reloads) != 1 || reloads[0] != http.MethodPut {
		t.Fatalf("config calls = %v, want exactly one PUT", reloads)
	}
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !strings.Contains(string(data), "second sub") {
		t.Fatalf("runtime.yaml not resynthesized from new profile: %v", err)
	}
}

func TestUseByUIDAndAlreadyCurrent(t *testing.T) {
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedTwoProfiles(t, home)

	// uid 匹配
	if err := Use(context.Background(), home, "new02"); err != nil {
		t.Fatalf("Use by uid: %v", err)
	}
	// 再切一次同名目标：已 current → no-op，无额外热重载
	n := len(reloads)
	if err := Use(context.Background(), home, "new02"); err != nil {
		t.Fatalf("Use(already current): %v", err)
	}
	if len(reloads) != n {
		t.Fatalf("already-current must not hot reload; reloads = %v", reloads)
	}
}

func TestUseAmbiguousNameErrors(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	items, _ := store.LoadIndex(home)
	items.Items = append(items.Items, store.ProfileItem{
		UID: "dup03", Type: store.ProfileTypeFile, Name: "second sub", File: "dup03.yaml",
	})
	if err := store.SaveIndex(home, items); err != nil {
		t.Fatal(err)
	}
	err := Use(context.Background(), home, "second sub")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous name err = %v, want ambiguity error", err)
	}
}

func TestUseUnknownTargetErrors(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	if err := Use(context.Background(), home, "nope"); err == nil {
		t.Fatal("unknown target: want error")
	}
	if err := Use(context.Background(), home, ""); err == nil {
		t.Fatal("empty target: want usage error")
	}
}

func TestListRows(t *testing.T) {
	// 列内容：current 标记 / uid / name / type / updated / 流量 / 到期
	idx := store.Index{Current: "r1", Items: []store.ProfileItem{
		{UID: "r1", Type: store.ProfileTypeRemote, Name: "机场", Updated: 1763400000,
			Extra: store.ExtraInfo{Upload: 1024, Download: 2048, Total: 10240, Expire: 1767225600}},
		{UID: "f2", Type: store.ProfileTypeFile, Name: "本地"},
	}}
	rows := listRows(idx)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	if !strings.Contains(rows[0], "*\tr1\t机场\tremote") {
		t.Fatalf("row0 = %q, want current marker + fields", rows[0])
	}
	if !strings.Contains(rows[0], "3.00 KB / 10.00 KB") || !strings.Contains(rows[0], "2026-01-01") {
		t.Fatalf("row0 traffic/expire = %q", rows[0])
	}
	if strings.Contains(rows[1], "r1") || !strings.HasPrefix(rows[1], " \tf2") {
		t.Fatalf("row1 = %q, want non-current file row", rows[1])
	}
}

func TestListEmptyIndex(t *testing.T) {
	// 空索引不报错（提示先 add）
	if err := List(paths.Home(stubSocketDir(t))); err != nil {
		t.Fatalf("List(empty): %v", err)
	}
}
