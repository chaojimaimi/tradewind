package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// purge（批⑥b §4.1）经 purgeWith + 临时目录覆盖——绝不调用生产 Purge()（它会动
// /Library 下的真实锚目录，红线②）。

// purgeFixture 组装临时锚目录 + 注入的 lstat（生产 = os.Lstat，临时目录下同义）。
func purgeFixture(t *testing.T) (anchor string) {
	t.Helper()
	anchor = filepath.Join(shortTempDir(t), "Anchor")
	if err := os.MkdirAll(filepath.Join(anchor, "service-data", "proxies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(anchor, "service-data", "cache.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(anchor, "service.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return anchor
}

// TestPurgeRemovesCoreHomeKeepsRestOfAnchor service-data 是普通目录 → 整目录删除；
// 锚目录里的其它文件原样保留。
func TestPurgeRemovesCoreHomeKeepsRestOfAnchor(t *testing.T) {
	anchor := purgeFixture(t)
	if err := purgeWith(anchor, os.Lstat, &strings.Builder{}); err != nil {
		t.Fatalf("purgeWith: %v", err)
	}
	if _, err := os.Stat(filepath.Join(anchor, "service-data")); !os.IsNotExist(err) {
		t.Fatalf("service-data must be gone (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(anchor, "service.json")); err != nil {
		t.Fatalf("anchor files must be preserved: %v", err)
	}
}

// TestPurgeNoOpWhenAbsent service-data 不存在 → no-op（卸载脚本对未安装候选逐个
// 调用，必须幂等）。
func TestPurgeNoOpWhenAbsent(t *testing.T) {
	anchor := purgeFixture(t)
	if err := os.RemoveAll(filepath.Join(anchor, "service-data")); err != nil {
		t.Fatal(err)
	}
	if err := purgeWith(anchor, os.Lstat, &strings.Builder{}); err != nil {
		t.Fatalf("purgeWith(absent): %v", err)
	}
}

// TestPurgeRemovesSymlinkOnly service-data 是符号链接 → 只删链接本身，目标不动。
func TestPurgeRemovesSymlinkOnly(t *testing.T) {
	anchor := purgeFixture(t)
	target := filepath.Join(shortTempDir(t), "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "precious.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(anchor, "service-data")
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := purgeWith(anchor, os.Lstat, &strings.Builder{}); err != nil {
		t.Fatalf("purgeWith(symlink): %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the link itself must be removed (err = %v)", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "precious.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target must stay untouched (data=%q err=%v)", data, err)
	}
}

// TestPurgeRefusesSymlinkedAnchor 锚目录是符号链接 → 拒绝且链接与目标都不动
// （"service-data 位于锚内"是编译期常量的词法事实，锚若是链接这个判断恒真）。
func TestPurgeRefusesSymlinkedAnchor(t *testing.T) {
	base := shortTempDir(t)
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "service-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "service-data", "cache.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "Anchor")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if err := purgeWith(link, os.Lstat, &strings.Builder{}); err == nil {
		t.Fatal("purge must refuse a symlinked anchor")
	} else if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
	// 链接与目标都不动
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the anchor link must stay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, "service-data", "cache.db")); err != nil {
		t.Fatalf("the real dir must stay: %v", err)
	}
}

// TestPurgeRefusesNonDirAnchor 锚是普通文件 → 拒绝（环境异常，不动任何东西）。
func TestPurgeRefusesNonDirAnchor(t *testing.T) {
	base := shortTempDir(t)
	file := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := purgeWith(file, os.Lstat, &strings.Builder{}); err == nil {
		t.Fatal("purge must refuse a non-directory anchor")
	}
}

// TestPurgeProductionRefusesNonRoot 生产入口 Purge() 的 root 守卫：非 root 直接
// 报错——守卫先于 purgeWith，本用例因此绝不会碰到 /Library 下的真实锚目录。
func TestPurgeProductionRefusesNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the production root guard is only observable as a non-root user")
	}
	err := Purge(&strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("err = %v, want the non-root refusal", err)
	}
}
