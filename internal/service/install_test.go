package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLaunchctl 记录 launchctl 调用（绝不触碰真实 launchd）。
type fakeLaunchctl struct {
	calls [][]string
}

func (f *fakeLaunchctl) Run(args ...string) error {
	f.calls = append(f.calls, args)
	return nil
}

// newInstallPlan 造安装落点（全部临时目录——绝不触碰真实系统目录）。execDir 含
// 空格：路径拼装须逐段引用安全（生产第一候选 /Library/Application Support 同形）。
func newInstallPlan(t *testing.T) installPlan {
	t.Helper()
	base := shortTempDir(t)
	return installPlan{
		dataDir:          filepath.Join(base, "data"),
		dataUID:          501,
		execDir:          filepath.Join(base, "exec dir"),
		configDir:        filepath.Join(base, "support"),
		launchDaemonsDir: filepath.Join(base, "LaunchDaemons"),
		logsDir:          filepath.Join(base, "Logs"),
	}
}

// seedRootCoreCopy 在执行目录内放置假内核副本 + sidecar（sidecarHex 非空 = 写给定
// 摘要，失配用例；空 = 写匹配摘要）。
func seedRootCoreCopy(t *testing.T, p installPlan, sidecarHex string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(p.execDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	core := []byte("fake root core copy")
	if err := os.WriteFile(CorePathIn(p.execDir), core, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(core))
	if sidecarHex != "" {
		sum = sidecarHex
	}
	if err := os.WriteFile(SidecarPathIn(p.execDir), []byte(sum+"  "+CorePathIn(p.execDir)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readWrittenAnchor 读回落盘的 service.json。
func readWrittenAnchor(t *testing.T, p installPlan) (ConfigFile, os.FileMode) {
	t.Helper()
	cfgPath := filepath.Join(p.configDir, "service.json")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat service.json: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var got ConfigFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse service.json: %v", err)
	}
	return got, info.Mode().Perm()
}

// TestInstallDaemonWritesRootOwnedLayout helper install 新布局（batch6 真机
// fold-in）：service.json 的 helper_path/core_bin/sidecar 全部指向执行目录（root
// 属主副本），runtime/sock/log 留在数据目录；plist ProgramArguments 用 helper_path
// （不再是遗留位置）；launchctl 序 = bootout → bootstrap。
func TestInstallDaemonWritesRootOwnedLayout(t *testing.T) {
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, "")
	lc := &fakeLaunchctl{}

	if err := installDaemon(p, lc, &strings.Builder{}); err != nil {
		t.Fatalf("installDaemon: %v", err)
	}
	got, perm := readWrittenAnchor(t, p)
	if perm != 0o644 {
		t.Fatalf("service.json perm = %o, want 644", perm)
	}
	want := ConfigFile{
		AllowedUIDs: []int{501},
		DataDir:     p.dataDir,
		HelperPath:  HelperPathIn(p.execDir),
		CoreBin:     CorePathIn(p.execDir),
		Sidecar:     SidecarPathIn(p.execDir),
		Runtime:     filepath.Join(p.dataDir, "runtime.yaml"),
		Sock:        filepath.Join(p.dataDir, "core.sock"),
		Log:         filepath.Join(p.dataDir, "logs", "core-service.log"),
		DataUID:     501,
	}
	if len(got.AllowedUIDs) != 1 || got.AllowedUIDs[0] != 501 {
		t.Fatalf("allowed_uids = %v, want [501]", got.AllowedUIDs)
	}
	// ConfigFile 含 []int 不可整体 ==，逐字段比对
	if got.DataDir != want.DataDir || got.HelperPath != want.HelperPath || got.CoreBin != want.CoreBin ||
		got.Sidecar != want.Sidecar || got.Runtime != want.Runtime || got.Sock != want.Sock ||
		got.Log != want.Log || got.DataUID != want.DataUID {
		t.Fatalf("service.json = %+v\nwant            %+v", got, want)
	}
	// LPE 修复锚：root 执行的内核副本绝不在用户可写的数据目录之内
	if pathWithin(got.CoreBin, got.DataDir) {
		t.Fatalf("core_bin %q must be outside data_dir %q", got.CoreBin, got.DataDir)
	}
	if err := got.validate(); err != nil {
		t.Fatalf("written anchor must pass the service startup gate: %v", err)
	}

	plist, err := os.ReadFile(filepath.Join(p.launchDaemonsDir, DaemonLabel+".plist"))
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	text := string(plist)
	for _, want := range []string{
		"<string>" + DaemonLabel + "</string>",
		"<string>" + want.HelperPath + "</string>",
		"<string>service</string>",
		"<key>KeepAlive</key>",
		"<string>" + filepath.Join(p.logsDir, "tradewind-service.log") + "</string>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, HelperInstallPath) {
		t.Fatalf("plist must not reference the legacy helper location:\n%s", text)
	}

	plistPath := filepath.Join(p.launchDaemonsDir, DaemonLabel+".plist")
	if len(lc.calls) != 2 ||
		strings.Join(lc.calls[0], " ") != "bootout system/"+DaemonLabel ||
		strings.Join(lc.calls[1], " ") != "bootstrap system "+plistPath {
		t.Fatalf("launchctl calls = %v, want [bootout system/%s] then [bootstrap system %s]", lc.calls, DaemonLabel, plistPath)
	}
}

// TestInstallRejectsExecDirInsideDataDir LPE 纵深防御（Install 侧）：exec_dir 落在
// 数据目录内 → 拒绝，不写任何文件、不碰 launchd。
func TestInstallRejectsExecDirInsideDataDir(t *testing.T) {
	p := newInstallPlan(t)
	p.execDir = filepath.Join(p.dataDir, "exec")
	seedRootCoreCopy(t, p, "")
	lc := &fakeLaunchctl{}

	err := installDaemon(p, lc, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "inside data_dir") {
		t.Fatalf("err = %v, want exec_dir-inside-data_dir rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(p.configDir, "service.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("service.json must not be written on rejection (err = %v)", statErr)
	}
	if len(lc.calls) != 0 {
		t.Fatalf("launchctl must not be touched on rejection: %v", lc.calls)
	}
}

// TestInstallRejectsCoreCopyMismatch 写锚前校验 root 副本一致性：sidecar 与副本
// 不符 → 拒绝，不写锚、不碰 launchd。
func TestInstallRejectsCoreCopyMismatch(t *testing.T) {
	p := newInstallPlan(t)
	seedRootCoreCopy(t, p, strings.Repeat("0", 64))
	lc := &fakeLaunchctl{}

	err := installDaemon(p, lc, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("err = %v, want root copy verification failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(p.configDir, "service.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("service.json must not be written on verification failure (err = %v)", statErr)
	}
	if len(lc.calls) != 0 {
		t.Fatalf("launchctl must not be touched on verification failure: %v", lc.calls)
	}
}

// TestInstallRejectsMissingRootCopy root 副本缺失（脚本自测后被人删掉等）→ 拒绝。
func TestInstallRejectsMissingRootCopy(t *testing.T) {
	p := newInstallPlan(t) // 未放置副本
	lc := &fakeLaunchctl{}

	err := installDaemon(p, lc, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("err = %v, want verification failure for missing root copy", err)
	}
	if len(lc.calls) != 0 {
		t.Fatalf("launchctl must not be touched: %v", lc.calls)
	}
}

// TestInstallRejectsRelativeExecDir 相对 exec_dir → 拒绝（root 下的相对路径随
// 工作目录漂移，属注入面）。
func TestInstallRejectsRelativeExecDir(t *testing.T) {
	p := newInstallPlan(t)
	p.execDir = "relative/exec"
	seedRootCoreCopy(t, p, "")
	if err := installDaemon(p, &fakeLaunchctl{}, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "exec_dir") {
		t.Fatalf("err = %v, want relative exec_dir rejection", err)
	}
}

// TestConfigValidateRejectsCoreBinInsideDataDir LPE 纵深防御（服务启动闸）：旧布局
// 锚（core_bin 指向 data_dir/bin）被 validate 拒绝 → LoadConfigFile 报错 → 服务
// exit 1 fail-closed（launchd 节流重试，用户须 tun reinstall），绝不带病运行。
func TestConfigValidateRejectsCoreBinInsideDataDir(t *testing.T) {
	dir := shortTempDir(t)
	legacy := ConfigFile{
		AllowedUIDs: []int{501},
		DataDir:     dir,
		CoreBin:     filepath.Join(dir, "bin", "mihomo"),
		Sidecar:     filepath.Join(dir, "bin", "mihomo.sha256"),
		Runtime:     filepath.Join(dir, "runtime.yaml"),
		Sock:        filepath.Join(dir, "core.sock"),
		Log:         filepath.Join(dir, "logs", "core-service.log"),
		DataUID:     501,
	}
	err := legacy.validate()
	if err == nil || !strings.Contains(err.Error(), "core_bin must not live inside data_dir") {
		t.Fatalf("legacy-layout anchor validate err = %v, want LPE rejection", err)
	}
	if err := writeJSONFile(filepath.Join(dir, "service.json"), legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(filepath.Join(dir, "service.json")); err == nil {
		t.Fatal("service startup gate must reject the legacy-layout anchor (fail-closed)")
	}
	// 对照：同结构仅 core_bin/sidecar 移到数据目录外 → 通过
	fixed := legacy
	fixed.CoreBin = filepath.Join(dir, "..", "exec", "bin", "mihomo")
	fixed.Sidecar = filepath.Join(dir, "..", "exec", "bin", "mihomo.sha256")
	if err := fixed.validate(); err != nil {
		t.Fatalf("exec-dir layout must pass validate: %v", err)
	}
}

// TestPathWithin 词法包含判断矩阵：自身/子路径在内；兄弟前缀、.. 逃逸在外。
func TestPathWithin(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c/d", "/a/b", true},
		{"/a/b/c/d", "/a/b/", true},
		{"/a/bc/d", "/a/b", false}, // 兄弟前缀不算包含
		{"/a/c", "/a/b", false},
		{"/a", "/a/b", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.child, c.parent); got != c.want {
			t.Fatalf("pathWithin(%q, %q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}

// TestExecDirCandidatesOrderAndPaths 候选顺序（锚目录优先 → libexec → /opt）与
// 执行目录内落点派生；候选含空格路径的派生不引入分隔符歧义。
func TestExecDirCandidatesOrderAndPaths(t *testing.T) {
	got := ExecDirCandidates()
	want := []string{DefaultConfigDir, "/usr/local/libexec/tradewind", "/opt/tradewind"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ExecDirCandidates = %v, want %v", got, want)
	}
	// 返回副本：调用方改动不污染后续调用
	got[0] = "/tampered"
	if ExecDirCandidates()[0] != DefaultConfigDir {
		t.Fatal("ExecDirCandidates must return a fresh copy")
	}
	if HelperPathIn("/x y") != "/x y"+string(filepath.Separator)+HelperFileName {
		t.Fatalf("HelperPathIn = %q", HelperPathIn("/x y"))
	}
	if CorePathIn("/x") != "/x/bin/mihomo" || SidecarPathIn("/x") != "/x/bin/mihomo.sha256" {
		t.Fatalf("CorePathIn/SidecarPathIn = %q / %q", CorePathIn("/x"), SidecarPathIn("/x"))
	}
}

// TestDaemonPlistRejectsXMLChars plist 注入面防御：含 XML 特殊字符的路径拒绝
// 生成（autostart.launchdPlist 同款守卫先例）。
func TestDaemonPlistRejectsXMLChars(t *testing.T) {
	if _, err := daemonPlist("/evil<bin>", "/Library/Logs/tradewind-service.log"); err == nil {
		t.Fatal("xml-special helper path: want error")
	}
	if _, err := daemonPlist(HelperPathIn("/x"), "/Library/Logs/a&b.log"); err == nil {
		t.Fatal("xml-special log path: want error")
	}
}
