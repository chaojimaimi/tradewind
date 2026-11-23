package service

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rootOwned 是 root 属主的合法形态（mode 按条目给：目录 0755、可执行文件 0755、
// 校验文件 0644）。
func rootOwned(mode os.FileMode) ownerStat { return ownerStat{mode: mode, uid: 0} }

// TestCheckExecDirEntryMatrix 单项判定矩阵（纯函数）：非符号链接 + uid 0 +
// group/other 不可写三者缺一即拒；错误信息带条目路径与不达标原因。
func TestCheckExecDirEntryMatrix(t *testing.T) {
	cases := []struct {
		name    string
		st      ownerStat
		statErr error
		wantErr string // 空 = 必须通过
	}{
		{"root dir 0755", rootOwned(os.ModeDir | 0o755), nil, ""},
		{"root file 0755", rootOwned(0o755), nil, ""},
		{"root file 0644", rootOwned(0o644), nil, ""},
		{"symlink even when root-owned", rootOwned(os.ModeSymlink | 0o777), nil, "symbolic link"},
		{"owned by a user", ownerStat{mode: os.ModeDir | 0o755, uid: 501}, nil, "owned by uid 501"},
		{"group-writable dir", rootOwned(os.ModeDir | 0o775), nil, "group/other-writable"},
		{"other-writable file", rootOwned(0o646), nil, "group/other-writable"},
		{"world-writable", rootOwned(0o777), nil, "group/other-writable"},
		{"missing entry", ownerStat{}, errors.New("no such file or directory"), "no such file or directory"},
	}
	for _, c := range cases {
		err := checkExecDirEntry("/x/entry", c.st, c.statErr)
		if c.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: unexpected rejection: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "/x/entry") {
			t.Fatalf("%s: err = %v, want %q naming the entry", c.name, err, c.wantErr)
		}
	}
}

// TestVerifyExecDirOwnershipWalksAllFiveEntries 五项逐一遍历：全部达标通过；
// 任一项不达标都按路径报出；首个不达标即短路。
func TestVerifyExecDirOwnershipWalksAllFiveEntries(t *testing.T) {
	execDir := "/Library/Application Support/Tradewind"
	if err := verifyExecDirOwnershipWith(execDir, func(string) (ownerStat, error) {
		return rootOwned(0o755), nil
	}); err != nil {
		t.Fatalf("all root-owned entries must pass: %v", err)
	}

	entries := execDirEntries(execDir)
	for i, bad := range entries {
		err := verifyExecDirOwnershipWith(execDir, func(p string) (ownerStat, error) {
			if p == bad {
				return rootOwned(0o777), nil // world-writable
			}
			return rootOwned(0o755), nil
		})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("entry %d (%s) must be rejected by path, got %v", i, bad, err)
		}
	}

	// 短路：第一项（目录本身）不达标时不再查后续条目
	asked := map[string]bool{}
	err := verifyExecDirOwnershipWith(execDir, func(p string) (ownerStat, error) {
		asked[p] = true
		return ownerStat{mode: os.ModeDir | 0o755, uid: 501}, nil
	})
	if err == nil || len(asked) != 1 {
		t.Fatalf("first failing entry must short-circuit (asked %d entries, err = %v)", len(asked), err)
	}
}

// TestLstatOwnerSemantics 真 Lstat 投影的语义：普通文件报真实 uid、不带
// ModeSymlink；符号链接（含悬空）报 ModeSymlink 而不是跟随到目标。
func TestLstatOwnerSemantics(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs a non-root test user to observe a non-zero uid")
	}
	dir := shortTempDir(t)
	regular := filepath.Join(dir, "plain")
	if err := os.WriteFile(regular, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := lstatOwner(regular)
	if err != nil || st.uid != os.Getuid() || st.mode&os.ModeSymlink != 0 || st.mode.Perm() != 0o755 {
		t.Fatalf("lstatOwner(regular) = %+v err = %v, want uid %d mode 0755 without ModeSymlink", st, err, os.Getuid())
	}
	for name, target := range map[string]string{
		"link to a file": regular,
		"dangling link":  filepath.Join(dir, "missing"),
	} {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		st, err := lstatOwner(link)
		if err != nil || st.mode&os.ModeSymlink == 0 {
			t.Fatalf("lstatOwner(%s) = %+v err = %v, want ModeSymlink (must not follow)", name, st, err)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lstatOwner(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("lstatOwner(absent) must report the stat error")
	}
}

// TestVerifyExecDirOwnershipRealLstat 真 Lstat 接入遍历：非 root 测试用户属主的
// 执行目录被拒；后续条目（非 root 环境造不出 root 属主文件）用混合注入——目标
// 条目走真 lstatOwner、其余喂 root 属主结果——验证符号链接与缺失条目按路径报出。
func TestVerifyExecDirOwnershipRealLstat(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs a non-root test user to exercise the uid rejection")
	}
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	if err := os.WriteFile(HelperPathIn(p.execDir), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := verifyExecDirOwnership(p.execDir); err == nil ||
		!strings.Contains(err.Error(), "owned by uid "+strconv.Itoa(os.Getuid())) || !strings.Contains(err.Error(), p.execDir) {
		t.Fatalf("user-owned exec dir must be rejected by path, got %v", err)
	}

	// 混合注入：只有 target 走真 Lstat
	realFor := func(target string) func(string) (ownerStat, error) {
		return func(path string) (ownerStat, error) {
			if path == target {
				return lstatOwner(path)
			}
			return rootOwned(0o755), nil
		}
	}

	helper := HelperPathIn(p.execDir)
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.dataDir, "elsewhere"), helper); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecDirOwnershipWith(p.execDir, realFor(helper)); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), helper) {
		t.Fatalf("symlinked helper must be rejected by path, got %v", err)
	}

	sidecar := SidecarPathIn(p.execDir)
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecDirOwnershipWith(p.execDir, realFor(sidecar)); err == nil ||
		!strings.Contains(err.Error(), sidecar) {
		t.Fatalf("missing sidecar must be rejected by path, got %v", err)
	}
}

// TestInstallWithRejectsUntrustedExecDir 生产入口的编排：归属校验不达标 →
// "refusing to install"，不写 service.json、不碰 launchctl。
func TestInstallWithRejectsUntrustedExecDir(t *testing.T) {
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	lc := &fakeLaunchctl{}

	err := installWith(p, lc, &strings.Builder{}, func(string) error {
		return errors.New("service: exec dir entry /x is owned by uid 501 (must be root)")
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to install") || !strings.Contains(err.Error(), "owned by uid 501") {
		t.Fatalf("err = %v, want the ownership refusal surfaced", err)
	}
	if _, statErr := os.Stat(filepath.Join(p.configDir, "service.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("nothing may be written on an ownership refusal (err = %v)", statErr)
	}
	if len(lc.calls) != 0 {
		t.Fatalf("launchctl must not be touched on an ownership refusal: %v", lc.calls)
	}
}

// TestInstallWithChecksOwnershipThenInstalls 归属校验针对的正是 --exec-dir，且发生在
// 任何落盘之前；通过后才落锚并 bootout → bootstrap。
func TestInstallWithChecksOwnershipThenInstalls(t *testing.T) {
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	lc := &fakeLaunchctl{}
	var checked []string
	anchorAtCheck := "verifier never ran"

	err := installWith(p, lc, &strings.Builder{}, func(dir string) error {
		checked = append(checked, dir)
		_, statErr := os.Stat(filepath.Join(p.configDir, "service.json"))
		anchorAtCheck = "absent"
		if !errors.Is(statErr, os.ErrNotExist) {
			anchorAtCheck = "present"
		}
		return nil
	})
	if err != nil {
		t.Fatalf("installWith: %v", err)
	}
	if len(checked) != 1 || checked[0] != p.execDir {
		t.Fatalf("ownership check targets = %q, want exactly [%s]", checked, p.execDir)
	}
	if anchorAtCheck != "absent" {
		t.Fatalf("anchor at ownership-check time = %s, want absent (check must precede any write)", anchorAtCheck)
	}
	readWrittenAnchor(t, p) // 缺文件时自行 Fatalf
	if len(lc.calls) != 2 {
		t.Fatalf("launchctl calls = %v, want bootout + bootstrap", lc.calls)
	}
}
