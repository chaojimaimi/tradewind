package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/service"
)

// commandLines 返回去注释的命令行（红线断言的对象——注释里对"绝不 rm -rf"的
// 说明文字属预期，不应触发字面匹配）。
func commandLines(script string) string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
		}
	}
	return strings.ToLower(strings.Join(kept, "\n"))
}

// indexOfLine 返回含 sub 的首行下标（找不到 = -1）。
func indexOfLine(lines []string, sub string) int {
	for i, line := range lines {
		if strings.Contains(line, sub) {
			return i
		}
	}
	return -1
}

// TestTunInstallScriptContent 安装脚本内容断言（batch6 真机 fold-in）：候选顺序、
// rm 先于 cp、执行自测行、遗留位置清理、其余候选清理、全败 exit 3、末行 install
// 带 --exec-dir、无 rm -rf、无 bashism。
func TestTunInstallScriptContent(t *testing.T) {
	script := tunInstallScript("/Applications/Tradewind.app/cli",
		"/Users/le e/Library/Application Support/tradewind", 501)
	lines := scriptLines(script)
	for _, want := range []string{
		`CLI='/Applications/Tradewind.app/cli'`,
		`DATA='/Users/le e/Library/Application Support/tradewind'`,
		`DATA_UID=501`,
		`ANCHOR='/Library/Application Support/Tradewind'`,
		`LEGACY='/Library/PrivilegedHelperTools/io.github.chaojimaimi.tradewind.service'`,
		`mkdir -p "$ANCHOR" || fail "无法创建锚目录 $ANCHOR" 1`,
		`chown root:wheel "$ANCHOR" || fail "无法设置锚目录属主 $ANCHOR" 1`,
		`chmod 0755 "$ANCHOR" || fail "无法设置锚目录权限 $ANCHOR" 1`,
		// 遗留位置：只清理，不落位
		`rm -f "$LEGACY"`,
		// 源文件预检（缺失 ≠ 系统策略拦截）
		`[ -f "$CORE" ] || fail "数据目录缺少内核 $CORE（内核尚未安装？）" 4`,
		// 执行自测：两个二进制都能执行才算该位置可用
		`if "$CAND/tradewind-service" version >/dev/null 2>&1 && "$CAND/bin/mihomo" -v >/dev/null 2>&1; then`,
		`echo "tradewind: $CAND 被系统策略拦截，尝试下一处" >&2`,
		`chmod 0644 "$CAND/bin/mihomo.sha256"`,
		// 末步：helper install 携带 --exec-dir
		`"$EXE/tradewind-service" install --data-dir "$DATA" --data-uid "$DATA_UID" --exec-dir "$EXE"`,
		`exit "$?"`,
	} {
		if indexOfLine(lines, want) < 0 {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}

	// 候选顺序：锚目录 → libexec → /opt——三个探测在同一条 `||` 链上按序短路，
	// 链尾收于全败 exit 3
	chainIdx := indexOfLine(lines, `try_candidate '/Library/Application Support/Tradewind' ||`)
	if chainIdx < 0 {
		t.Fatalf("candidate probe chain missing:\n%s", script)
	}
	chain := lines[chainIdx]
	if strings.Count(chain, "try_candidate '") != 3 {
		t.Fatalf("probe chain must hold exactly the three candidates:\n%s", chain)
	}
	lastPos := -1
	for _, cand := range service.ExecDirCandidates() {
		pos := strings.Index(chain, "try_candidate '"+cand+"' || ")
		if pos <= lastPos {
			t.Fatalf("candidate %s out of order in chain %q", cand, chain)
		}
		lastPos = pos
	}
	// 链尾文案：说明系统策略拦截 + quarantine 提示（fold-in 2），以 exit 3 收尾
	terminator := "均被 macOS 系统策略拦截（未公证二进制受限），未安装任何文件：" +
		strings.Join(service.ExecDirCandidates(), " / ") +
		"；若 CLI/内核二进制带 com.apple.quarantine 扩展属性（浏览器下载后解压常见），副本会继承并被拦" +
		"——先对解压目录执行 xattr -dr com.apple.quarantine <目录> 再重试' 3"
	if !strings.HasSuffix(chain, terminator) {
		t.Fatalf("probe chain must terminate in the explained exit 3:\n%s", chain)
	}

	// rm 必须先于 cp（新 inode：cp 原地覆写运行中二进制 = ETXTBSY）
	rm := indexOfLine(lines, `rm -f "$CAND/tradewind-service" "$CAND/bin/mihomo" "$CAND/bin/mihomo.sha256"`)
	cp := indexOfLine(lines, `if ! cp "$CLI" "$CAND/tradewind-service"`)
	if rm < 0 || cp < 0 || rm > cp {
		t.Fatalf("rm must precede cp (rm=%d cp=%d):\n%s", rm, cp, script)
	}

	// 其余候选清理（历史安装可能落在别的候选）
	for _, cand := range service.ExecDirCandidates() {
		want := `if [ "$EXE" != '` + cand + `' ]; then clean_candidate '` + cand + `'; fi`
		if indexOfLine(lines, want) < 0 {
			t.Fatalf("script missing stale-candidate cleanup for %s:\n%s", cand, script)
		}
	}

	// 红线与兼容性：无 rm -rf；不往遗留位置拷贝；无 bashism（只看命令行）
	lower := commandLines(script)
	for _, banned := range []string{"rm -rf", "rm -fr", "rm -r ", "local ", "declare ", "[[", "function "} {
		if strings.Contains(lower, banned) {
			t.Fatalf("script must not contain %q:\n%s", banned, script)
		}
	}
	if strings.Contains(script, "cp '$CLI' '/Library/PrivilegedHelperTools") ||
		strings.Contains(script, "cp \"/Library/PrivilegedHelperTools") {
		t.Fatalf("script must not install into the legacy helper location:\n%s", script)
	}
}

// TestTunUninstallScriptContent 卸载脚本内容断言（批⑥b §4.5）：bootout → 逐候选
// 调 helper 的 purge（须在 clean_candidate 之前——clean 之后 helper 就没了）→
// rm -f plist/三份日志/锚文件/锚目录 core.sock/遗留 helper → 逐候选清理 → 回收
// logs 与锚目录（仅空时）→ 回收可能新建的父目录 → exit 0；无 rm -rf。
func TestTunUninstallScriptContent(t *testing.T) {
	script := tunUninstallScript()
	lines := scriptLines(script)
	bootout := indexOfLine(lines, "launchctl bootout system/"+service.DaemonLabel+" 2>/dev/null || true")
	anchor := service.DefaultConfigDir
	logs := service.DefaultLogsDir
	rmLine := `rm -f '/Library/LaunchDaemons/` + service.DaemonLabel + `.plist' '` + logs + `/service.log' '` +
		logs + `/core.log' '` + logs + `/core.log.1' '` + anchor + `/service.json' '` +
		anchor + `/core.pid' '` + anchor + `/core.sock' '/Library/PrivilegedHelperTools/` + service.DaemonLabel + `'`
	rm := indexOfLine(lines, rmLine)
	if bootout < 0 || rm < 0 || bootout > rm {
		t.Fatalf("uninstall must boot out before removing files (bootout=%d rm=%d):\n%s", bootout, rm, script)
	}
	// 逐候选 purge 调用，且全部位于首个 clean_candidate 之前
	firstClean := -1
	for _, cand := range service.ExecDirCandidates() {
		purgeLine := `[ -x '` + cand + `/tradewind-service' ] && '` + cand + `/tradewind-service' purge || true`
		i := indexOfLine(lines, purgeLine)
		if i < 0 {
			t.Fatalf("uninstall script missing purge call for %s:\n%s", cand, script)
		}
		cleanIdx := indexOfLine(lines, "clean_candidate '"+cand+"'")
		if cleanIdx < 0 {
			t.Fatalf("uninstall script missing candidate cleanup %s:\n%s", cand, script)
		}
		if firstClean < 0 || cleanIdx < firstClean {
			firstClean = cleanIdx
		}
		if i > cleanIdx {
			t.Fatalf("purge for %s must run before its clean_candidate (purge=%d clean=%d):\n%s", cand, i, cleanIdx, script)
		}
	}
	rmdirLogs := indexOfLine(lines, "rmdir '"+logs+"' 2>/dev/null")
	rmdirAnchor := indexOfLine(lines, "rmdir '"+anchor+"' 2>/dev/null")
	parent := indexOfLine(lines, "rmdir '/usr/local/libexec' 2>/dev/null")
	exit := indexOfLine(lines, "exit 0")
	if rmdirLogs < firstClean || rmdirAnchor < rmdirLogs || parent < rmdirAnchor || exit != len(lines)-1 {
		t.Fatalf("uninstall epilogue order wrong (firstClean=%d rmdirLogs=%d rmdirAnchor=%d parent=%d exit=%d):\n%s",
			firstClean, rmdirLogs, rmdirAnchor, parent, exit, script)
	}
	// 逐候选清理用显式文件路径；目录仅在空时移除
	for _, want := range []string{
		`rm -f "$1/tradewind-service" "$1/bin/mihomo" "$1/bin/mihomo.sha256"`,
		`rmdir "$1/bin" 2>/dev/null`,
		`rmdir "$1" 2>/dev/null`,
	} {
		if indexOfLine(lines, want) < 0 {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	lower := commandLines(script)
	for _, banned := range []string{"rm -rf", "rm -fr", "rm -r ", "local ", "rmdir '/opt"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("uninstall script must not contain %q:\n%s", banned, script)
		}
	}
}

// TestTunScriptsParseUnderBinSh 生成脚本能被 /bin/sh 解析（`sh -n` 仅做语法检查，
// 不执行任何命令——绝不提权/触真系统目录）。
func TestTunScriptsParseUnderBinSh(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{
		"install.sh":   tunInstallScript("/Applications/Tradewind.app/cli", "/Users/le e/Library/Application Support/tradewind", 501),
		"uninstall.sh": tunUninstallScript(),
	}
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/bin/sh", "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("%s is not valid POSIX sh: %v\n%s\n--- script ---\n%s", name, err, out, body)
		}
	}
}

// --- 行为测试：真 /bin/sh 跑生成的脚本，落点全在临时目录 ---

// scriptFixture 是提权脚本行为测试的落点（根目录含空格——生产第一候选
// /Library/Application Support 同形，验证逐段引用）。PATH 前置假 chown/launchctl：
// 真 chown 需要 root、真 launchctl 触真 launchd，二者在测试里都必须是桩。
type scriptFixture struct {
	root    string
	layout  tunScriptLayout
	fakeBin string
	record  string // 桩二进制记录调用参数的目录
	dataDir string
	cli     string
}

const fakePolicyBlockLogic = `if [ -n "${FAKE_BLOCK_DIR:-}" ]; then
	case "$0" in
	*"$FAKE_BLOCK_DIR"*) exit 137 ;;
	esac
fi
`

func newScriptFixture(t *testing.T) *scriptFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root dir")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &scriptFixture{root: root}
	f.fakeBin = filepath.Join(root, "fake bin")
	f.record = filepath.Join(root, "record")
	f.dataDir = filepath.Join(root, "user data")
	for _, d := range []string{f.fakeBin, f.record, filepath.Join(f.dataDir, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	anchor := filepath.Join(root, "Anchor Dir")
	f.layout = tunScriptLayout{
		anchorDir:  anchor,
		legacy:     filepath.Join(root, "legacy helper", service.DaemonLabel),
		candidates: []string{anchor, filepath.Join(root, "lib exec", "tradewind"), filepath.Join(root, "opt", "tradewind")},
		plist:      filepath.Join(root, "LaunchDaemons", service.DaemonLabel+".plist"),
		logsDir:    filepath.Join(anchor, "logs"),
		anchorSock: filepath.Join(anchor, "core.sock"),
		corePID:    filepath.Join(anchor, "core.pid"),
		rmdirDirs:  []string{filepath.Join(root, "lib exec")},
	}
	f.writeExecutable(t, filepath.Join(f.fakeBin, "chown"), "#!/bin/sh\nexit 0\n")
	f.writeExecutable(t, filepath.Join(f.fakeBin, "launchctl"),
		"#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$FAKE_RECORD_DIR/launchctl.args\"\nexit 0\n")
	// 假 CLI：`version` 可执行（自测）；`install` 记录 argv；从被拦目录执行 = 137
	f.cli = filepath.Join(root, "Tradewind.app", "cli")
	if err := os.MkdirAll(filepath.Dir(f.cli), 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeExecutable(t, f.cli, "#!/bin/sh\n"+fakePolicyBlockLogic+
		`case "$1" in
version) echo "tradewind fake"; exit 0 ;;
install) printf '%s\n' "$@" > "$FAKE_RECORD_DIR/install.args"; exit 0 ;;
esac
exit 2
`)
	f.writeExecutable(t, filepath.Join(f.dataDir, "bin", "mihomo"),
		"#!/bin/sh\n"+fakePolicyBlockLogic+"echo \"Mihomo Meta fake v1\"\nexit 0\n")
	if err := os.WriteFile(filepath.Join(f.dataDir, "bin", "mihomo.sha256"),
		[]byte("0000000000000000000000000000000000000000000000000000000000000000  mihomo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *scriptFixture) writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// envWith 返回 env 的副本，其中 key 被替换为 value（不改动进程环境）。
func envWith(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// run 以 /bin/sh 执行脚本；blockDir 非空 = 模拟"该目录被系统策略拦截"（从中执行
// 的二进制以 137 退出，即 Gatekeeper SIGKILL 的表现）。
func (f *scriptFixture) run(t *testing.T, script, blockDir string) (exitCode int, stderr string) {
	t.Helper()
	path := filepath.Join(f.root, "script.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", path)
	cmd.Env = []string{
		"PATH=" + f.fakeBin + ":/usr/bin:/bin",
		"FAKE_RECORD_DIR=" + f.record,
		"FAKE_BLOCK_DIR=" + blockDir,
		"HOME=" + t.TempDir(),
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err == nil {
		return 0, errOut.String()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run script: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	return exitErr.ExitCode(), errOut.String()
}

func (f *scriptFixture) recordLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.record, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// assertCandidatePopulated 断言候选目录内三件套齐全且权限为 0755/0755/0644。
func (f *scriptFixture) assertCandidatePopulated(t *testing.T, cand string) {
	t.Helper()
	for name, want := range map[string]os.FileMode{
		"tradewind-service": 0o755,
		"bin/mihomo":        0o755,
		"bin/mihomo.sha256": 0o644,
	} {
		info, err := os.Stat(filepath.Join(cand, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("candidate %s missing %s: %v", cand, name, err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s/%s perm = %o, want %o", cand, name, info.Mode().Perm(), want)
		}
	}
}

// plant 三件套占位（模拟历史安装落位）。helper 副本是记录参数的 shell 脚本
// （批⑥b：卸载脚本会调用它的 purge 子命令）——绝不能执行真实的 tradewind 二进制。
func (f *scriptFixture) plant(t *testing.T, cand string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cand, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeExecutable(t, filepath.Join(cand, "tradewind-service"), "#!/bin/sh\n"+fakePolicyBlockLogic+
		`printf '%s\n' "$0 $*" >> "$FAKE_RECORD_DIR/helper-args.log"
exit 0
`)
	for _, name := range []string{"bin/mihomo", "bin/mihomo.sha256"} {
		if err := os.WriteFile(filepath.Join(cand, filepath.FromSlash(name)), []byte("stale"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *scriptFixture) assertCandidateClean(t *testing.T, cand string) {
	t.Helper()
	for _, name := range []string{"tradewind-service", "bin/mihomo", "bin/mihomo.sha256"} {
		if _, err := os.Lstat(filepath.Join(cand, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate %s still holds %s (err = %v)", cand, name, err)
		}
	}
}

// TestTunInstallScriptFirstCandidatePasses 首候选（含空格路径）自测通过：三件套
// 落位且权限正确、其余候选不创建、遗留 helper 被清、末步 install 携带 --exec-dir。
func TestTunInstallScriptFirstCandidatePasses(t *testing.T) {
	f := newScriptFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.layout.legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.layout.legacy, []byte("leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	c1 := f.layout.candidates[0]

	code, stderr := f.run(t, tunInstallScriptFor(f.cli, f.dataDir, 501, f.layout), "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	f.assertCandidatePopulated(t, c1)
	for _, other := range f.layout.candidates[1:] {
		if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unselected candidate %s must not be created (err = %v)", other, err)
		}
	}
	if _, err := os.Stat(f.layout.legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy helper must be removed (err = %v)", err)
	}
	want := []string{"install", "--data-dir", f.dataDir, "--data-uid", "501", "--exec-dir", c1}
	if got := f.recordLines(t, "install.args"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("install argv = %q, want %q", got, want)
	}
	if !strings.Contains(stderr, "安装位置 "+c1) {
		t.Fatalf("stderr must name the selected location (it surfaces if the helper fails later):\n%s", stderr)
	}
}

// TestTunInstallScriptFallsBackWhenPolicyBlocks 首候选被系统策略拦截 → 清理该
// 候选 + 下一处；选定后清理其余候选里的陈旧三文件（诱饵文件保留——无 rm -rf）。
func TestTunInstallScriptFallsBackWhenPolicyBlocks(t *testing.T) {
	f := newScriptFixture(t)
	c1, c2, c3 := f.layout.candidates[0], f.layout.candidates[1], f.layout.candidates[2]
	f.plant(t, c3) // 历史安装落在第三候选
	decoy := filepath.Join(c3, "unrelated.txt")
	if err := os.WriteFile(decoy, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stderr := f.run(t, tunInstallScriptFor(f.cli, f.dataDir, 501, f.layout), c1)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, c1+" 被系统策略拦截，尝试下一处") {
		t.Fatalf("stderr must report the blocked location:\n%s", stderr)
	}
	f.assertCandidateClean(t, c1)
	if _, err := os.Lstat(filepath.Join(c1, "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked candidate's bin dir must be removed (err = %v)", err)
	}
	f.assertCandidatePopulated(t, c2)
	f.assertCandidateClean(t, c3)
	if data, err := os.ReadFile(decoy); err != nil || string(data) != "keep me" {
		t.Fatalf("stale-candidate cleanup must only touch the three files (decoy err = %v)", err)
	}
	want := []string{"install", "--data-dir", f.dataDir, "--data-uid", "501", "--exec-dir", c2}
	if got := f.recordLines(t, "install.args"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("install argv = %q, want %q", got, want)
	}
}

// TestTunInstallScriptAllCandidatesBlocked 全部候选被拦 → exit 3 + stderr 说明
// （未公证二进制受限）；任何候选都不留文件，helper install 不被调用。
func TestTunInstallScriptAllCandidatesBlocked(t *testing.T) {
	f := newScriptFixture(t)

	code, stderr := f.run(t, tunInstallScriptFor(f.cli, f.dataDir, 501, f.layout), f.root)
	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "均被 macOS 系统策略拦截（未公证二进制受限）") {
		t.Fatalf("stderr must explain the policy block:\n%s", stderr)
	}
	if strings.Count(stderr, "被系统策略拦截，尝试下一处") != len(f.layout.candidates) {
		t.Fatalf("each candidate must report one blocked line:\n%s", stderr)
	}
	// quarantine 提示：带 com.apple.quarantine 的二进制其副本会继承并被拦
	if !strings.Contains(stderr, "com.apple.quarantine") || !strings.Contains(stderr, "xattr -dr com.apple.quarantine <目录>") {
		t.Fatalf("the all-blocked message must carry the quarantine hint:\n%s", stderr)
	}
	for _, cand := range f.layout.candidates {
		f.assertCandidateClean(t, cand)
	}
	if _, err := os.Stat(filepath.Join(f.record, "install.args")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper install must not run when every candidate is blocked (err = %v)", err)
	}
}

// TestTunInstallScriptMissingSource 源文件缺失（数据目录无内核）→ exit 4 并指明
// 缺失文件——不得误报成"系统策略拦截"，也不动任何候选。
func TestTunInstallScriptMissingSource(t *testing.T) {
	f := newScriptFixture(t)
	if err := os.Remove(filepath.Join(f.dataDir, "bin", "mihomo")); err != nil {
		t.Fatal(err)
	}

	code, stderr := f.run(t, tunInstallScriptFor(f.cli, f.dataDir, 501, f.layout), "")
	if code != 4 {
		t.Fatalf("exit = %d, want 4; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, filepath.Join(f.dataDir, "bin", "mihomo")) || strings.Contains(stderr, "系统策略拦截") {
		t.Fatalf("stderr must name the missing file and not blame system policy:\n%s", stderr)
	}
	for _, cand := range f.layout.candidates {
		if _, err := os.Stat(cand); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("no candidate may be touched when sources are missing (err = %v)", err)
		}
	}
}

// TestTunUninstallScriptBehaviour 卸载行为（批⑥b §4.5）：bootout（经桩 launchctl）
// → 逐候选调 helper 的 purge（桩 helper 记录调用）→ rm -f 清单 → 逐候选清三文件并
// 回收空目录；诱饵文件所在目录保留（无 rm -rf）；logs 与锚目录仅在空时回收；父目录
// 仅在空时回收；末尾恒 exit 0。
func TestTunUninstallScriptBehaviour(t *testing.T) {
	f := newScriptFixture(t)
	f.plantUninstallArtifacts(t)
	c1, c2, c3 := f.layout.candidates[0], f.layout.candidates[1], f.layout.candidates[2]
	decoy := filepath.Join(c2, "unrelated.txt")
	if err := os.WriteFile(decoy, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stderr := f.run(t, tunUninstallScriptFor(f.layout), "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if got := f.recordLines(t, "launchctl.args"); strings.Join(got, "|") != "bootout system/"+service.DaemonLabel {
		t.Fatalf("launchctl argv = %q (stub must be the one invoked)", got)
	}
	// 每个候选的 helper 都收到 purge 调用（记录桩按 $0 区分候选）
	purges := f.recordLines(t, "helper-args.log")
	if len(purges) != len(f.layout.candidates) {
		t.Fatalf("helper purge calls = %q, want one per candidate %v", purges, f.layout.candidates)
	}
	for i, cand := range f.layout.candidates {
		if purges[i] != filepath.Join(cand, "tradewind-service")+" purge" {
			t.Fatalf("purge call %d = %q, want %s purge", i, purges[i], filepath.Join(cand, "tradewind-service"))
		}
	}
	for _, cand := range f.layout.candidates {
		f.assertCandidateClean(t, cand)
	}
	if _, err := os.Stat(filepath.Join(c2, "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate bin dir must be reclaimed (err = %v)", err)
	}
	for _, gone := range []string{c1, c3, f.layout.plist, f.layout.corePID, f.layout.anchorSock,
		filepath.Join(c1, "service.json"), f.layout.legacy,
		filepath.Join(f.layout.logsDir, service.ServiceLogName),
		filepath.Join(f.layout.logsDir, service.CoreLogName),
		filepath.Join(f.layout.logsDir, service.CoreLogName+".1")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s must be removed (err = %v)", gone, err)
		}
	}
	if data, err := os.ReadFile(decoy); err != nil || string(data) != "keep me" {
		t.Fatalf("uninstall must not remove unrelated files (decoy err = %v)", err)
	}
	// 锚目录在 rm -f 清单 + logs 回收之后已空 → rmdir 成功（purge 桩没有真删
	// service-data，这里锚内没有它——service-data 的删除由真 helper 的 purge 负责）
	if _, err := os.Stat(f.layout.logsDir); !os.IsNotExist(err) {
		t.Fatalf("logs dir must be reclaimed when empty (err = %v)", err)
	}
	// 诱饵令第二候选非空 → 其 bin 已回收但目录保留 → 父目录同样保留
	if _, err := os.Stat(filepath.Join(f.root, "lib exec")); err != nil {
		t.Fatalf("parent dir must be kept while a child is non-empty: %v", err)
	}
}

// TestTunUninstallScriptReclaimsParentWhenEmpty 无诱饵时候选目录与可能新建的父目录
// 一并回收（仅空时成功）。
func TestTunUninstallScriptReclaimsParentWhenEmpty(t *testing.T) {
	f := newScriptFixture(t)
	f.plantUninstallArtifacts(t)

	if code, stderr := f.run(t, tunUninstallScriptFor(f.layout), ""); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	for _, gone := range append(append([]string{}, f.layout.candidates...), filepath.Join(f.root, "lib exec")) {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must be reclaimed when empty (err = %v)", gone, err)
		}
	}
}

// plantUninstallArtifacts 铺满卸载脚本要清理的全部对象（模拟已安装态 + 遗留残留）：
// plist、三份日志（含轮转出的 .1）、锚文件、锚目录 core.sock、遗留 helper。
func (f *scriptFixture) plantUninstallArtifacts(t *testing.T) {
	t.Helper()
	for _, cand := range f.layout.candidates {
		f.plant(t, cand)
	}
	for _, file := range []string{
		f.layout.plist,
		filepath.Join(f.layout.logsDir, service.ServiceLogName),
		filepath.Join(f.layout.logsDir, service.CoreLogName),
		filepath.Join(f.layout.logsDir, service.CoreLogName+".1"),
		filepath.Join(f.layout.anchorDir, "service.json"),
		f.layout.corePID,
		f.layout.anchorSock,
		f.layout.legacy,
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
