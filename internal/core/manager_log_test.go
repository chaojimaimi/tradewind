package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 日志路径不跟随符号链接（batch6 真机 fold-in 2）：特权服务以 root 打开用户数据
// 目录里的 core-service.log——该路径被换成指向系统文件的链接时，跟随打开 = root
// 往目标追加日志；tailFile 跟随 = root 专读文件内容进错误信息。以下用例在临时
// 目录里把日志路径做成符号链接，断言"报错而非跟随"；普通文件行为零回归。

// plantLogLink 让 link 指向 target，返回 target 路径。
func plantLogLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestOpenLogFileRefusesSymlink(t *testing.T) {
	dir := shortTempDir(t)
	target := writeTempFile(t, dir, "target.log", "TARGET-CONTENT\n")
	link := filepath.Join(dir, "core.log")
	plantLogLink(t, link, target)

	f, err := openLogFile(link)
	if err == nil {
		f.Close()
		t.Fatal("openLogFile must refuse a symbolic-link log path")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want an explicit symlink refusal", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "TARGET-CONTENT\n" {
		t.Fatalf("link target must stay untouched, got %q", data)
	}
}

// 悬空链接：既不报成功，也不得透过链接把目标建出来。
func TestOpenLogFileDoesNotCreateThroughDanglingSymlink(t *testing.T) {
	dir := shortTempDir(t)
	missing := filepath.Join(dir, "missing.log")
	link := filepath.Join(dir, "core.log")
	plantLogLink(t, link, missing)

	if f, err := openLogFile(link); err == nil {
		f.Close()
		t.Fatal("openLogFile must refuse a dangling symbolic-link log path")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target must not be created through the link (err = %v)", err)
	}
}

// 普通文件零回归：已存在 → 以追加方式打开（不截断）；不存在 → 以 0600 新建。
func TestOpenLogFileRegularFileAppends(t *testing.T) {
	dir := shortTempDir(t)
	existing := writeTempFile(t, dir, "core.log", "line1\n")
	f, err := openLogFile(existing)
	if err != nil {
		t.Fatalf("openLogFile(regular): %v", err)
	}
	if _, err := f.WriteString("line2\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if data, _ := os.ReadFile(existing); string(data) != "line1\nline2\n" {
		t.Fatalf("regular log must be appended, got %q", data)
	}
	fresh := filepath.Join(dir, "fresh.log")
	f, err = openLogFile(fresh)
	if err != nil {
		t.Fatalf("openLogFile(create): %v", err)
	}
	f.Close()
	info, err := os.Stat(fresh)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fresh log: perm = %v err = %v, want 0600", info.Mode().Perm(), err)
	}
}

func TestRotateLogRejectsSymlink(t *testing.T) {
	dir := shortTempDir(t)
	// 目标大于轮转阈值：若 rotateLog 跟随链接读了目标大小，就会动手 rename
	target := writeTempFile(t, dir, "target.log", strings.Repeat("x", logRotateSize+1))
	link := filepath.Join(dir, "core.log")
	plantLogLink(t, link, target)

	if err := rotateLog(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("rotateLog(symlink) = %v, want symlink refusal", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("link target must not be renamed away: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the link itself must be left alone: %v", err)
	}
	if _, err := os.Stat(link + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no rotation artifact may appear (err = %v)", err)
	}
}

// 普通文件零回归：超阈值照常轮转为 .1；未超阈值不动；缺失视为无事。
func TestRotateLogRegularFileStillRotates(t *testing.T) {
	dir := shortTempDir(t)
	big := writeTempFile(t, dir, "core.log", strings.Repeat("y", logRotateSize+1))
	if err := rotateLog(big); err != nil {
		t.Fatalf("rotateLog(big regular): %v", err)
	}
	if _, err := os.Stat(big); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotated log must be renamed (err = %v)", err)
	}
	if info, err := os.Stat(big + ".1"); err != nil || info.Size() != logRotateSize+1 {
		t.Fatalf("rotated copy: err = %v size = %v", err, info)
	}

	small := writeTempFile(t, dir, "small.log", "tiny")
	if err := rotateLog(small); err != nil {
		t.Fatalf("rotateLog(small): %v", err)
	}
	if _, err := os.Stat(small); err != nil {
		t.Fatalf("small log must stay in place: %v", err)
	}
	if err := rotateLog(filepath.Join(dir, "absent.log")); err != nil {
		t.Fatalf("rotateLog(absent) = %v, want nil", err)
	}
}

func TestTailFileRefusesSymlink(t *testing.T) {
	dir := shortTempDir(t)
	const secret = "ROOT-ONLY-CONTENT-MUST-NOT-LEAK\n"
	target := writeTempFile(t, dir, "target.log", secret)
	link := filepath.Join(dir, "core.log")
	plantLogLink(t, link, target)

	got := tailFile(link, 4096)
	if strings.Contains(got, "ROOT-ONLY-CONTENT") {
		t.Fatalf("tailFile followed the symlink and leaked the target: %q", got)
	}
	if !strings.Contains(got, "log unavailable") {
		t.Fatalf("tailFile(symlink) = %q, want the unavailable marker", got)
	}
	// 普通文件仍读得到尾部
	if tail := tailFile(target, 6); tail != "-LEAK\n" {
		t.Fatalf("tailFile(regular) = %q, want the last 6 bytes", tail)
	}
}

// TestSpawnRejectsSymlinkedLog 端到端：日志路径是符号链接 → Spawn 报错（不是
// 跟随）、不拉起子进程、目标内容不变。
func TestSpawnRejectsSymlinkedLog(t *testing.T) {
	dir := shortTempDir(t)
	cfg := stubConfig(t, dir, "normal")
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := writeTempFile(t, dir, "target.log", "TARGET\n")
	plantLogLink(t, cfg.LogPath, target)

	m := NewManager(cfg)
	err := m.Spawn()
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Spawn with symlinked log = %v, want symlink refusal", err)
	}
	if pid := m.PID(); pid != 0 {
		_ = m.Stop()
		t.Fatalf("no core may be spawned behind a symlinked log (pid %d)", pid)
	}
	if data, _ := os.ReadFile(target); string(data) != "TARGET\n" {
		t.Fatalf("link target must stay untouched, got %q", data)
	}
}
