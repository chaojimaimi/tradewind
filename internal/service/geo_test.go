package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// geo 同步（批⑥b §4.4）：每次 spawn 前把用户目录里的 geo 数据搬进 <core_home>。
// 实现在 rootstate.go；用例单独成文件（单文件 ≤400 行惯例）。

// TestSyncGeoCopiesAndFollowsMtime 源存在 → 拷贝且目标 mtime 跟随源（下次未变化才
// 能跳过）；源变化 → 重新拷贝。
func TestSyncGeoCopiesAndFollowsMtime(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome}
	name := "geoip.metadb"

	if err := os.WriteFile(filepath.Join(dataDir, name), []byte("geo-v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcMod := time.Unix(1700000000, 0)
	if err := os.Chtimes(filepath.Join(dataDir, name), srcMod, srcMod); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)
	target := filepath.Join(coreHome, name)
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "geo-v1" {
		t.Fatalf("target = %q err = %v, want copied content", data, err)
	}
	if info, err := os.Stat(target); err != nil || !info.ModTime().Equal(srcMod) {
		t.Fatalf("target mtime = %v err = %v, want %v", info.ModTime(), err, srcMod)
	}

	// 源变化 → 重新拷贝
	if err := os.WriteFile(filepath.Join(dataDir, name), []byte("geo-v2-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)
	if data, err = os.ReadFile(target); err != nil || string(data) != "geo-v2-longer" {
		t.Fatalf("target after change = %q err = %v, want re-copied", data, err)
	}
}

// TestSyncGeoSkipsUnchanged 未变 → 跳过：目标 (size, mtime) 与源一致时不再重写
// ——把目标改写成同大小不同内容并对齐 mtime，再同步后目标内容保持被改写的样子
// （被覆盖回来 = 判定失误）。
func TestSyncGeoSkipsUnchanged(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome}
	name := "geosite.dat"
	src := filepath.Join(dataDir, name)
	if err := os.WriteFile(src, []byte("AAAAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)

	// 目标改写成同大小不同内容 + mtime 对齐源 → (size, mtime) 判"未变化"
	target := filepath.Join(coreHome, name)
	if err := os.WriteFile(target, []byte("BBBBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	sinfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, sinfo.ModTime(), sinfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)
	if data, err := os.ReadFile(target); err != nil || string(data) != "BBBBBB" {
		t.Fatalf("unchanged target was rewritten: %q err = %v", data, err)
	}
}

// TestSyncGeoReplacesSymlinkTargetWithRegularFile 目标是符号链接 → 被普通文件替换
// （rename 语义天然不跟随）。
func TestSyncGeoReplacesSymlinkTargetWithRegularFile(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome}
	name := "Country.mmdb"
	if err := os.WriteFile(filepath.Join(dataDir, name), []byte("geo"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(coreHome, name)
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("target must be a regular file after sync (mode=%v err=%v)", info.Mode(), err)
	}
	if data, _ := os.ReadFile(target); string(data) != "geo" {
		t.Fatalf("target content = %q, want copied geo", data)
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep" {
		t.Fatalf("symlink target must stay untouched, got %q", data)
	}
}

// TestSyncGeoSkipsSymlinkSource 源是符号链接 → 打一行 skipped 日志（stderr）后
// 跳过，目标原样不动，不阻断（syncGeo 无返回值即"永不阻断 spawn"）。
func TestSyncGeoSkipsSymlinkSource(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome}
	name := "geoip.db"
	outside := filepath.Join(dir, "outside.geo")
	if err := os.WriteFile(outside, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, name)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(coreHome, name)
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncGeo(c) // 不得 panic/阻断；目标不动
	if data, err := os.ReadFile(target); err != nil || string(data) != "old" {
		t.Fatalf("target must stay untouched, got %q err = %v", data, err)
	}
}

// TestSyncGeoSkipsForeignOwnerSource 源属主不符 → 跳过（openSafe 属主闸）。
func TestSyncGeoSkipsForeignOwnerSource(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// DataUID 指向不存在的 uid → openSafe 属主校验必拒（非 root 环境可复现）
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid() + 77, CoreHome: coreHome}
	name := "ASN.mmdb"
	if err := os.WriteFile(filepath.Join(dataDir, name), []byte("geo"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)
	if _, err := os.Stat(filepath.Join(coreHome, name)); !os.IsNotExist(err) {
		t.Fatalf("target must not appear (err = %v)", err)
	}
}

// geoSyncFixture 组装数据目录与内核工作目录，返回配置。
func geoSyncFixture(t *testing.T) (c ConfigFile, dataDir, coreHome string) {
	t.Helper()
	dir := shortTempDir(t)
	dataDir = filepath.Join(dir, "data")
	coreHome = filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome}, dataDir, coreHome
}

// TestSyncGeoRecoversFromStaleTmp <core_home> 里留着上一实例的半成品 .geo-sync.tmp
// （拷贝中途被 kill -9）→ 同步仍然成功：先摘掉残留再 O_EXCL 创建，否则这个文件
// 此后永远同步不过来。
func TestSyncGeoRecoversFromStaleTmp(t *testing.T) {
	c, dataDir, coreHome := geoSyncFixture(t)
	name := "geoip.metadb"
	if err := os.WriteFile(filepath.Join(dataDir, name), []byte("fresh-geo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coreHome, ".geo-sync.tmp"), []byte("half-written"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncGeo(c)
	if data, err := os.ReadFile(filepath.Join(coreHome, name)); err != nil || string(data) != "fresh-geo" {
		t.Fatalf("target = %q err = %v, want the source bytes despite the stale tmp", data, err)
	}
	if _, err := os.Stat(filepath.Join(coreHome, ".geo-sync.tmp")); !os.IsNotExist(err) {
		t.Fatalf("the tmp file must be renamed away (err = %v)", err)
	}
}

// TestCopyStreamRejectsSizeDrift 拷贝期间源与 stat 的大小不一致（变大或变小）→
// 判失败、不产生目标文件。上限因此管住的是整个拷贝过程，而不只是 stat 那一刻
// ——源在 stat 之后被同账号进程追加，不能把超过 geoSyncMaxBytes 的内容搬进
// <core_home>。
func TestCopyStreamRejectsSizeDrift(t *testing.T) {
	dir := shortTempDir(t)
	for _, tc := range []struct {
		name    string
		content string
		size    int64
	}{
		{"grew", "0123456789", 5},
		{"shrank", "abc", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := filepath.Join(dir, tc.name+".tmp")
			target := filepath.Join(dir, tc.name)
			err := copyStream(tmp, target, strings.NewReader(tc.content), tc.size, time.Now())
			if err == nil || !strings.Contains(err.Error(), "source changed during copy") {
				t.Fatalf("err = %v, want the size-drift refusal", err)
			}
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("no target may be produced on drift (err = %v)", statErr)
			}
		})
	}
}
