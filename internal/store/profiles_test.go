package store

import (
	"os"
	"regexp"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestIndexRoundTripAndLookup(t *testing.T) {
	home := paths.Home(t.TempDir())
	idx := Index{Items: []ProfileItem{{
		UID: "a1b2c3d4", Type: ProfileTypeRemote, Name: "机场", File: "a1b2c3d4.yaml",
		URL: "https://example.com/sub", Updated: 1763400000,
		Extra: ExtraInfo{Upload: 1, Download: 2, Total: 3, Expire: 4},
	}}}
	idx.Current = "a1b2c3d4"
	if err := SaveIndex(home, idx); err != nil {
		t.Fatalf("SaveIndex: %v", err)
	}
	got, err := LoadIndex(home)
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	if got.Current != "a1b2c3d4" || len(got.Items) != 1 {
		t.Fatalf("index got %+v", got)
	}
	item, ok := got.CurrentItem()
	if !ok || item.Name != "机场" || item.Extra.Total != 3 {
		t.Fatalf("current item got %+v ok=%v", item, ok)
	}
	if _, ok := got.ByUID("missing"); ok {
		t.Fatal("ByUID(missing) should miss")
	}
}

func TestLoadIndexMissingFile(t *testing.T) {
	// 首次导入场景：索引文件不存在返回空索引而非报错
	home := paths.Home(t.TempDir())
	idx, err := LoadIndex(home)
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	if idx.Current != "" || len(idx.Items) != 0 {
		t.Fatalf("empty index got %+v", idx)
	}
}

func TestLoadIndexCorruptFile(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := os.WriteFile(home.IndexFile(), []byte("items: [{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIndex(home); err == nil {
		t.Fatal("corrupt index: want error")
	}
}

func TestSaveProfileFilePerm(t *testing.T) {
	home := paths.Home(t.TempDir())
	if err := SaveProfileFile(home, "a1b2c3d4.yaml", []byte("proxies: []")); err != nil {
		t.Fatalf("SaveProfileFile: %v", err)
	}
	info, err := os.Stat(home.ProfileFile("a1b2c3d4.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile file perm = %o, want 600", info.Mode().Perm())
	}
}

func TestNewUIDFormat(t *testing.T) {
	uid, err := NewUID()
	if err != nil {
		t.Fatalf("NewUID: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(uid) {
		t.Fatalf("uid = %q, want 8 hex chars", uid)
	}
	other, _ := NewUID()
	if other == uid {
		t.Fatalf("uid collision: %s", uid)
	}
}

func TestFileProfileHasNoURL(t *testing.T) {
	// file 型订阅无 url 字段（DESIGN §5.2）
	home := paths.Home(t.TempDir())
	item := ProfileItem{UID: "x", Type: ProfileTypeFile, Name: "local", File: "x.yaml"}
	if err := SaveIndex(home, Index{Items: []ProfileItem{item}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(home.IndexFile())
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`url:`).Match(data) {
		t.Fatalf("file-type item should omit url, got:\n%s", data)
	}
}
