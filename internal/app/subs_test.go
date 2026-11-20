package app

import (
	"context"
	"encoding/json"
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
	if err := List(paths.Home(stubSocketDir(t)), false); err != nil {
		t.Fatalf("List(empty): %v", err)
	}
}

// --- subs delete / subs interval（M3-plan §11.2-5，评审 P1-2 语义）---

func TestSubsDeleteNonCurrent(t *testing.T) {
	// 删非 current：索引移除条目 + 原文件清理，current 与其余条目不动、无热重载
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedTwoProfiles(t, home)

	if err := SubsDelete(context.Background(), home, "second sub", false); err != nil {
		t.Fatalf("SubsDelete: %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Current != "old01" || len(idx.Items) != 1 || idx.Items[0].UID != "old01" {
		t.Fatalf("index after delete = %+v, want only old01 with current intact", idx)
	}
	if _, err := os.Stat(home.ProfileFile("new02.yaml")); !os.IsNotExist(err) {
		t.Fatal("profile file must be removed")
	}
	if _, err := os.Stat(home.ProfileFile("old01.yaml")); err != nil {
		t.Fatalf("other profile file must stay: %v", err)
	}
	if len(reloads) != 0 {
		t.Fatalf("non-current delete must not reload; reloads = %v", reloads)
	}
}

func TestSubsDeleteCurrentWithoutForceRejected(t *testing.T) {
	// current 保护：非 --force 删当前订阅显式报错，索引与文件均不动
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	err := SubsDelete(context.Background(), home, "old01", false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want current-protection error", err)
	}
	idx, _ := store.LoadIndex(home)
	if len(idx.Items) != 2 || idx.Current != "old01" {
		t.Fatalf("index must be untouched; got %+v", idx)
	}
	if _, err := os.Stat(home.ProfileFile("old01.yaml")); err != nil {
		t.Fatalf("current profile file must stay: %v", err)
	}
}

func TestSubsDeleteCurrentForceBareCore(t *testing.T) {
	// 删 current（--force）：Current 置空 → 纯模板裸内核重合成 + 热重载 + 原文件清理
	var reloads []string
	home, cleanup := stubFullCore(t, &reloads)
	defer cleanup()
	seedTwoProfiles(t, home)

	if err := SubsDelete(context.Background(), home, "old01", true); err != nil {
		t.Fatalf("SubsDelete(--force): %v", err)
	}
	idx, _ := store.LoadIndex(home)
	if idx.Current != "" || len(idx.Items) != 1 {
		t.Fatalf("index after force delete = %+v, want current empty with new02 left", idx)
	}
	if _, err := os.Stat(home.ProfileFile("old01.yaml")); !os.IsNotExist(err) {
		t.Fatal("deleted current profile file must be removed")
	}
	if len(reloads) != 1 || reloads[0] != http.MethodPut {
		t.Fatalf("reloads = %v, want bare-core hot reload", reloads)
	}
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil || strings.Contains(string(data), "proxies:") {
		t.Fatalf("runtime.yaml must be bare template after current delete:\n%s", data)
	}
}

func TestSubsIntervalOff(t *testing.T) {
	// 评审 P1-2 语义：off → AllowAutoUpdate=false（真实关闭开关，非 interval=0）
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	if err := SubsInterval(context.Background(), home, "new02", "off"); err != nil {
		t.Fatalf("SubsInterval(off): %v", err)
	}
	idx, _ := store.LoadIndex(home)
	item, _ := idx.ByUID("new02")
	if item.AllowAutoUpdate == nil || *item.AllowAutoUpdate {
		t.Fatalf("AllowAutoUpdate = %+v, want explicit false", item.AllowAutoUpdate)
	}
	// 其余条目不受影响
	other, _ := idx.ByUID("old01")
	if other.AllowAutoUpdate != nil {
		t.Fatalf("other item AllowAutoUpdate = %+v, want untouched", other.AllowAutoUpdate)
	}
}

func TestSubsIntervalValidHours(t *testing.T) {
	// 1/12/24 → UpdateInterval=3600/43200/86400 且 AllowAutoUpdate=true
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	for mode, want := range map[string]int{"1": 3600, "12": 43200, "24": 86400} {
		if err := SubsInterval(context.Background(), home, "old01", mode); err != nil {
			t.Fatalf("SubsInterval(%s): %v", mode, err)
		}
		idx, _ := store.LoadIndex(home)
		item, _ := idx.ByUID("old01")
		if item.UpdateInterval != want {
			t.Fatalf("UpdateInterval = %d, want %d", item.UpdateInterval, want)
		}
		if item.AllowAutoUpdate == nil || !*item.AllowAutoUpdate {
			t.Fatalf("AllowAutoUpdate = %+v, want explicit true", item.AllowAutoUpdate)
		}
	}
}

func TestSubsIntervalInvalid(t *testing.T) {
	// 非法档位 / 空目标显式报错，profiles 不动
	home := paths.Home(stubSocketDir(t))
	seedTwoProfiles(t, home)
	for _, mode := range []string{"30", "abc", "0", ""} {
		if err := SubsInterval(context.Background(), home, "old01", mode); err == nil {
			t.Fatalf("interval %q: want error", mode)
		}
	}
	if err := SubsInterval(context.Background(), home, "nope", "12"); err == nil {
		t.Fatal("unknown target: want error")
	}
	if err := SubsInterval(context.Background(), home, "", "12"); err == nil {
		t.Fatal("empty target: want usage error")
	}
	idx, _ := store.LoadIndex(home)
	item, _ := idx.ByUID("old01")
	if item.UpdateInterval != 0 || item.AllowAutoUpdate != nil {
		t.Fatalf("invalid calls must not touch profiles; got %+v", item)
	}
}

// --- list --json / autostart status --json（壳机器可读输出，M3-plan §11.2-5）---

func TestListJSONDecodable(t *testing.T) {
	// 字段契约：current 标记 / uid / name / type / updated / extra / allow_auto_update / update_interval
	off := false
	idx := store.Index{Current: "r1", Items: []store.ProfileItem{
		{UID: "r1", Type: store.ProfileTypeRemote, Name: "机场", Updated: 1763400000,
			AllowAutoUpdate: &off, UpdateInterval: 43200,
			Extra: store.ExtraInfo{Upload: 1024, Download: 2048, Total: 10240, Expire: 1767225600}},
		{UID: "f2", Type: store.ProfileTypeFile, Name: "本地"},
	}}
	line, err := MarshalListJSON(listReport(idx))
	if err != nil {
		t.Fatal(err)
	}
	var got []ProfileJSON
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("list --json not decodable: %v (%s)", err, line)
	}
	if len(got) != 2 || !got[0].Current || got[1].Current {
		t.Fatalf("current markers wrong: %+v", got)
	}
	if got[0].UID != "r1" || got[0].Name != "机场" || got[0].Type != "remote" || got[0].Updated != 1763400000 {
		t.Fatalf("item0 fields = %+v", got[0])
	}
	if got[0].Extra.Total != 10240 || got[0].Extra.Expire != 1767225600 {
		t.Fatalf("item0 extra = %+v", got[0].Extra)
	}
	// off 态必须可辨（allow_auto_update=false 显式出现，不能被 omitempty 吞掉）
	if !strings.Contains(line, `"allow_auto_update":false`) {
		t.Fatalf("list --json must expose explicit off state: %s", line)
	}
	if got[0].UpdateInterval != 43200 {
		t.Fatalf("update_interval = %d, want 43200", got[0].UpdateInterval)
	}
}

func TestListJSONEmptyIndex(t *testing.T) {
	// 空索引输出 []（壳解码为数组而非 null）
	line, err := MarshalListJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "[]" {
		t.Fatalf("empty list json = %q, want []", line)
	}
}

func TestAutostartStatusJSONDecodable(t *testing.T) {
	// 机器可读开关态（人读文本与 --json 共用 autostartStatusReport 数据源）
	dir := t.TempDir()
	line, err := autostartStatusJSONReport(dir, &fakeLaunchctl{})
	if err != nil {
		t.Fatal(err)
	}
	var st autostartStatusJSON
	if err := json.Unmarshal([]byte(line), &st); err != nil {
		t.Fatalf("autostart --json not decodable: %v (%s)", err, line)
	}
	if st.Installed || st.Loaded {
		t.Fatalf("disabled state = %+v, want installed=false loaded=false", st)
	}
	content, _ := launchdPlist(LaunchAgentLabel, "/x/tradewind", "/x/logs")
	if err := os.WriteFile(plistPathFor(dir), content, 0o644); err != nil {
		t.Fatal(err)
	}
	line, err = autostartStatusJSONReport(dir, &fakeLaunchctl{listLoaded: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(line), &st); err != nil {
		t.Fatalf("enabled state not decodable: %v", err)
	}
	if !st.Installed || !st.Loaded {
		t.Fatalf("enabled state = %+v, want installed=true loaded=true", st)
	}
}
