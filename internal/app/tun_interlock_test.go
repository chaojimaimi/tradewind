package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
)

// TUN 特权服务联锁（tun_interlock.go）：tun on / install / reinstall 默认拒绝，显式
// 确认风险（TRADEDWIND_ALLOW_INSECURE_TUN=1）才放行；tun off / status / uninstall
// 不受约束。公开入口自带生产环境（productionTunEnv），下面的沙箱把它会触达的系统
// 缝隙全部指向测试桩或临时路径——绝不触真服务 socket / launchd / 提权 / 子进程。

// stubTunInterlock 覆盖联锁确认环境变量的读取缝隙（测试不依赖进程真实环境）。
func stubTunInterlock(t *testing.T, value string, set bool) {
	t.Helper()
	old := tunInterlockEnv
	tunInterlockEnv = func() (string, bool) { return value, set }
	t.Cleanup(func() { tunInterlockEnv = old })
}

// allowInsecureTun 显式确认风险（公开入口的放行形态）。
func allowInsecureTun(t *testing.T) {
	t.Helper()
	stubTunInterlock(t, "1", true)
}

// fileSnapshot 是单个文件的落盘状态（区分"缺失"与"空文件"）。
type fileSnapshot struct {
	exists bool
	data   []byte
}

// publicEntrySandbox 让经 productionTunEnv 的公开入口可以在测试里安全调用：即便
// 联锁失效放行，编排也到不了真实系统。
type publicEntrySandbox struct {
	home       paths.Home
	elevations int
	spawns     int
	spawnErr   error // nil = 假装 detached spawn 成功（逃生口用例）
}

func newPublicEntrySandbox(t *testing.T) *publicEntrySandbox {
	t.Helper()
	s := &publicEntrySandbox{home: paths.Home(stubSocketDir(t))}
	seedTunOffHome(t, s.home)
	seedFakeCore(t, s.home) // 联锁放行后生成安装脚本要算内核摘要（批⑥b §4.5）
	// 服务视为未安装（锚指向不存在的路径 → 客户端工厂直接报未安装，不碰真 socket）；
	// 遗留位置同理指向临时路径，不读真系统目录
	pointTunAnchorAt(t, filepath.Join(s.home.String(), "absent", "service.json"))
	oldLegacy := tunLegacyHelperPath
	tunLegacyHelperPath = filepath.Join(s.home.String(), "absent-legacy")
	t.Cleanup(func() { tunLegacyHelperPath = oldLegacy })
	// 提权与 spawn 缝隙 → 计数桩（绝不真跑 osascript / 不真拉子进程）
	oldOsa := runOsaScript
	runOsaScript = func(context.Context, string) (string, string, error) {
		s.elevations++
		return "", "", errors.New("sandbox: elevation reached")
	}
	t.Cleanup(func() { runOsaScript = oldOsa })
	oldSpawn := spawnDetachedArgs
	spawnDetachedArgs = func(string, ...string) error {
		s.spawns++
		return s.spawnErr
	}
	t.Cleanup(func() { spawnDetachedArgs = oldSpawn })
	// 安装验证窗口 → 0：提权桩失败后即刻返回，不空等 12s
	oldWin := tunInstallVerifyWindow
	tunInstallVerifyWindow = 0
	t.Cleanup(func() { tunInstallVerifyWindow = oldWin })
	// 候选过滤 → 固定返回默认候选（不读本机系统目录的真实属主）
	oldCands := tunSafeExecDirCandidates
	tunSafeExecDirCandidates = func() ([]string, error) { return service.ExecDirCandidates(), nil }
	t.Cleanup(func() { tunSafeExecDirCandidates = oldCands })
	return s
}

// controlledEntries 是全部受联锁约束的公开入口及其出路文案里的子命令名。
var controlledEntries = []struct {
	cmd string
	run func(context.Context, paths.Home) error
}{
	{"on", TunOn},
	{"install", func(_ context.Context, home paths.Home) error { return TunInstall(home) }},
	{"reinstall", func(_ context.Context, home paths.Home) error { return TunReinstall(home) }},
}

func (s *publicEntrySandbox) snapshot(t *testing.T, name string) fileSnapshot {
	t.Helper()
	var path string
	switch name {
	case "settings":
		path = s.home.SettingsFile()
	case "state":
		path = s.home.StateFile()
	case "script":
		path = filepath.Join(s.home.String(), "tun-install-script.sh")
	default:
		t.Fatalf("unknown snapshot %q", name)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return fileSnapshot{exists: true, data: data}
}

// assertUntouched 断言拒绝路径零写入：settings / state / 安装脚本均与调用前一致。
func (s *publicEntrySandbox) assertUntouched(t *testing.T, before map[string]fileSnapshot) {
	t.Helper()
	for name, want := range before {
		got := s.snapshot(t, name)
		if got.exists != want.exists || !bytes.Equal(got.data, want.data) {
			t.Fatalf("%s changed across a refused call: before %+v after %+v", name, want, got)
		}
	}
}

// assertMutationLockFree 证明拒绝路径没有把 mutation lock 留在手里（同进程换 fd
// 再取，flock 语义下仍会忙——TestTryAcquireMutationLockBusy 同款）。
func assertMutationLockFree(t *testing.T, home paths.Home) {
	t.Helper()
	lock, err := TryAcquireMutationLock(home)
	if err != nil {
		t.Fatalf("mutation lock must not be held after the call: %v", err)
	}
	lock.Release()
}

// TestTunInterlockOpenOnlyForExactOne 仅精确值 "1" 放行；未设置与其他任何取值一律
// 视为未确认。
func TestTunInterlockOpenOnlyForExactOne(t *testing.T) {
	type ack struct {
		value string
		set   bool
	}
	cases := []ack{
		{"", false},
		{"", true},
		{"0", true},
		{"1", true},
		{"01", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{" 1", true},
		{"1 ", true},
		{"1\n", true},
	}
	for _, c := range cases {
		stubTunInterlock(t, c.value, c.set)
		want := c.set && c.value == "1"
		if got := tunInterlockOpen(); got != want {
			t.Fatalf("tunInterlockOpen(value=%q set=%v) = %v, want %v", c.value, c.set, got, want)
		}
	}
}

// TestTunInterlockRejectsControlledCommands 未确认（未设置，或设为其他值）→ 三个受控
// 命令均拒绝：错误带联锁哨兵与出路文案；零副作用——无 elevate、无 spawn、settings /
// state / 脚本未写、mutation lock 未被持有。
func TestTunInterlockRejectsControlledCommands(t *testing.T) {
	states := []struct {
		name  string
		value string
		set   bool
	}{
		{"unset", "", false},
		{"set to 0", "0", true},
		{"set to true", "true", true},
		{"set to ' 1'", " 1", true},
		{"set to empty", "", true},
	}
	for _, state := range states {
		for _, entry := range controlledEntries {
			name := state.name + "/" + entry.cmd
			t.Run(name, func(t *testing.T) {
				s := newPublicEntrySandbox(t)
				stubTunInterlock(t, state.value, state.set)
				before := map[string]fileSnapshot{
					"settings": s.snapshot(t, "settings"),
					"state":    s.snapshot(t, "state"),
					"script":   s.snapshot(t, "script"),
				}

				err := entry.run(context.Background(), s.home)
				if !errors.Is(err, errTunInterlocked) {
					t.Fatalf("err = %v, want the interlock refusal", err)
				}
				for _, want := range []string{
					"TUN 特权服务暂未开放：root 内核仍以用户可写目录为工作目录，任何以当前账号运行的程序都可借它取得 root（提权边界加固中）。",
					"本机自测请显式确认风险后重试：TRADEDWIND_ALLOW_INSECURE_TUN=1 tradewind tun " + entry.cmd,
				} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("err = %v, want it to carry %q", err, want)
					}
				}
				if s.elevations != 0 || s.spawns != 0 {
					t.Fatalf("a refused call must not reach elevation/spawn: elevations=%d spawns=%d", s.elevations, s.spawns)
				}
				s.assertUntouched(t, before)
				assertMutationLockFree(t, s.home)
			})
		}
	}
}

// TestTunInterlockErrorMatchesSpecifiedText 拒绝文案的全文锚（tun on）。
func TestTunInterlockErrorMatchesSpecifiedText(t *testing.T) {
	s := newPublicEntrySandbox(t)
	stubTunInterlock(t, "", false)
	err := TunOn(context.Background(), s.home)
	want := "app: TUN 特权服务暂未开放：root 内核仍以用户可写目录为工作目录，任何以当前账号运行的程序都可借它取得 root" +
		"（提权边界加固中）。本机自测请显式确认风险后重试：TRADEDWIND_ALLOW_INSECURE_TUN=1 tradewind tun on"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant exactly:\n%s", err, want)
	}
}

// TestTunInterlockPassesThroughWhenAcknowledged 显式确认（=1）→ 放行，走进既有编排。
// 编排最终停在沙箱自己的提权桩上（锚缺失 → 前置安装 → 生成脚本 → 提权缝），这恰好
// 证明联锁之后既有链路原样在跑；错误不再是联锁拒绝。
func TestTunInterlockPassesThroughWhenAcknowledged(t *testing.T) {
	for _, entry := range controlledEntries {
		t.Run(entry.cmd, func(t *testing.T) {
			s := newPublicEntrySandbox(t)
			allowInsecureTun(t)

			err := entry.run(context.Background(), s.home)
			if err == nil {
				t.Fatal("the sandbox's elevation stub must stop the orchestration with an error")
			}
			if errors.Is(err, errTunInterlocked) || strings.Contains(err.Error(), "暂未开放") {
				t.Fatalf("err = %v, want the interlock to be open", err)
			}
			if s.elevations != 1 {
				t.Fatalf("elevation seam calls = %d, want exactly 1 (orchestration reached the elevated install)", s.elevations)
			}
			if s.snapshot(t, "script").exists {
				t.Fatal("the install script must never be written to disk (批⑥b §4.5 I5)")
			}
		})
	}
}

// TestTunInterlockEscapeHatchesUnaffected tun off / status / uninstall 不受联锁约束：
// 联锁关闭、显式确认、设成别的值，三种状态下行为一致。tun off 经公开入口完整走通
// （TUN 开启 → settings 翻关 → detached spawn 用户态 start → 用户态就绪）。
func TestTunInterlockEscapeHatchesUnaffected(t *testing.T) {
	states := []struct {
		name  string
		value string
		set   bool
	}{
		{"interlock closed", "", false},
		{"acknowledged", "1", true},
		{"bogus value", "sure", true},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			s := newPublicEntrySandbox(t)
			stubTunInterlock(t, state.value, state.set)

			// status：不报错，带联锁状态行（标签列与其余状态行对齐）
			lines, err := tunStatusLines(s.home)
			if err != nil {
				t.Fatalf("tunStatusLines: %v", err)
			}
			if len(lines) == 0 || lines[len(lines)-1] != tunInterlockStatusLine() {
				t.Fatalf("status lines = %q, want the interlock line last", lines)
			}
			if !strings.Contains(lines[len(lines)-1], "联锁关闭（R16 加固中）") || !strings.HasPrefix(lines[len(lines)-1], "interlock:  ") {
				t.Fatalf("interlock line = %q, want the R16 wording on the aligned label column", lines[len(lines)-1])
			}
			if err := TunStatus(s.home); err != nil {
				t.Fatalf("TunStatus: %v", err)
			}

			// uninstall：未安装 → no-op，零提权
			if err := TunUninstall(s.home); err != nil {
				t.Fatalf("TunUninstall: %v", err)
			}
			if s.elevations != 0 {
				t.Fatalf("uninstall of an absent service must not elevate: %d", s.elevations)
			}

			// off：TUN 开启态经公开入口关掉（spawn 桩放行，用户态就绪由版本桩应答）
			seedTunOnHome(t, s.home)
			listenVersionStub(t, s.home)
			s.spawnErr = nil
			if err := TunOff(context.Background(), s.home); err != nil {
				t.Fatalf("TunOff with the interlock %s: %v", state.name, err)
			}
			if tunSettings(t, s.home).TunEnabled {
				t.Fatal("tun off must still flip tun_enabled to false")
			}
			if s.spawns != 1 || s.elevations != 0 {
				t.Fatalf("off flow: spawns=%d elevations=%d, want 1 spawn of the user-mode start and no elevation", s.spawns, s.elevations)
			}
		})
	}
}
