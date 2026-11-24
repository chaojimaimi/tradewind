// tun_install.go：特权服务安装/卸载/重装编排 + 提权执行（批⑥ §3.3；真机 fold-in）。
//
// 脚本本体见 tun_script.go（纯函数生成器）。提权经 osascript `do shell script`，
// 参数只有脚本路径——密码不落盘/不落 argv/不落日志。脚本文件置于用户数据目录
// （0700）；写入与 root 执行之间的同 uid 窗口如实登记 R12（威胁模型=同 uid 已
// 妥协时 root 面已失守）。
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// tunElevateTimeout 提权执行的有界等待：弹窗等待含用户输入 + 安装脚本执行自测与
// helper install，超时明确报错不无限挂起。
const tunElevateTimeout = 120 * time.Second

// TunInstall 编排 `tradewind tun install`（幂等）：已装且 helper 与 root 内核副本
// 均与当前一致 → no-op；否则生成脚本 → 提权执行 → 安装验证。本函数在测试中永远
// 经桩 elevate，绝不真实提权。受联锁约束：未显式确认风险即拒绝，此前不拿任何资源。
func TunInstall(home paths.Home) error {
	if err := requireTunInterlockOpen("install"); err != nil {
		return err
	}
	return tunInstallWith(home, productionTunEnv(home), false)
}

// TunReinstall 编排 `tradewind tun reinstall`（R4 陈旧修复）：强制重拷 helper + 内核
// root 副本 + helper install + bootstrap（重装前服务 stop 收敛 root core，失败仅
// 警告——bootout/bootstrap 由 helper install 幂等处理）。TUN 开启期间，安装验证
// 通过后把会话重收敛回来（见 tunReconvergeTunSession）。受联锁约束（同 TunInstall）。
func TunReinstall(home paths.Home) error {
	if err := requireTunInterlockOpen("reinstall"); err != nil {
		return err
	}
	return tunInstallWith(home, productionTunEnv(home), true)
}

// TunUninstall 编排 `tradewind tun uninstall`：服务 stop（先停 root core）→ 提权
// bootout + 清 plist/日志/锚文件/遗留 helper/各候选三文件。锚与遗留残留都不在 →
// no-op；只有遗留残留（失败安装留下的 root 属主 helper、没有 service.json）同样
// 走提权清理。不受联锁约束（卸载是收敛手段，必须永远可用）。
func TunUninstall(home paths.Home) error {
	return tunUninstallWith(home, productionTunEnv(home))
}

func tunInstallWith(home paths.Home, env tunEnv, force bool) error {
	if !force && env.serviceAnchorExists() && env.helperMatchesCurrent() && env.coreMatchesCurrent(home) {
		fmt.Println("tun: service already installed and up to date; nothing to do")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("app: resolve executable: %w", err)
	}
	if force && env.serviceAnchorExists() {
		if err := tunStopCoreForward(env); err != nil {
			fmt.Printf("warning: pre-reinstall service stop skipped (%v)\n", err)
		}
	}
	// 候选过滤（review P2）：祖先目录用户/admin 可写的候选不进脚本——那种目录下
	// 执行目录可被整个换成符号链接。一个都不剩 → 说明原因，不生成脚本。
	candidates, err := env.safeExecDirCandidates()
	if err != nil {
		return err
	}
	script := tunInstallScriptFor(exe, home.String(), os.Getuid(), installScriptLayout(candidates))
	if err := tunRunElevated(home, env, script); err != nil {
		return err
	}
	// 安装验证（真机 fold-in）：提权成功 ≠ 服务可用——helper 被 SIGKILL 时脚本
	// 未必以非零退出告终，故以"服务 status 可达"为安装成功的判据。
	if err := env.waitForServiceReady(); err != nil {
		return err
	}
	fmt.Println("tun: privileged service installed")
	// 只有 reinstall 重收敛：`tun on` 的前置安装（force=false）自身随后就会走
	// converge，且它已持有 mutation lock——在这里再收敛会双重重启并自锁。
	if force {
		if err := tunReconvergeTunSession(home, env); err != nil {
			return err
		}
	}
	return nil
}

// tunReconvergeTunSession 重装后的 TUN 会话重收敛（真机 fold-in 2）：helper install
// 会 bootout 旧 daemon，root core 随之停下，而 start 进程的健康轮询只通知不自愈
// ——TUN 开启期间重装后会话就此断掉。settings 说 TUN 开 → 复用既有缝隙
// stopStart → spawnStart → waitServiceCore 把会话拉回来（不写 settings、不碰
// sysproxy）；未开启 → 无动作。任一步失败 → 报错并给出恢复指引。
func tunReconvergeTunSession(home paths.Home, env tunEnv) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	if !settings.TunEnabled {
		return nil
	}
	// 与 tun on/off 同一把 mutation lock：重收敛期间并发的切换编排串行化而非交错
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	const guidance = "请执行 `tradewind tun off && tradewind tun on` 恢复会话"
	if err := env.stopStart(home); err != nil {
		return fmt.Errorf("app: 重装后重收敛 TUN 会话失败（停止旧 start）: %v；%s", err, guidance)
	}
	if err := env.spawnStart(); err != nil {
		return fmt.Errorf("app: 重装后重收敛 TUN 会话失败（拉起 start）: %v；%s", err, guidance)
	}
	if _, err := tunWaitReady(context.Background(), home, env, true); err != nil {
		return fmt.Errorf("app: 重装后重收敛 TUN 会话失败（TUN core 未就绪）: %v；%s", err, guidance)
	}
	fmt.Println("tun: TUN session re-converged after reinstall")
	return nil
}

func tunUninstallWith(home paths.Home, env tunEnv) error {
	anchor := env.serviceAnchorExists()
	if !anchor && !env.legacyResidueExists() {
		fmt.Println("tun: service is not installed; nothing to do")
		return nil
	}
	if err := tunStopCoreForward(env); err != nil {
		fmt.Printf("warning: pre-uninstall service stop skipped (%v)\n", err)
	}
	if err := tunRunElevated(home, env, tunUninstallScript()); err != nil {
		return err
	}
	if anchor {
		fmt.Println("tun: privileged service uninstalled")
	} else {
		fmt.Println("tun: legacy helper residue removed")
	}
	return nil
}

// tunRunElevated 落盘脚本（0700 用户数据目录，R12 边界登记）→ 提权执行 → 清理脚本。
func tunRunElevated(home paths.Home, env tunEnv, script string) error {
	path := tunScriptPath(home)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return fmt.Errorf("app: write tun script: %w", err)
	}
	defer os.Remove(path)
	return env.elevate(path)
}

func tunScriptPath(home paths.Home) string { return home.Join("tun-install-script.sh") }

// shQuote 单引号包裹转义（含空格/单引号用户名场景，批⑥ §3.3）。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runOsaScript 经 osascript 执行 AppleScript 片段的生产实现（缝隙——测试注入桩，
// 绝不真实提权）。stdout/stderr 原文返回供失败诊断：脚本的 stderr 经
// `do shell script` 原样透传到 osascript 的 stderr。
var runOsaScript = func(ctx context.Context, appleScript string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "osascript", "-e", appleScript)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	return out.String(), errOut.String(), runErr
}

// runElevatedScript 经 osascript do shell script 提权执行脚本文件（弹窗交互；
// osascript 参数只有脚本路径）。失败时把 osascript 的输出带进错误——真机首测
// 时 CLI 丢弃了它，只报出含糊的 "exit status 1"，根因（AppleSystemPolicy 拦截）
// 要翻系统日志才看得到。
func runElevatedScript(scriptPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tunElevateTimeout)
	defer cancel()
	stdout, stderr, err := runOsaScript(ctx, fmt.Sprintf(
		"do shell script %s with administrator privileges", quoteAS("/bin/sh "+shQuote(scriptPath))))
	if err == nil {
		return nil
	}
	return elevatedRunError(ctx.Err(), stdout, stderr, err)
}

// elevatedRunError 组装提权失败诊断（纯函数）：三分类——「用户取消弹窗」（osascript
// 以 -128 报 User canceled）／「超时」／「脚本执行失败」（带输出摘要）。
func elevatedRunError(ctxErr error, stdout, stderr string, runErr error) error {
	switch {
	case strings.Contains(stderr, "(-128)"):
		return errors.New("app: 已取消管理员授权弹窗，未执行任何更改")
	case ctxErr != nil:
		return fmt.Errorf("app: 提权执行超时（%s 内弹窗未响应或脚本未结束）", tunElevateTimeout)
	default:
		return fmt.Errorf("app: 提权脚本执行失败（%v）；脚本输出:\n%s", runErr, elevatedOutputSnippet(stdout, stderr))
	}
}

// elevatedOutputMaxBytes 是提权失败诊断携带的输出上限。
const elevatedOutputMaxBytes = 2000

// elevatedOutputSnippet 提权失败时的输出摘要：stderr 在前（诊断信息主体——脚本
// 自身的 stderr 与 osascript 的错误行都在这里），stdout 在后；合计截断到
// elevatedOutputMaxBytes，且不切断多字节字符。
func elevatedOutputSnippet(stdout, stderr string) string {
	var parts []string
	for _, s := range []string{stderr, stdout} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	joined := strings.Join(parts, "\n")
	if len(joined) <= elevatedOutputMaxBytes {
		return joined
	}
	cut := joined[:elevatedOutputMaxBytes]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n…(已截断)"
}
