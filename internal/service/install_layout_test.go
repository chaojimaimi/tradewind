package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// prepareLayout（批⑥b §4.1）：三个目录权限位、两份日志预创建、重装幂等（fd 收敛）、
// 符号链接/O_NOFOLLOW 打开失败一律报错且不回落到按路径操作。

// assertAnchorLayout 断言锚目录布局的权限位与属主。
func assertAnchorLayout(t *testing.T, p installPlan) {
	t.Helper()
	for path, want := range map[string]os.FileMode{
		p.configDir:  0o755,
		p.logsDir():  0o755,
		p.coreHome(): 0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s must be a directory", path)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s perm = %o, want %o", path, info.Mode().Perm(), want)
		}
		if uid, _ := fileOwner(t, path); uid != p.owner.uid {
			t.Fatalf("%s owner uid = %d, want %d", path, uid, p.owner.uid)
		}
	}
}

// fileOwner 返回路径的属主 uid（测试断言用）。
func fileOwner(t *testing.T, path string) (int, os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("cannot determine owner of %s", path)
	}
	return int(sys.Uid), info.Mode().Perm()
}

// assertLogsPrecreated 断言两份日志存在、0600、属主 dataUID。
func assertLogsPrecreated(t *testing.T, p installPlan) {
	t.Helper()
	for _, name := range []string{ServiceLogName, CoreLogName} {
		path := filepath.Join(p.logsDir(), name)
		uid, perm := fileOwner(t, path)
		if perm != 0o600 {
			t.Fatalf("%s perm = %o, want 0600", path, perm)
		}
		if uid != p.dataUID {
			t.Fatalf("%s owner = %d, want data_uid %d", path, uid, p.dataUID)
		}
	}
}

// TestPrepareLayoutCreatesDirsAndLogs 首次安装：锚 0755、logs 0755、service-data
// 0700；两份日志预创建 0600、属主 dataUID（WHY 属主给用户见 ensureLogFile 注释
// ——0640 root:staff 等于全员可读）。
func TestPrepareLayoutCreatesDirsAndLogs(t *testing.T) {
	p := newInstallPlan(t)
	if err := prepareLayout(p); err != nil {
		t.Fatalf("prepareLayout: %v", err)
	}
	assertAnchorLayout(t, p)
	assertLogsPrecreated(t, p)
}

// TestPrepareLayoutReinstallConvergesViaFd 重装（已存在）幂等且经 fd 收敛：把目录
// 权限改宽、日志改成 0644 后再跑一遍——全部收敛回目标值。收敛走 fd（fchown/fchmod
// 不跟随符号链接），没有任何按路径的 chown/chmod。
func TestPrepareLayoutReinstallConvergesViaFd(t *testing.T) {
	p := newInstallPlan(t)
	if err := prepareLayout(p); err != nil {
		t.Fatalf("first prepareLayout: %v", err)
	}
	// 模拟漂移：目录改宽、日志放宽
	for path, perm := range map[string]os.FileMode{
		p.configDir:  0o777,
		p.logsDir():  0o777,
		p.coreHome(): 0o777,
	} {
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{ServiceLogName, CoreLogName} {
		if err := os.Chmod(filepath.Join(p.logsDir(), name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareLayout(p); err != nil {
		t.Fatalf("reinstall prepareLayout: %v", err)
	}
	assertAnchorLayout(t, p)
	assertLogsPrecreated(t, p)
}

// TestInstallDaemonRunsPrepareLayoutBeforeAnchor installDaemon 的序：prepareLayout
// 失败（logs 是符号链接 → 不删除、不跟随）→ service.json 未写、launchd 未碰。
func TestInstallDaemonRunsPrepareLayoutBeforeAnchor(t *testing.T) {
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	// 预置 logs 为指向外部的符号链接——绝不删除、绝不跟随
	outside := filepath.Join(shortTempDir(t), "outside-logs")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	lc := &fakeLaunchctl{}
	// prepareLayout 的 MkdirAll 分支要求 logs 不存在；这里 logs 已存在（符号链接）
	// → Lstat 拒绝。configDir 由 ensureOwnedDir 处理（不存在 → 创建）
	if err := os.MkdirAll(p.configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p.logsDir()); err != nil {
		t.Fatal(err)
	}
	if err := installDaemon(p, lc, &strings.Builder{}); err == nil {
		t.Fatal("install must refuse a symlinked logs dir")
	} else if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want symlink refusal", err)
	}
	if _, err := os.Lstat(p.logsDir()); err != nil {
		t.Fatalf("the symlink must not be removed: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the symlink target must not be touched, found %v", entries)
	}
	if _, err := os.Stat(filepath.Join(p.configDir, "service.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service.json must not be written on layout failure (err = %v)", err)
	}
	if len(lc.calls) != 0 {
		t.Fatalf("launchd must not be touched on layout failure: %v", lc.calls)
	}
}

// TestPrepareLayoutRefusesSymlinkedLogFile O_NOFOLLOW 打开失败的缝：已存在的
// service.log 是符号链接 → O_CREATE|O_EXCL 失败（EEXIST）→ O_NOFOLLOW 打开失败
// （ELOOP）→ 报错且绝不回落到按路径操作（链接目标不被 chown/chmod、不被创建）。
func TestPrepareLayoutRefusesSymlinkedLogFile(t *testing.T) {
	p := newInstallPlan(t)
	if err := prepareLayout(p); err != nil {
		t.Fatalf("first prepareLayout: %v", err)
	}
	// 把 service.log 换成指向外部的符号链接
	if err := os.Remove(filepath.Join(p.logsDir(), ServiceLogName)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(shortTempDir(t), "outside.log")
	if err := os.WriteFile(target, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(p.logsDir(), ServiceLogName)); err != nil {
		t.Fatal(err)
	}
	if err := prepareLayout(p); err == nil {
		t.Fatal("prepareLayout must refuse a symlinked log path")
	} else if !strings.Contains(err.Error(), "O_NOFOLLOW") {
		t.Fatalf("err = %v, want an O_NOFOLLOW open failure", err)
	}
	// 不回落：目标文件属主/权限/内容原样（任何按路径 chown/chmod 都会跟随链接）
	uid, perm := fileOwner(t, target)
	if uid != os.Getuid() || perm != 0o600 {
		t.Fatalf("symlink target converged via path ops: uid=%d perm=%o", uid, perm)
	}
	if data, _ := os.ReadFile(target); string(data) != "do not touch" {
		t.Fatalf("symlink target content changed: %q", data)
	}
}

// TestPrepareLayoutRefusesNonDirConfigDir 锚位被普通文件占据 → 报错（不删除）。
func TestPrepareLayoutRefusesNonDirConfigDir(t *testing.T) {
	p := newInstallPlan(t)
	if err := os.MkdirAll(filepath.Dir(p.configDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.configDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareLayout(p); err == nil {
		t.Fatal("prepareLayout must refuse a non-directory config dir")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v, want not-a-directory refusal", err)
	}
	if data, _ := os.ReadFile(p.configDir); string(data) != "x" {
		t.Fatalf("the occupying file must not be removed: %q", data)
	}
}
