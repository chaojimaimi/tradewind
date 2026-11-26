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

// tun_entry_test.go：TUN 开放（批⑥b 阶段二）后的公开入口回归。
//
// 联锁（TRADEDWIND_ALLOW_INSECURE_TUN 环境变量 + 提权脚本 --allow-insecure-tun 旗标）
// 已整体移除：tun on / install / reinstall 不带任何确认环境变量即可走进既有编排。
// 下面的沙箱把公开入口会触达的系统缝隙全部指向测试桩或临时路径——绝不触真服务
// socket / launchd / 提权 / 子进程。

// ungatedEntries 是不再受任何闸门约束的公开入口及其出路文案里的子命令名。
var ungatedEntries = []struct {
	cmd string
	run func(context.Context, paths.Home) error
}{
	{"on", TunOn},
	{"install", func(_ context.Context, home paths.Home) error { return TunInstall(home) }},
	{"reinstall", func(_ context.Context, home paths.Home) error { return TunReinstall(home) }},
}

// TestTunEntriesProceedWithoutAcknowledgement 三个公开入口不带确认环境变量即可走进
// 既有编排：前置安装走到沙箱的提权桩（elevate 恰好一次），错误是提权桩自身的失败
// ——不再是任何形式的联锁拒绝；安装脚本不落盘（批⑥b §4.5 I5）。
func TestTunEntriesProceedWithoutAcknowledgement(t *testing.T) {
	for _, entry := range ungatedEntries {
		t.Run(entry.cmd, func(t *testing.T) {
			home := paths.Home(stubSocketDir(t))
			stubBrokerSockPath(t) // TUN 形态的内核 API 入口 → 短路径桩
			seedTunOffHome(t, home)
			seedFakeCore(t, home) // 生成安装脚本要算内核摘要（批⑥b §4.5）
			// 服务视为未安装（锚指向不存在的路径 → 不碰真 socket）；遗留位置同理指向
			// 临时路径，不读真系统目录
			pointTunAnchorAt(t, filepath.Join(home.String(), "absent", "service.json"))
			oldLegacy := tunLegacyHelperPath
			tunLegacyHelperPath = filepath.Join(home.String(), "absent-legacy")
			t.Cleanup(func() { tunLegacyHelperPath = oldLegacy })
			// 提权与 spawn 缝隙 → 计数桩（绝不真跑 osascript / 不真拉子进程）
			elevations, spawns := 0, 0
			oldOsa := runOsaScript
			runOsaScript = func(context.Context, string) (string, string, error) {
				elevations++
				return "", "", errors.New("sandbox: elevation reached")
			}
			t.Cleanup(func() { runOsaScript = oldOsa })
			oldSpawn := spawnDetachedArgs
			spawnDetachedArgs = func(string, ...string) error {
				spawns++
				return errors.New("sandbox: spawn reached")
			}
			t.Cleanup(func() { spawnDetachedArgs = oldSpawn })
			// 候选过滤 → 固定返回默认候选（不读本机系统目录的真实属主）
			oldCands := tunSafeExecDirCandidates
			tunSafeExecDirCandidates = func() ([]string, error) { return service.ExecDirCandidates(), nil }
			t.Cleanup(func() { tunSafeExecDirCandidates = oldCands })

			err := entry.run(context.Background(), home)
			if err == nil {
				t.Fatal("the sandbox's elevation stub must stop the orchestration with an error")
			}
			for _, banned := range []string{"暂未开放", "TRADEDWIND_ALLOW_INSECURE_TUN", "allow-insecure-tun"} {
				if strings.Contains(err.Error(), banned) {
					t.Fatalf("err = %v, must not carry the removed interlock refusal (%q)", err, banned)
				}
			}
			if elevations != 1 {
				t.Fatalf("elevation seam calls = %d, want exactly 1 (orchestration reached the elevated install)", elevations)
			}
			if spawns != 0 {
				t.Fatalf("spawns = %d, want 0 (the flow stops at the elevated install)", spawns)
			}
			entries, err := os.ReadDir(home.String())
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.Contains(e.Name(), "tun-install-script") {
					t.Fatalf("the elevated script must not be written to the user data dir, found %s", e.Name())
				}
			}
		})
	}
}
