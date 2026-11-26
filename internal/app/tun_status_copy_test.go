package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// tun_status_copy_test.go：`tun status` 的未安装文案与"旧布局"提示。批⑥b §4.7 终审
// 的联锁两态（P3-②/P3-③）已随 TUN 开放（阶段二）整体收敛为单态——interlock 行删除，
// 未安装行只保留安装指引。

// TestTunStatusLinesNotInstalledGuidance 未安装行的安装指引（联锁移除后的单态）：
// 点明 `tun install` 与 `tun on` 会自动安装，不再提及任何确认环境变量；interlock
// 行从状态输出中整体消失。
func TestTunStatusLinesNotInstalledGuidance(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
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
	want := "service:    not installed（`tradewind tun install` 安装特权服务；`tun on` 会自动安装）"
	if !strings.Contains(joined, want) {
		t.Fatalf("status lines =\n%s\nwant the not-installed line to carry %q", joined, want)
	}
	// 联锁已整体移除：既没有 interlock 行，也没有确认环境变量的任何踪迹
	for _, banned := range []string{"interlock", "TRADEDWIND_ALLOW_INSECURE_TUN", "联锁"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("status lines =\n%s\nmust not mention the removed interlock (%q)", joined, banned)
		}
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
