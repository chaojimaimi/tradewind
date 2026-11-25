package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
)

// 安装脚本的候选过滤（review P2）：祖先目录用户/admin 可写的候选不进脚本；一个都
// 不剩 → 说明原因，不生成脚本、不提权。

// captureInstallScript 把 elevate 换成"捕获脚本源码"（批⑥b §4.5 I5：脚本文本经
// 提权缝隙直达，不再有落盘的脚本文件可读）。
func captureInstallScript(env *tunEnv) *string {
	var script string
	env.elevate = func(source string) error {
		script = source
		return nil
	}
	return &script
}

// TestTunInstallFiltersUnsafeExecDirCandidates 不安全的候选不出现在脚本里：探测链
// 与全败文案都只列安全候选。
func TestTunInstallFiltersUnsafeExecDirCandidates(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedFakeCore(t, home) // 生成脚本要算内核摘要（批⑥b §4.5）
	env, _, _, _, _ := newTunTestEnv(t)
	env.serviceAnchorExists = func() bool { return false } // 尚未安装：非 force 才会走到生成脚本
	safe := []string{"/Library/Application Support/Tradewind", "/opt/tradewind"}
	env.safeExecDirCandidates = func() ([]string, error) { return safe, nil }
	script := captureInstallScript(&env)

	if err := tunInstallWith(home, env, false); err != nil {
		t.Fatalf("tunInstallWith: %v", err)
	}
	if *script == "" {
		t.Fatal("the install script was not generated")
	}
	for _, want := range []string{
		"try_candidate '" + safe[0] + "' || ",
		"try_candidate '" + safe[1] + "' || fail ",
	} {
		if !strings.Contains(*script, want) {
			t.Fatalf("script missing %q:\n%s", want, *script)
		}
	}
	if strings.Contains(*script, "try_candidate '/usr/local/libexec/tradewind'") {
		t.Fatalf("an unsafe candidate must not be probed:\n%s", *script)
	}
	terminator := "未安装任何文件：" + strings.Join(safe, " / ")
	if !strings.Contains(*script, terminator) || strings.Contains(*script, "/opt/tradewind / ") {
		t.Fatalf("the all-blocked message must list only the safe candidates:\n%s", *script)
	}
}

// TestTunInstallAbortsWhenNoSafeCandidate 一个候选都剩不下：install / reinstall /
// `tun on` 的前置安装都报错并带出各候选被排除的原因，不生成脚本、不提权；`tun on`
// 还不得翻转 settings。
func TestTunInstallAbortsWhenNoSafeCandidate(t *testing.T) {
	noneLeft := errors.New("app: 没有祖先目录安全的安装位置（…）：/Library/Application Support/Tradewind（… group/other-writable …）；" +
		"/usr/local/libexec/tradewind（… /usr/local is owned by uid 501 …）；/opt/tradewind（… symbolic link …）；修正对应目录的属主/权限后重试")

	cases := []struct {
		name string
		run  func(home paths.Home, env tunEnv) error
	}{
		{"tun install", func(home paths.Home, env tunEnv) error {
			env.serviceAnchorExists = func() bool { return false } // 尚未安装，非 force 才会生成脚本
			return tunInstallWith(home, env, false)
		}},
		{"tun reinstall", func(home paths.Home, env tunEnv) error { return tunInstallWith(home, env, true) }},
		{"tun on pre-install", func(home paths.Home, env tunEnv) error {
			var installed bool
			env.serviceAnchorExists = func() bool { return installed }
			return tunOnWith(context.Background(), home, env)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := paths.Home(stubSocketDir(t))
			seedTunOffHome(t, home)
			env, _, _, _, _ := newTunTestEnv(t)
			env.safeExecDirCandidates = func() ([]string, error) { return nil, noneLeft }
			elevated := 0
			env.elevate = func(string) error { elevated++; return nil }

			err := c.run(home, env)
			if !errors.Is(err, noneLeft) {
				t.Fatalf("err = %v, want the no-safe-candidate error propagated verbatim", err)
			}
			for _, want := range []string{"owned by uid 501", "/opt/tradewind", "symbolic link"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want it to carry %q", err, want)
				}
			}
			if elevated != 0 {
				t.Fatalf("elevation runs = %d, want 0 (no script may be generated)", elevated)
			}
			// 脚本不落盘（批⑥b §4.5 I5）：用户数据目录里不存在旧实现的脚本文件
			if _, statErr := os.Stat(filepath.Join(home.String(), "tun-install-script.sh")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("no script file may be left behind (err = %v)", statErr)
			}
			if tunSettings(t, home).TunEnabled {
				t.Fatal("settings must stay untouched")
			}
		})
	}
}

// TestComposeSafeCandidates "一个不剩"的报错文案拼装：逐候选带原因；有安全候选时
// 原样返回。
func TestComposeSafeCandidates(t *testing.T) {
	safe := []string{"/a", "/b"}
	if got, err := composeSafeCandidates(safe, []service.ExecDirCandidateRejection{
		{Candidate: "/c", Reason: errors.New("group-writable")},
	}); err != nil || strings.Join(got, "|") != "/a|/b" {
		t.Fatalf("composeSafeCandidates = %q err = %v, want the safe list as is", got, err)
	}

	rejected := []service.ExecDirCandidateRejection{
		{Candidate: "/c1", Reason: errors.New("entry /c1 is group/other-writable (0775)")},
		{Candidate: "/c2", Reason: errors.New("entry /usr/local is owned by uid 501 (must be root)")},
	}
	got, err := composeSafeCandidates(nil, rejected)
	if got != nil || err == nil {
		t.Fatalf("got %q err = %v, want nil and an error", got, err)
	}
	for _, want := range []string{"没有祖先目录安全的安装位置", "/c1（entry /c1 is group/other-writable (0775)）",
		"/c2（entry /usr/local is owned by uid 501 (must be root)）", "修正对应目录的属主/权限后重试"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to carry %q", err, want)
		}
	}
}
