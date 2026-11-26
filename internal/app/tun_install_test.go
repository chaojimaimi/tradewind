package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
)

// stubTunClient 只覆盖服务客户端工厂缝隙（不改锚路径——需要自定义锚的用例用）。
func stubTunClient(t *testing.T, factory func(time.Duration) (tunServiceClientT, error)) {
	t.Helper()
	old := tunNewClient
	tunNewClient = factory
	t.Cleanup(func() { tunNewClient = old })
}

// pointTunAnchorAt 把锚路径缝隙指向 path。
func pointTunAnchorAt(t *testing.T, path string) {
	t.Helper()
	old := tunServiceConfigPath
	tunServiceConfigPath = path
	t.Cleanup(func() { tunServiceConfigPath = old })
}

// writeTunAnchor 写一份合法锚到 path（v3 形态，批⑥b §4.1；mutate 可改写字段）；
// 返回写入的结构。路径是词法事实——不需要真实存在（锚消费方只读字段）。
func writeTunAnchor(t *testing.T, path string, mutate func(*service.ConfigFile)) service.ConfigFile {
	t.Helper()
	base := filepath.Dir(path)
	execDir := filepath.Join(base, "exec dir")
	dataDir := filepath.Join(base, "data")
	coreHome := filepath.Join(base, "service-data")
	cfg := service.ConfigFile{
		Layout:      3,
		AllowedUIDs: []int{501},
		DataDir:     dataDir,
		DataUID:     501,
		HelperPath:  filepath.Join(execDir, service.HelperFileName),
		CoreBin:     service.CorePathIn(execDir),
		Sidecar:     service.SidecarPathIn(execDir),
		CoreHome:    coreHome,
		KernelSock:  service.KernelSockIn(coreHome),
		BrokerSock:  filepath.Join(base, "core.sock"),
		CoreLog:     filepath.Join(base, "logs", service.CoreLogName),
		UserRuntime: filepath.Join(dataDir, "runtime.yaml"),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedFakeCore 在数据目录放一个假内核（摘要绑定需要 home.Bin() 可读；编排测试
// 绝不真实提权——这份假内核只喂给 sha256SumFile）。
func seedFakeCore(t *testing.T, home paths.Home) {
	t.Helper()
	writeFileT(t, home.Bin(), "fake mihomo core (digest-binding fixture)")
}

// --- 提权执行与失败诊断 ---

// TestAppleScriptLiteralEscapesGolden 字面量转义 golden（批⑥b §4.5；P2 实测规则）：
// `\`→`\\`、`"`→`\"`、换行→`\n`、回车→`\r`，其余逐字节保留，两端加引号。
func TestAppleScriptLiteralEscapesGolden(t *testing.T) {
	in := "a\\b\"c\nd\ré"
	want := `"a\\b\"c\nd\ré"` // 原始串：两端引号 + 转义后的两字符序列
	if got := appleScriptLiteral(in); got != want {
		t.Fatalf("appleScriptLiteral(%q) = %q, want %q", in, got, want)
	}
	if got := appleScriptLiteral("plain"); got != `"plain"` {
		t.Fatalf("plain literal = %q, want quoted passthrough", got)
	}
}

// TestNewOsaCommandStdinDelivery osascript 的 argv 恰为 ["osascript"]（无 -e、无
// 文件参数——脚本文本不进 argv，ps 不可见），AppleScript 经标准输入交付且内容
// 恰为组装的语句（经 *strings.Reader 可回读断言，不消耗读取位置）。
func TestNewOsaCommandStdinDelivery(t *testing.T) {
	appleScript := "do shell script \"echo hi\" with administrator privileges"
	cmd := newOsaCommand(context.Background(), appleScript)
	if len(cmd.Args) != 1 || cmd.Args[0] != "osascript" {
		t.Fatalf("cmd.Args = %q, want exactly [osascript] (no script text in argv)", cmd.Args)
	}
	r, ok := cmd.Stdin.(*strings.Reader)
	if !ok {
		t.Fatalf("cmd.Stdin = %T, want *strings.Reader", cmd.Stdin)
	}
	got := make([]byte, r.Len())
	if n, err := r.ReadAt(got, 0); n != len(got) || err != nil && err != io.EOF {
		t.Fatalf("read stdin back: n=%d err=%v", n, err)
	}
	if string(got) != appleScript {
		t.Fatalf("stdin = %q, want the assembled AppleScript %q", got, appleScript)
	}
}

// TestRunElevatedScriptPassesInlineSource 脚本源码内联进 `do shell script` 字面量
// （批⑥b §4.5 I5：不再落盘、不再套 /bin/sh '<path>' 一层）。
func TestRunElevatedScriptPassesInlineSource(t *testing.T) {
	source := "#!/bin/sh\necho \"hi from $(id -u)\"\nexit 0\n"
	var got []string
	stubTunOsaScript(t, func(_ context.Context, appleScript string) (string, string, error) {
		got = append(got, appleScript)
		return "", "", nil
	})
	if err := runElevatedScript(source); err != nil {
		t.Fatalf("runElevatedScript: %v", err)
	}
	want := `do shell script "#!/bin/sh\necho \"hi from $(id -u)\"\nexit 0\n" with administrator privileges`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("osascript input = %q, want the inline-source statement %q", got, want)
	}
	if strings.Contains(got[0], "/bin/sh '") {
		t.Fatalf("the old /bin/sh '<path>' wrapper must be gone:\n%s", got[0])
	}
}

func stubTunOsaScript(t *testing.T, fn func(context.Context, string) (string, string, error)) {
	t.Helper()
	old := runOsaScript
	runOsaScript = fn
	t.Cleanup(func() { runOsaScript = old })
}

// TestTunRunElevatedNeverTouchesDisk tunRunElevated 不落盘（批⑥b §4.5 I5）：桩
// elevate 捕获到的参数就是脚本源码本身；执行之后用户数据目录里没有
// tun-install-script.sh（对目录列表断言）。
func TestTunRunElevatedNeverTouchesDisk(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	scriptBody := "#!/bin/sh\necho elevated\n"
	var captured string
	env := tunEnv{elevate: func(script string) error { captured = script; return nil }}
	if err := tunRunElevated(env, scriptBody); err != nil {
		t.Fatalf("tunRunElevated: %v", err)
	}
	if captured != scriptBody {
		t.Fatalf("elevate received %q, want the script source verbatim", captured)
	}
	entries, err := os.ReadDir(home.String())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "tun-install-script") {
			t.Fatalf("the elevated script must not be written to the user data dir, found %s", e.Name())
		}
	}
}

// TestTunInstallWithRequiresDigests 摘要绑定（批⑥b §4.5）：内核缺失 → 报错、不
// 生成脚本、不提权（fail-closed——坏摘要不许进脚本）。
func TestTunInstallWithRequiresDigests(t *testing.T) {
	home := paths.Home(stubSocketDir(t)) // 未种 bin/mihomo
	env, _, _, _, _ := newTunTestEnv(t)
	env.serviceAnchorExists = func() bool { return false } // 未安装：走到生成脚本一步
	elevated := 0
	env.elevate = func(string) error { elevated++; return nil }

	err := tunInstallWith(home, env, false)
	if err == nil || !strings.Contains(err.Error(), "摘要") {
		t.Fatalf("err = %v, want the digest failure", err)
	}
	if elevated != 0 {
		t.Fatalf("elevation runs = %d, want 0 (no script may be generated)", elevated)
	}
}

// TestTunInstallWithEmbedsDigests 生成的脚本嵌入命令开始时算出的摘要（批⑥b §4.5）。
// 确认旗标已随联锁开放一并移除（批⑥b 阶段二）：脚本绝不再携带 --allow-insecure-tun。
func TestTunInstallWithEmbedsDigests(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedFakeCore(t, home)
	env, _, _, _, _ := newTunTestEnv(t)
	env.serviceAnchorExists = func() bool { return false }
	script := captureInstallScript(&env)

	// 独立实现算期望摘要（不经被测的 sha256SumFile，避免同源掩盖失配）
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digestOf := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	cliSHA, coreSHA := digestOf(exe), digestOf(home.Bin())

	if err := tunInstallWith(home, env, false); err != nil {
		t.Fatalf("tunInstallWith: %v", err)
	}
	for _, want := range []string{"CLI_SHA='" + cliSHA + "'", "CORE_SHA='" + coreSHA + "'"} {
		if !strings.Contains(*script, want) {
			t.Fatalf("script missing %q:\n%s", want, *script)
		}
	}
	if strings.Contains(*script, "--allow-insecure-tun") {
		t.Fatalf("the acknowledgement flag must be gone (TUN is open):\n%s", *script)
	}
	wantLine := `"$EXE/tradewind-service" install --data-dir "$DATA" --data-uid "$DATA_UID" --exec-dir "$EXE"`
	if !strings.Contains(*script, wantLine) {
		t.Fatalf("script missing the helper install line %q:\n%s", wantLine, *script)
	}
}

// TestRunElevatedScriptSurfacesOsaScriptStderr osascript stderr 透传（真机首测：
// 根因在脚本的 stderr 里，CLI 丢弃后只剩 "exit status 1"）。
func TestRunElevatedScriptSurfacesOsaScriptStderr(t *testing.T) {
	stubTunOsaScript(t, func(context.Context, string) (string, string, error) {
		return "", "sh: tradewind: 所有候选安装位置均被 macOS 系统策略拦截\n number 3", errors.New("exit status 1")
	})
	err := runElevatedScript("#!/bin/sh\necho x\n")
	if err == nil || !strings.Contains(err.Error(), "系统策略拦截") || !strings.Contains(err.Error(), "number 3") {
		t.Fatalf("err = %v, want the script's stderr surfaced", err)
	}
}

// TestElevatedRunErrorClassifiesOutcomes 三分类：用户取消弹窗（-128）／超时／脚本
// 执行失败（带 stdout+stderr）。
func TestElevatedRunErrorClassifiesOutcomes(t *testing.T) {
	cancel := elevatedRunError(nil, "", "execution error: 用户已取消。(-128)", errors.New("exit status 1")).Error()
	if !strings.Contains(cancel, "已取消管理员授权弹窗") || strings.Contains(cancel, "脚本执行失败") {
		t.Fatalf("cancel = %q", cancel)
	}
	timeout := elevatedRunError(context.DeadlineExceeded, "", "", errors.New("signal: killed")).Error()
	if !strings.Contains(timeout, "提权执行超时") || strings.Contains(timeout, "signal: killed") {
		t.Fatalf("timeout = %q", timeout)
	}
	failure := elevatedRunError(nil, "partial stdout", "sh: line 1: cannot create", errors.New("exit status 1")).Error()
	for _, want := range []string{"提权脚本执行失败", "cannot create", "partial stdout", "exit status 1"} {
		if !strings.Contains(failure, want) {
			t.Fatalf("failure = %q, want it to carry %q", failure, want)
		}
	}
}

// TestElevatedOutputSnippetTruncatesOnRuneBoundary 输出摘要截断到 2000 字节且不
// 切断多字节字符；stderr 在前；空输出为空串。
func TestElevatedOutputSnippetTruncatesOnRuneBoundary(t *testing.T) {
	if got := elevatedOutputSnippet("out", "err"); got != "err\nout" {
		t.Fatalf("snippet = %q, want stderr first", got)
	}
	if got := elevatedOutputSnippet("", "  \n"); got != "" {
		t.Fatalf("empty snippet = %q", got)
	}
	got := elevatedOutputSnippet("", strings.Repeat("错", 2000)) // 6000 字节
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…(已截断)") ||
		len(got) > elevatedOutputMaxBytes+len("\n…(已截断)") {
		t.Fatalf("snippet must be rune-aligned and capped: valid=%v len=%d", utf8.ValidString(got), len(got))
	}
	if exact := elevatedOutputSnippet("", strings.Repeat("a", elevatedOutputMaxBytes)); len(exact) != elevatedOutputMaxBytes {
		t.Fatalf("output at the cap must be kept verbatim, len = %d", len(exact))
	}
}

// --- 安装验证 ---

// TestTunWaitForServiceReady 安装验证：服务可达即通过；窗口内不可达 → 报错附服务
// 日志尾部（可读时）+ 系统日志排查提示；日志不可读时只有提示。
func TestTunWaitForServiceReady(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		stubTunClient(t, func(time.Duration) (tunServiceClientT, error) {
			return &fakeTunService{}, nil
		})
		if err := tunWaitForServiceReady(); err != nil {
			t.Fatalf("tunWaitForServiceReady: %v", err)
		}
	})
	t.Run("unreachable with readable service log", func(t *testing.T) {
		stubTunClient(t, func(time.Duration) (tunServiceClientT, error) {
			return nil, errors.New("dial unix: no such file or directory")
		})
		shortenTunVerifyWindow(t)
		logPath := filepath.Join(t.TempDir(), "tradewind-service.log")
		writeFileT(t, logPath, "service: listening on /var/run/tradewind-service.sock\npanic: boom\n")
		oldLog := tunDaemonLogPath
		tunDaemonLogPath = logPath
		t.Cleanup(func() { tunDaemonLogPath = oldLog })

		err := tunWaitForServiceReady()
		if err == nil {
			t.Fatal("unreachable service must fail the install verification")
		}
		for _, want := range []string{"不可达", "AppleSystemPolicy", "log show --last 2m", "panic: boom"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to carry %q", err, want)
			}
		}
	})
	t.Run("unreachable without readable service log", func(t *testing.T) {
		stubTunClient(t, func(time.Duration) (tunServiceClientT, error) { return nil, errors.New("refused") })
		shortenTunVerifyWindow(t)
		oldLog := tunDaemonLogPath
		tunDaemonLogPath = filepath.Join(t.TempDir(), "absent.log")
		t.Cleanup(func() { tunDaemonLogPath = oldLog })

		err := tunWaitForServiceReady()
		if err == nil || !strings.Contains(err.Error(), "不可达") || strings.Contains(err.Error(), "服务日志尾部") {
			t.Fatalf("err = %v, want hint without a log tail section", err)
		}
	})
}

func shortenTunVerifyWindow(t *testing.T) {
	t.Helper()
	old := tunInstallVerifyWindow
	tunInstallVerifyWindow = 0
	t.Cleanup(func() { tunInstallVerifyWindow = old })
}

// TestTunServiceReachable 可达性探测：status 成功 = 可达；客户端构造失败或请求
// 失败 = 不可达。
func TestTunServiceReachable(t *testing.T) {
	stubTunClient(t, func(time.Duration) (tunServiceClientT, error) {
		return &fakeTunService{}, nil
	})
	if !tunServiceReachable() {
		t.Fatal("responding service must be reachable")
	}
	stubTunClient(t, func(time.Duration) (tunServiceClientT, error) {
		return nil, errors.New("not installed")
	})
	if tunServiceReachable() {
		t.Fatal("unconstructible client must be unreachable")
	}
	stubTunClient(t, func(time.Duration) (tunServiceClientT, error) {
		return failingStatusClient{}, nil
	})
	if tunServiceReachable() {
		t.Fatal("failing status request must be unreachable")
	}
}

// failingStatusClient 恒失败的 status（transport 层错误）。
type failingStatusClient struct{}

func (failingStatusClient) Start(int) (service.Response, error) {
	return service.Response{}, errors.New("down")
}
func (failingStatusClient) Stop() (service.Response, error) {
	return service.Response{}, errors.New("down")
}
func (failingStatusClient) Status() (service.Response, error) {
	return service.Response{}, errors.New("dial: refused")
}
func (failingStatusClient) Reload() (service.Response, error) {
	return service.Response{}, errors.New("down")
}

// TestTunDaemonLogPathDefault 批⑥b §4.6：服务日志默认落在 root 属主锚目录的
// logs/ 下、属主为数据目录用户（迁出 admin 可写的 /Library/Logs，R22 闭环）。
func TestTunDaemonLogPathDefault(t *testing.T) {
	want := filepath.Join(service.DefaultLogsDir, service.ServiceLogName)
	if tunDaemonLogPath != want {
		t.Fatalf("tunDaemonLogPath = %q, want %q", tunDaemonLogPath, want)
	}
}

// --- 两项陈旧检测 ---

// TestTunBinaryStaleness 陈旧检测矩阵（锚 helper_path/core_bin 为比对来源）：
// 两侧一致 = 最新；self-update（CLI 变）→ helper 陈旧；self-update --core（数据
// 目录内核变）→ 内核副本陈旧；锚缺 helper_path / 旧布局锚（被 validate 拒）/ 锚
// 不可读 → 一律按陈旧处理（走 reinstall，绝不放行）。
func TestTunBinaryStaleness(t *testing.T) {
	dir := t.TempDir()
	anchorPath := filepath.Join(dir, "service.json")
	cfg := writeTunAnchor(t, anchorPath, nil)
	pointTunAnchorAt(t, anchorPath)
	cli := filepath.Join(dir, "current-cli")
	home := paths.Home(cfg.DataDir)
	writeFileT(t, cli, "CLI-BYTES")
	writeFileT(t, cfg.HelperPath, "CLI-BYTES")
	writeFileT(t, home.Bin(), "CORE-BYTES")
	writeFileT(t, cfg.CoreBin, "CORE-BYTES")

	if !helperBinaryMatchesCLI(cli) || !coreBinaryMatches(home) {
		t.Fatal("identical helper and core copy must read as current")
	}
	// self-update --core：数据目录内核更新，root 副本停在旧版
	writeFileT(t, home.Bin(), "CORE-NEW")
	if coreBinaryMatches(home) || !helperBinaryMatchesCLI(cli) {
		t.Fatal("diverged core copy must read as stale while helper stays current")
	}
	// self-update：CLI 更新，安装位 helper 停在旧版
	writeFileT(t, cli, "CLI-NEW")
	writeFileT(t, home.Bin(), "CORE-BYTES")
	if helperBinaryMatchesCLI(cli) || !coreBinaryMatches(home) {
		t.Fatal("diverged helper must read as stale while core copy is current again")
	}
	// 锚缺 helper_path（旧锚）→ helper 陈旧
	writeTunAnchor(t, anchorPath, func(c *service.ConfigFile) { c.HelperPath = "" })
	if helperBinaryMatchesCLI(cli) {
		t.Fatal("anchor without helper_path must read as stale helper")
	}
	// 旧布局锚（core_bin 在用户可写的数据目录内）→ validate 拒绝 → 两项皆陈旧
	writeTunAnchor(t, anchorPath, func(c *service.ConfigFile) {
		c.CoreBin = filepath.Join(c.DataDir, "bin", "mihomo")
		c.Sidecar = filepath.Join(c.DataDir, "bin", "mihomo.sha256")
	})
	if helperBinaryMatchesCLI(cli) || coreBinaryMatches(home) {
		t.Fatal("legacy-layout anchor must never read as current")
	}
	// root 副本文件缺失（被删）→ 陈旧
	writeTunAnchor(t, anchorPath, nil)
	if err := os.Remove(cfg.CoreBin); err != nil {
		t.Fatal(err)
	}
	if coreBinaryMatches(home) {
		t.Fatal("missing root core copy must read as stale")
	}
	// 锚不可读 → 两项皆陈旧
	pointTunAnchorAt(t, filepath.Join(dir, "absent.json"))
	if helperBinaryMatchesCLI(cli) || coreBinaryMatches(home) {
		t.Fatal("unreadable anchor must read as stale")
	}
}

// TestTunStatusLinesShowBothStalenessItems `tun status` 分别显示两项状态；哪项
// 陈旧才出哪条警告（内核副本陈旧不得误报 helper 陈旧）。
func TestTunStatusLinesShowBothStalenessItems(t *testing.T) {
	dir := t.TempDir()
	anchorPath := filepath.Join(dir, "service.json")
	cfg := writeTunAnchor(t, anchorPath, nil)
	pointTunAnchorAt(t, anchorPath)
	home := paths.Home(cfg.DataDir)
	cli := filepath.Join(dir, "current-cli") // helper 比对基准（可注入形态）
	writeFileT(t, cli, "CLI-BYTES")
	writeFileT(t, cfg.HelperPath, "CLI-BYTES")
	writeFileT(t, home.Bin(), "CORE-NEW")
	writeFileT(t, cfg.CoreBin, "CORE-OLD")

	joined := strings.Join(tunBinaryStatusLinesFor(home, cli), "\n")
	for _, want := range []string{"helper:     up to date", "core copy: stale", "服务内核副本已陈旧"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("status missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "服务二进制已陈旧") {
		t.Fatalf("a stale core copy must not also flag the helper:\n%s", joined)
	}
	// 两项都最新：无任何 reinstall 警告
	writeFileT(t, cfg.CoreBin, "CORE-NEW")
	joined = strings.Join(tunBinaryStatusLinesFor(home, cli), "\n")
	if strings.Contains(joined, "reinstall") || !strings.Contains(joined, "core copy: up to date") {
		t.Fatalf("current install must show no warning:\n%s", joined)
	}
}

// --- 编排闸门 ---

// TestTunOnAbortsWhenServiceUnreachable 服务已装但不可达 → 在杀旧 start **之前**
// 报错（真机 fold-in：否则白折腾一次用户态 core 重启）；settings 不翻转、零动作。
func TestTunOnAbortsWhenServiceUnreachable(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, stopStarts, _ := newTunTestEnv(t)
	env.serviceReachable = func() bool { return false }

	err := tunOnWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "tradewind tun status") {
		t.Fatalf("err = %v, want unreachable-service guidance", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("settings must stay untouched when the service is unreachable")
	}
	if *spawns != 0 || *stopStarts != 0 || svc.stopCount() != 0 {
		t.Fatalf("unreachable service must abort before killing the old start: spawns=%d stopStarts=%d stops=%d",
			*spawns, *stopStarts, svc.stopCount())
	}
}

// TestTunOnCoreCopyStaleAborts root 内核副本陈旧 → 报错指引 reinstall、零动作。
func TestTunOnCoreCopyStaleAborts(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	env, svc, spawns, _, _ := newTunTestEnv(t)
	env.coreMatchesCurrent = func(paths.Home) bool { return false }

	err := tunOnWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "reinstall") || !strings.Contains(err.Error(), "内核副本") {
		t.Fatalf("err = %v, want stale-core-copy reinstall guidance", err)
	}
	if tunSettings(t, home).TunEnabled || *spawns != 0 || svc.stopCount() != 0 {
		t.Fatal("stale core copy must abort before any action")
	}
}

// TestTunInstallVerificationFailureAbortsTunOn 提权成功 ≠ 安装成功：安装验证失败
// → 报错（带诊断），settings 不翻转、不 spawn。
func TestTunInstallVerificationFailureAbortsTunOn(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	seedFakeCore(t, home) // 前置安装要算摘要（批⑥b §4.5）
	env, _, spawns, _, _ := newTunTestEnv(t)
	var installed bool
	env.serviceAnchorExists = func() bool { return installed }
	env.elevate = func(string) error { installed = true; return nil }
	env.waitForServiceReady = func() error {
		return errors.New("app: 特权服务安装后 12s 内不可达；服务日志尾部:\nASP: Security policy would not allow process")
	}

	err := tunOnWith(context.Background(), home, env)
	if err == nil || (!strings.Contains(err.Error(), "不可达") && !strings.Contains(err.Error(), "AppleSystemPolicy")) {
		t.Fatalf("err = %v, want the install-verification diagnostics", err)
	}
	if tunSettings(t, home).TunEnabled || *spawns != 0 {
		t.Fatal("failed install verification must abort before flipping settings / spawning")
	}
}

// TestTunInstallElevatesWhenCoreCopyStale `tun install` 幂等须两项皆最新：仅 helper
// 最新、内核副本陈旧 → 仍走提权重装 + 安装验证。
func TestTunInstallElevatesWhenCoreCopyStale(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedFakeCore(t, home) // 生成脚本要算内核摘要（批⑥b §4.5）
	env, _, _, _, _ := newTunTestEnv(t)
	env.coreMatchesCurrent = func(paths.Home) bool { return false }
	elevated, verified := 0, 0
	env.elevate = func(string) error { elevated++; return nil }
	env.waitForServiceReady = func() error { verified++; return nil }

	if err := tunInstallWith(home, env, false); err != nil {
		t.Fatalf("tunInstallWith: %v", err)
	}
	if elevated != 1 || verified != 1 {
		t.Fatalf("stale core copy must trigger reinstall + verification: elevated=%d verified=%d", elevated, verified)
	}
}

// TestTunCoreStaleReminder self-update --core 后的提醒：服务锚存在 → 一行 reinstall
// 提示；未安装 → 静默。
func TestTunCoreStaleReminder(t *testing.T) {
	quiet := &strings.Builder{}
	pointTunAnchorAt(t, filepath.Join(t.TempDir(), "absent.json"))
	tunCoreStaleReminder(quiet)
	if quiet.String() != "" {
		t.Fatalf("no reminder without the privileged service: %q", quiet.String())
	}
	anchorPath := filepath.Join(t.TempDir(), "service.json")
	writeTunAnchor(t, anchorPath, nil)
	pointTunAnchorAt(t, anchorPath)
	loud := &strings.Builder{}
	tunCoreStaleReminder(loud)
	if !strings.Contains(loud.String(), "tradewind tun reinstall") || strings.Count(loud.String(), "\n") != 1 {
		t.Fatalf("reminder = %q, want exactly one line naming `tradewind tun reinstall`", loud.String())
	}
}
