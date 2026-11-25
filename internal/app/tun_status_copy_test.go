package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// tun_status_copy_test.go：`tun status` 三处文案的联锁两态（批⑥b §4.7 终审
// P3-②/P3-③）。

// TestTunStatusLinesInterlockStates 联锁关/开 × 未安装：未安装行与 interlock 行的
// 两态文案 + 标签列前缀 `interlock:  `（与其余状态行同列对齐）。
func TestTunStatusLinesInterlockStates(t *testing.T) {
	cases := []struct {
		name        string
		value       string // 联锁环境变量的值（set=false 表示不设）
		set         bool
		wantService string // 未安装行必含
		wantLocked  string // interlock 行必含
	}{
		{
			name:        "interlock closed",
			value:       "",
			set:         false,
			wantService: "not installed（联锁关闭：`tradewind tun install` 与 `tun on` 默认拒绝；本机自测需显式确认 TRADEDWIND_ALLOW_INSECURE_TUN=1）",
			wantLocked:  "联锁关闭（R16 加固中）",
		},
		{
			name:        "interlock open",
			value:       "1",
			set:         true,
			wantService: "not installed（`tradewind tun install` 安装特权服务；`tun on` 会自动安装）",
			wantLocked:  "已显式放行（仅限本机自测）",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := paths.Home(stubSocketDir(t))
			seedTunOffHome(t, home)
			stubTunInterlock(t, c.value, c.set)
			// 锚不存在 = 未安装（不触真锚）
			pointTunAnchorAt(t, filepath.Join(stubSocketDir(t), "absent.json"))
			oldNewClient := tunNewClient
			tunNewClient = func(time.Duration) (tunServiceClientT, error) {
				return nil, errTunServiceNotInstalled
			}
			t.Cleanup(func() { tunNewClient = oldNewClient })

			lines, err := tunStatusLines(home)
			if err != nil {
				t.Fatalf("tunStatusLines: %v", err)
			}
			joined := strings.Join(lines, "\n")
			if !strings.Contains(joined, c.wantService) {
				t.Fatalf("status lines =\n%s\nwant the not-installed line to carry %q", joined, c.wantService)
			}
			if !strings.Contains(joined, c.wantLocked) {
				t.Fatalf("status lines =\n%s\nwant the interlock line to carry %q", joined, c.wantLocked)
			}
			for _, line := range lines {
				if strings.HasPrefix(line, "interlock:") && !strings.HasPrefix(line, "interlock:  ") {
					t.Fatalf("interlock line = %q, want the aligned label column `interlock:  `", line)
				}
			}
			// 两态互斥：文案里不得同时出现两种措辞
			if strings.Contains(joined, "联锁关闭（R16 加固中）") && strings.Contains(joined, "已显式放行（仅限本机自测）") {
				t.Fatalf("status lines =\n%s\nwant exactly one interlock state wording", joined)
			}
		})
	}
}

// TestTunBinaryStatusLinesOldLayoutHint "旧布局"提示必须点明批⑥b 前内核以用户数据
// 目录为工作目录（用户据此理解为什么要 reinstall）。
func TestTunBinaryStatusLinesOldLayoutHint(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	// 锚不可读 = 按旧布局/无效处理（writeTunAnchor 的 v3 形态反着用：指向缺失文件）
	pointTunAnchorAt(t, filepath.Join(stubSocketDir(t), "absent.json"))

	lines := tunBinaryStatusLinesFor(home, "")
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "批⑥b 前内核以用户数据目录为工作目录") {
		t.Fatalf("status lines =\n%s\nwant the old-layout hint to explain the pre-batch6b working dir", joined)
	}
	if !strings.Contains(joined, "tun reinstall") {
		t.Fatalf("status lines =\n%s\nwant the reinstall guidance", joined)
	}
}
