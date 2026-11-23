package app

import (
	"context"
	"encoding/json"
	"errors"
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

// writeTunAnchor 写一份合法锚到 path（mutate 可改写字段）；返回写入的结构。
func writeTunAnchor(t *testing.T, path string, mutate func(*service.ConfigFile)) service.ConfigFile {
	t.Helper()
	base := filepath.Dir(path)
	cfg := service.ConfigFile{
		AllowedUIDs: []int{501},
		DataDir:     filepath.Join(base, "data"),
		HelperPath:  filepath.Join(base, "exec dir", service.HelperFileName),
		CoreBin:     service.CorePathIn(filepath.Join(base, "exec dir")),
		Sidecar:     service.SidecarPathIn(filepath.Join(base, "exec dir")),
		Runtime:     filepath.Join(base, "data", "runtime.yaml"),
		Sock:        filepath.Join(base, "data", "core.sock"),
		Log:         filepath.Join(base, "data", "logs", "core-service.log"),
		DataUID:     501,
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

// --- 提权执行与失败诊断 ---

// TestRunElevatedScriptPassesOnlyScriptPath osascript 的 AppleScript 片段只携带
// 脚本路径（密码红线：密码不落盘/不落 argv/不落日志）；路径含空格与数据目录同形。
func TestRunElevatedScriptPassesOnlyScriptPath(t *testing.T) {
	scriptPath := "/Users/le e/Library/Application Support/tradewind/tun-install-script.sh"
	var got []string
	stubTunOsaScript(t, func(_ context.Context, appleScript string) (string, string, error) {
		got = append(got, appleScript)
		return "", "", nil
	})
	if err := runElevatedScript(scriptPath); err != nil {
		t.Fatalf("runElevatedScript: %v", err)
	}
	want := `do shell script "/bin/sh '/Users/le e/Library/Application Support/tradewind/tun-install-script.sh'" with administrator privileges`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("osascript args = %q, want the single script-path-bearing statement %q", got, want)
	}
}

func stubTunOsaScript(t *testing.T, fn func(context.Context, string) (string, string, error)) {
	t.Helper()
	old := runOsaScript
	runOsaScript = fn
	t.Cleanup(func() { runOsaScript = old })
}

// TestRunElevatedScriptSurfacesOsaScriptStderr osascript stderr 透传（真机首测：
// 根因在脚本的 stderr 里，CLI 丢弃后只剩 "exit status 1"）。
func TestRunElevatedScriptSurfacesOsaScriptStderr(t *testing.T) {
	stubTunOsaScript(t, func(context.Context, string) (string, string, error) {
		return "", "sh: tradewind: 所有候选安装位置均被 macOS 系统策略拦截\n number 3", errors.New("exit status 1")
	})
	err := runElevatedScript("/tmp/tw/script.sh")
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
