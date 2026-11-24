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

// dir0755 是安全的目录条目。
func dir0755(string) (ownerStat, error) { return rootOwned(os.ModeDir | 0o755), nil }

// tableLstat 按表回放 Lstat 结果；表外路径视为不存在。
func tableLstat(table map[string]ownerStat) func(string) (ownerStat, error) {
	return func(path string) (ownerStat, error) {
		st, ok := table[path]
		if !ok {
			return ownerStat{}, os.ErrNotExist
		}
		return st, nil
	}
}

// chainTable 为执行目录的完整信任链生成"全部达标"的结果表（目录与文件均 0755
// root 属主），用例只需改动其中一项。
func chainTable(execDir string) map[string]ownerStat {
	table := map[string]ownerStat{}
	for _, p := range execDirChain(execDir) {
		table[p] = rootOwned(0o755)
	}
	return table
}

// safeSystemAncestors 是三个候选在标准 macOS 上的祖先基线（全部 root 属主 0755）。
func safeSystemAncestors() map[string]ownerStat {
	safe := map[string]ownerStat{}
	for _, dir := range []string{"/", "/Library", "/Library/Application Support", "/usr", "/usr/local", "/opt"} {
		safe[dir] = rootOwned(os.ModeDir | 0o755)
	}
	return safe
}

// namesEntry 断言错误信息精确点名某条目："entry <路径>" 之后必须是空格、冒号或
// 行尾（避免 /usr 与 /usr/local 这类前缀歧义；stat 失败的形态是 "entry <路径>: …"）。
func namesEntry(err error, path string) bool {
	if err == nil {
		return false
	}
	msg, needle := err.Error(), "entry "+path
	for from := 0; ; {
		idx := strings.Index(msg[from:], needle)
		if idx < 0 {
			return false
		}
		end := from + idx + len(needle)
		if end == len(msg) || msg[end] == ' ' || msg[end] == ':' {
			return true
		}
		from = end
	}
}

// --- 单项判定 ---

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
		if !namesEntry(err, "/x/entry") || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: err = %v, want %q naming the entry", c.name, err, c.wantErr)
		}
	}
}

// --- 信任链（纯函数）---

// TestAncestorDirs 祖先目录自上而下、到 / 为止；相对路径收到 "." 且不死循环。
func TestAncestorDirs(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/a/b/c", "/,/a,/a/b"},
		{"/a", "/"},
		{"a/b", ".,a"},
		{"x", "."},
	}
	for _, c := range cases {
		if got := strings.Join(ancestorDirs(c.path), ","); got != c.want {
			t.Fatalf("ancestorDirs(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestTrustChain 多路径展开为自上而下的信任链：共享祖先只出现一次，路径自身排在
// 其全部祖先之后；空路径跳过。
func TestTrustChain(t *testing.T) {
	got := trustChain("/x/bin/mihomo", "/x/helper", "/x/bin/mihomo.sha256")
	want := "/,/x,/x/bin,/x/bin/mihomo,/x/helper,/x/bin/mihomo.sha256"
	if strings.Join(got, ",") != want {
		t.Fatalf("trustChain = %q, want %q", strings.Join(got, ","), want)
	}
	withEmpty := trustChain("", "/x/helper")
	if strings.Join(withEmpty, ",") != "/,/x,/x/helper" {
		t.Fatalf("trustChain with an empty path = %q, want the empty one skipped", strings.Join(withEmpty, ","))
	}
}

// TestExecDirChain 执行目录的完整信任链：/ → … → 执行目录 → helper → bin → 内核 →
// 校验文件（含空格路径的候选）。
func TestExecDirChain(t *testing.T) {
	execDir := "/Library/Application Support/Tradewind"
	want := []string{
		"/", "/Library", "/Library/Application Support", execDir,
		HelperPathIn(execDir), filepath.Join(execDir, "bin"), CorePathIn(execDir), SidecarPathIn(execDir),
	}
	if got := strings.Join(execDirChain(execDir), "|"); got != strings.Join(want, "|") {
		t.Fatalf("execDirChain =\n%s\nwant\n%s", got, strings.Join(want, "|"))
	}
}

// --- 使用点一：Install 与运行期共用的严格复核 ---

// TestVerifyExecDirOwnershipWalksAllFiveEntries 执行目录内五项逐一遍历：全部达标
// 通过；任一项不达标都按路径报出；首个不达标即短路。
func TestVerifyExecDirOwnershipWalksAllFiveEntries(t *testing.T) {
	execDir := "/Library/Application Support/Tradewind"
	if err := verifyExecDirOwnershipWith(execDir, dir0755); err != nil {
		t.Fatalf("all root-owned entries must pass: %v", err)
	}

	for i, bad := range execDirEntries(execDir) {
		table := chainTable(execDir)
		table[bad] = rootOwned(0o777) // world-writable
		err := verifyExecDirOwnershipWith(execDir, tableLstat(table))
		if !namesEntry(err, bad) {
			t.Fatalf("entry %d (%s) must be rejected by path, got %v", i, bad, err)
		}
	}

	asked := map[string]bool{}
	err := verifyExecDirOwnershipWith(execDir, func(p string) (ownerStat, error) {
		asked[p] = true
		return ownerStat{mode: os.ModeDir | 0o755, uid: 501}, nil
	})
	if err == nil || len(asked) != 1 {
		t.Fatalf("first failing entry must short-circuit (asked %d entries, err = %v)", len(asked), err)
	}
}

// TestVerifyExecDirOwnershipIncludesAncestors 祖先目录纳入复核（review P2）：祖先
// 用户可写、是符号链接、group 可写、缺失，任一即拒并点名该级；自上而下先报最上层。
func TestVerifyExecDirOwnershipIncludesAncestors(t *testing.T) {
	execDir := "/usr/local/libexec/tradewind"
	cases := []struct {
		name    string
		badPath string
		bad     ownerStat // 零值 + delete = 缺失
		missing bool
		wantErr string
	}{
		{"ancestor owned by a user (Intel Homebrew /usr/local)", "/usr/local", ownerStat{mode: os.ModeDir | 0o755, uid: 501}, false, "owned by uid 501"},
		{"ancestor is a symlink", "/usr/local", rootOwned(os.ModeSymlink | 0o777), false, "symbolic link"},
		{"ancestor group-writable (admin)", "/usr/local", rootOwned(os.ModeDir | 0o775), false, "group/other-writable"},
		{"ancestor missing", "/usr/local", ownerStat{}, true, "does not exist"},
	}
	for _, c := range cases {
		table := chainTable(execDir)
		if c.missing {
			delete(table, c.badPath)
		} else {
			table[c.badPath] = c.bad
		}
		err := verifyExecDirOwnershipWith(execDir, tableLstat(table))
		if !namesEntry(err, c.badPath) || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: err = %v, want %q reported on %s", c.name, err, c.wantErr, c.badPath)
		}
	}

	// 自上而下：两层都不达标时先报最上层
	table := chainTable(execDir)
	table["/usr"] = ownerStat{mode: os.ModeDir | 0o755, uid: 501}
	table["/usr/local"] = ownerStat{mode: os.ModeDir | 0o777, uid: 0}
	err := verifyExecDirOwnershipWith(execDir, tableLstat(table))
	if !namesEntry(err, "/usr") || !strings.Contains(err.Error(), "owned by uid 501") {
		t.Fatalf("err = %v, want the top-most failing ancestor /usr reported first", err)
	}

	// 祖先全部安全、链完整 → 通过
	if err := verifyExecDirOwnershipWith(execDir, tableLstat(chainTable(execDir))); err != nil {
		t.Fatalf("safe chain must pass: %v", err)
	}
}

// TestVerifyAnchorExecTrustUsesRecordedPaths 运行期复核按锚里**记录的实际路径**展开
// 信任链，不假定制式布局：helper 与内核分属两个目录时，各自的祖先都在链上。
func TestVerifyAnchorExecTrustUsesRecordedPaths(t *testing.T) {
	anchor := ConfigFile{
		HelperPath: "/a/helper",
		CoreBin:    "/b/bin/mihomo",
		Sidecar:    "/b/bin/mihomo.sha256",
	}
	chain := strings.Join(anchorChain(anchor), ",")
	want := "/,/a,/a/helper,/b,/b/bin,/b/bin/mihomo,/b/bin/mihomo.sha256"
	if chain != want {
		t.Fatalf("anchorChain = %q, want %q", chain, want)
	}
	// 内核所在目录被换成非 root 属主 → 拒并点名（不是只查 helper 的目录）
	err := verifyAnchorExecTrustWith(anchor, func(p string) (ownerStat, error) {
		if p == "/b" {
			return ownerStat{mode: os.ModeDir | 0o755, uid: 501}, nil
		}
		return rootOwned(0o755), nil
	})
	if !namesEntry(err, "/b") {
		t.Fatalf("err = %v, want the core's own ancestor /b reported", err)
	}
	// 旧锚缺 helper_path：仍复核内核与校验文件的链
	noHelper := anchor
	noHelper.HelperPath = ""
	err = verifyAnchorExecTrustWith(noHelper, func(p string) (ownerStat, error) {
		if p == "/b/bin/mihomo" {
			return rootOwned(os.ModeSymlink | 0o777), nil
		}
		return rootOwned(0o755), nil
	})
	if !namesEntry(err, "/b/bin/mihomo") || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want the symlinked core rejected without helper_path", err)
	}
	if err := verifyAnchorExecTrustWith(noHelper, dir0755); err != nil {
		t.Fatalf("safe chain must pass: %v", err)
	}
}

// --- 使用点三：CLI 侧候选过滤（只判已存在的条目）---

// TestSafeExecDirCandidates 候选过滤矩阵：已存在的祖先须安全；尚不存在的一级起
// 不再往下判（将由 root 新建）；候选自身若已存在同样要达标；不安全的候选连同
// 原因返回，安全的保持原序。
func TestSafeExecDirCandidates(t *testing.T) {
	c1 := "/Library/Application Support/Tradewind"
	c2 := "/usr/local/libexec/tradewind"
	c3 := "/opt/tradewind"
	cands := []string{c1, c2, c3}
	joined := func(list []string) string { return strings.Join(list, "|") }

	t.Run("all safe, candidates not yet created", func(t *testing.T) {
		safe, rejected := safeExecDirCandidatesWith(cands, tableLstat(safeSystemAncestors()))
		if joined(safe) != joined(cands) || len(rejected) != 0 {
			t.Fatalf("safe = %q rejected = %+v, want all three in order", safe, rejected)
		}
	})

	t.Run("user-writable ancestor excludes only that candidate", func(t *testing.T) {
		table := safeSystemAncestors()
		table["/usr/local"] = ownerStat{mode: os.ModeDir | 0o755, uid: 501}
		safe, rejected := safeExecDirCandidatesWith(cands, tableLstat(table))
		if joined(safe) != joined([]string{c1, c3}) {
			t.Fatalf("safe = %q, want %q", safe, joined([]string{c1, c3}))
		}
		if len(rejected) != 1 || rejected[0].Candidate != c2 ||
			!namesEntry(rejected[0].Reason, "/usr/local") {
			t.Fatalf("rejected = %+v, want %s excluded because of /usr/local", rejected, c2)
		}
	})

	t.Run("missing entries stop at the nearest existing ancestor", func(t *testing.T) {
		// /usr/local 安全、其下尚不存在 → 通过（其下各级都还不存在，由 root 新建）
		if safe, _ := safeExecDirCandidatesWith([]string{c2}, tableLstat(safeSystemAncestors())); len(safe) != 1 {
			t.Fatal("candidate under a safe existing /usr/local must be kept")
		}
		// /usr/local 本身也不存在：最近的已存在祖先是 /usr（安全）→ 仍通过
		table := safeSystemAncestors()
		delete(table, "/usr/local")
		if safe, _ := safeExecDirCandidatesWith([]string{c2}, tableLstat(table)); len(safe) != 1 {
			t.Fatal("candidate kept when the nearest existing ancestor /usr is safe")
		}
		// 最近的已存在祖先 /usr 归用户 → 拒
		table["/usr"] = ownerStat{mode: os.ModeDir | 0o755, uid: 501}
		safe, rejected := safeExecDirCandidatesWith([]string{c2}, tableLstat(table))
		if len(safe) != 0 || len(rejected) != 1 || !namesEntry(rejected[0].Reason, "/usr") {
			t.Fatalf("safe = %q rejected = %+v, want rejection via the nearest existing ancestor /usr", safe, rejected)
		}
	})

	t.Run("symlinked or world-writable ancestors", func(t *testing.T) {
		cases := []struct {
			name    string
			path    string
			st      ownerStat
			wantErr string
		}{
			{"symlinked Application Support", "/Library/Application Support", rootOwned(os.ModeSymlink | 0o755), "symbolic link"},
			{"world-writable /opt", "/opt", rootOwned(os.ModeDir | 0o777), "group/other-writable"},
		}
		for _, c := range cases {
			table := safeSystemAncestors()
			table[c.path] = c.st
			safe, rejected := safeExecDirCandidatesWith(cands, tableLstat(table))
			if len(safe) != 2 || len(rejected) != 1 || !strings.Contains(rejected[0].Reason.Error(), c.wantErr) {
				t.Fatalf("%s: safe = %q rejected = %+v, want one rejection (%s)", c.name, safe, rejected, c.wantErr)
			}
		}
	})

	t.Run("existing candidate directory must itself be sound", func(t *testing.T) {
		table := safeSystemAncestors()
		table[c3] = rootOwned(os.ModeSymlink | 0o777) // /opt/tradewind 被换成符号链接
		safe, rejected := safeExecDirCandidatesWith(cands, tableLstat(table))
		if len(safe) != 2 || len(rejected) != 1 || rejected[0].Candidate != c3 ||
			!strings.Contains(rejected[0].Reason.Error(), "symbolic link") {
			t.Fatalf("safe = %q rejected = %+v, want %s excluded as a symlink", safe, rejected, c3)
		}
	})

	t.Run("stat error other than not-exist is a rejection", func(t *testing.T) {
		safe, rejected := safeExecDirCandidatesWith(cands, func(p string) (ownerStat, error) {
			if p == "/opt" {
				return ownerStat{}, errors.New("permission denied")
			}
			return rootOwned(os.ModeDir | 0o755), nil
		})
		if len(safe) != 2 || len(rejected) != 1 || !strings.Contains(rejected[0].Reason.Error(), "permission denied") {
			t.Fatalf("safe = %q rejected = %+v, want /opt/tradewind rejected on the stat error", safe, rejected)
		}
	})

	t.Run("none left reports every candidate with its reason", func(t *testing.T) {
		safe, rejected := safeExecDirCandidatesWith(cands, func(string) (ownerStat, error) {
			return ownerStat{mode: os.ModeDir | 0o777, uid: 501}, nil
		})
		if len(safe) != 0 || len(rejected) != len(cands) {
			t.Fatalf("safe = %q rejected = %+v, want no candidate left", safe, rejected)
		}
		for i, r := range rejected {
			if r.Candidate != cands[i] || r.Reason == nil {
				t.Fatalf("rejection %d = %+v, want %s with a reason", i, r, cands[i])
			}
		}
	})
}

// TestSafeExecDirCandidatesOnThisMachine 真机只读探测：三个候选的祖先在标准 macOS
// 上都应安全（/Library/Application Support 为 root:admin 0755，/usr、/opt 为
// root:wheel）。只断言不变量（安全项是候选的有序子集、拒绝项带原因），实际结果
// 记入日志——不同机型上 /usr/local 的属主本就不同，不能当断言。
func TestSafeExecDirCandidatesOnThisMachine(t *testing.T) {
	safe, rejected := SafeExecDirCandidates()
	t.Logf("safe candidates: %q; rejected: %+v", safe, rejected)
	all := ExecDirCandidates()
	if len(safe)+len(rejected) != len(all) {
		t.Fatalf("safe %q + rejected %+v must partition %q", safe, rejected, all)
	}
	next := 0
	for _, s := range safe {
		for next < len(all) && all[next] != s {
			next++
		}
		if next == len(all) {
			t.Fatalf("safe candidate %q is out of order or unknown", s)
		}
	}
	for _, r := range rejected {
		if r.Reason == nil {
			t.Fatalf("rejection of %s must carry a reason", r.Candidate)
		}
	}
}

// --- 真 Lstat ---

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

// TestVerifyExecDirOwnershipRealLstat 真 Lstat 接入遍历，验证执行目录**之内**各条目
// 的判定。祖先段喂 root 属主结果：开发机临时目录在 /tmp 下，祖先含系统符号链接与
// 全局可写目录，真 Lstat 会在祖先处先拒（见下一个用例）；非 root 环境也造不出
// root 属主文件。
func TestVerifyExecDirOwnershipRealLstat(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs a non-root test user to exercise the uid rejection")
	}
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	if err := os.WriteFile(HelperPathIn(p.execDir), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// execDir 之下的条目走真 Lstat，祖先放行
	realUnderExecDir := func(path string) (ownerStat, error) {
		if strings.HasPrefix(path, p.execDir+string(filepath.Separator)) || path == p.execDir {
			return lstatOwner(path)
		}
		return rootOwned(os.ModeDir | 0o755), nil
	}
	// 只有 target 走真 Lstat
	realFor := func(target string) func(string) (ownerStat, error) {
		return func(path string) (ownerStat, error) {
			if path == target {
				return lstatOwner(path)
			}
			return rootOwned(os.ModeDir | 0o755), nil
		}
	}

	err := verifyExecDirOwnershipWith(p.execDir, realUnderExecDir)
	if !namesEntry(err, p.execDir) ||
		!strings.Contains(err.Error(), "owned by uid "+strconv.Itoa(os.Getuid())) {
		t.Fatalf("user-owned exec dir must be rejected by path, got %v", err)
	}

	helper := HelperPathIn(p.execDir)
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.dataDir, "elsewhere"), helper); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecDirOwnershipWith(p.execDir, realFor(helper)); !namesEntry(err, helper) ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlinked helper must be rejected by path, got %v", err)
	}

	sidecar := SidecarPathIn(p.execDir)
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecDirOwnershipWith(p.execDir, realFor(sidecar)); !namesEntry(err, sidecar) {
		t.Fatalf("missing sidecar must be rejected by path, got %v", err)
	}
}

// TestVerifyExecDirOwnershipRejectsSymlinkedAncestor 真 Lstat 下的祖先校验：开发机
// 临时目录的祖先是系统符号链接 /tmp——严格复核必须在祖先处拒绝，而不是只看
// 执行目录自身。
func TestVerifyExecDirOwnershipRejectsSymlinkedAncestor(t *testing.T) {
	if info, err := os.Lstat("/tmp"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Skip("/tmp is not a symlink on this machine")
	}
	execDir := filepath.Join(shortTempDir(t), "exec") // /tmp/<rand>/exec
	if err := os.MkdirAll(filepath.Join(execDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := verifyExecDirOwnership(execDir)
	if !namesEntry(err, "/tmp") || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want the symlinked ancestor /tmp rejected before anything under it", err)
	}
}

// --- Install 入口 ---

// TestInstallWithRejectsUntrustedExecDir 生产入口的编排：信任链复核不达标 →
// "refusing to install"，不写 service.json、不碰 launchctl。
func TestInstallWithRejectsUntrustedExecDir(t *testing.T) {
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	lc := &fakeLaunchctl{}

	err := installWith(p, lc, &strings.Builder{}, func(string) error {
		return errors.New("service: exec dir entry /usr/local is owned by uid 501 (must be root)")
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

// TestInstallWithChecksOwnershipThenInstalls 信任链复核针对的正是 --exec-dir，且发生
// 在任何落盘之前；通过后才落锚并 bootout → bootstrap。
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
