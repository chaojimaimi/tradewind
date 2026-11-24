package service

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// safeopen（批⑥b §4.1，I1）的负路径矩阵：root 进程读用户目录文件的唯一通道，
// 符号链接（含悬空）/目录/FIFO/属主不符/超限一律拒绝；普通文件返回完整字节。

func TestOpenSafeReadsRegularFileOfRightOwner(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "runtime.yaml")
	content := "proxies: []\nlog-level: info\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openSafe(path, os.Getuid())
	if err != nil {
		t.Fatalf("openSafe(regular): %v", err)
	}
	defer f.Close()
	if !mustRegular(t, f) {
		t.Fatal("fd must point at a regular file")
	}
}

func mustRegular(t *testing.T, f *os.File) bool {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.Mode().IsRegular()
}

// 符号链接拒绝（指向真实文件）：跟随读到 root 专读文件后，YAML 语法错误信息会带
// 出内容片段——O_NOFOLLOW 在 open 处拒绝。
func TestOpenSafeRejectsSymlink(t *testing.T) {
	dir := shortTempDir(t)
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "runtime.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openSafe(link, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("openSafe must refuse a symbolic link")
	} else if !strings.Contains(err.Error(), "is a symbolic link (refusing to follow)") {
		t.Fatalf("err = %v, want an explicit symlink refusal", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "SECRET" {
		t.Fatalf("target must stay untouched, got %q", data)
	}
}

// 悬空链接同样拒绝（ELOOP 形态一致），且不得透过链接把目标建出来。
func TestOpenSafeRejectsDanglingSymlink(t *testing.T) {
	dir := shortTempDir(t)
	link := filepath.Join(dir, "runtime.yaml")
	missing := filepath.Join(dir, "missing")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openSafe(link, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("openSafe must refuse a dangling symbolic link")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("target must not be created through the link (err = %v)", err)
	}
}

// 目录拒绝：openSafe 只服务"读白名单文件"一种用途。
func TestOpenSafeRejectsDirectory(t *testing.T) {
	dir := shortTempDir(t)
	if f, err := openSafe(dir, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("openSafe must refuse a directory")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a not-regular-file refusal", err)
	}
}

// FIFO 拒绝：O_RDONLY 打开 FIFO 会阻塞到有写者——O_NONBLOCK 让打开立即返回，随后
// fstat 的"必须普通文件"拦下（没有 O_NONBLOCK 本用例会挂死）。
func TestOpenSafeRejectsFifo(t *testing.T) {
	dir := shortTempDir(t)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if f, err := openSafe(fifo, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("openSafe must refuse a FIFO")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a not-regular-file refusal", err)
	}
}

// 属主不符拒绝（期望 uid 传别的值）：同时挡住指向 root 文件的硬链接。
func TestOpenSafeRejectsForeignOwner(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(path, []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := openSafe(path, os.Getuid()+1); err == nil {
		f.Close()
		t.Fatal("openSafe must refuse a file owned by another uid")
	} else if !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("err = %v, want an owner mismatch refusal", err)
	}
}

// 超限拒绝：字节上限是 root 进程内存占用面的粗闸。
func TestReadSafeRejectsOversized(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSafe(path, os.Getuid(), 1024); err == nil {
		t.Fatal("readSafe must refuse a file over the byte limit")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want a limit refusal", err)
	}
}

// 正常文件返回完整字节；恰好等于上限的文件通过。
func TestReadSafeReturnsFullContent(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "runtime.yaml")
	content := "proxies: []\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSafe(path, os.Getuid(), int64(len(content)))
	if err != nil {
		t.Fatalf("readSafe: %v", err)
	}
	if string(got) != content {
		t.Fatalf("readSafe = %q, want %q", got, content)
	}
	// 边界：size == maxBytes 通过，size == maxBytes+1 拒绝
	if _, err := readSafe(path, os.Getuid(), int64(len(content))-1); err == nil {
		t.Fatal("readSafe must refuse size == maxBytes+1")
	}
}
