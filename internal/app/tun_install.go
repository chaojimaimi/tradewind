// tun_install.go：特权服务安装/卸载/重装编排（批⑥ §3.3）。
//
// 提权脚本最小化（TOCTOU 收敛）：仅两动作——①拷 helper（root:wheel 0755）
// ②执行 helper install 子命令——plist、service.json 由 root 身份的 helper 自写。
// 脚本文件置于用户数据目录（0700）；写入与 root 执行之间的同 uid 窗口如实登记
// R12（威胁模型=同 uid 已妥协时 root 面已失守）。密码红线：osascript 参数只有
// 脚本路径，脚本内容无任何秘密——密码不落盘/不落 argv/不落日志。
package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
)

// TunInstall 编排 `tradewind tun install`（幂等）：已装且 helper sha256 == 当前
// CLI → no-op；否则生成脚本 → 提权执行。本函数在测试中永远经桩 elevate，
// 绝不真实提权（真机链路进目验清单）。
func TunInstall(home paths.Home) error {
	return tunInstallWith(home, productionTunEnv(), false)
}

// TunReinstall 编排 `tradewind tun reinstall`（R4 self-update 陈旧修复）：强制
// 重拷 helper + helper install + bootstrap（重装前服务 stop 收敛 root core，
// 失败仅警告——bootout/bootstrap 由 helper install 幂等处理）。
func TunReinstall(home paths.Home) error {
	return tunInstallWith(home, productionTunEnv(), true)
}

// TunUninstall 编排 `tradewind tun uninstall`：服务 stop（先停 root core）→ 提权
// bootout + rm helper/plist/service.json（同提权弹窗）。未安装 → no-op。
func TunUninstall(home paths.Home) error {
	return tunUninstallWith(home, productionTunEnv())
}

func tunInstallWith(home paths.Home, env tunEnv, force bool) error {
	if !force && env.serviceAnchorExists() && env.helperMatchesCurrent() {
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
	script := tunInstallScript(exe, home.String(), os.Getuid())
	if err := tunRunElevated(home, env, script); err != nil {
		return err
	}
	fmt.Println("tun: privileged service installed")
	return nil
}

func tunUninstallWith(home paths.Home, env tunEnv) error {
	if !env.serviceAnchorExists() {
		fmt.Println("tun: service is not installed; nothing to do")
		return nil
	}
	if err := tunStopCoreForward(env); err != nil {
		fmt.Printf("warning: pre-uninstall service stop skipped (%v)\n", err)
	}
	if err := tunRunElevated(home, env, tunUninstallScript()); err != nil {
		return err
	}
	fmt.Println("tun: privileged service uninstalled")
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

// tunInstallScript 生成安装提权脚本（纯函数——单测断言两动作与单引号转义）。
func tunInstallScript(cliPath, dataDir string, dataUID int) string {
	helper := shQuote(service.HelperInstallPath)
	var b strings.Builder
	b.WriteString("mkdir -p " + shQuote(filepath.Dir(service.HelperInstallPath)) + "\n")
	b.WriteString("cp " + shQuote(cliPath) + " " + helper + "\n")
	b.WriteString("chown root:wheel " + helper + "\n")
	b.WriteString("chmod 0755 " + helper + "\n")
	b.WriteString(helper + " install --data-dir " + shQuote(dataDir) + " --data-uid " + strconv.Itoa(dataUID) + "\n")
	return b.String()
}

// tunUninstallScript 生成卸载提权脚本（纯函数）：bootout + rm helper/plist/锚
// （core.pid 一并清理——孤儿收编判据随锚失效）。
func tunUninstallScript() string {
	var b strings.Builder
	b.WriteString("launchctl bootout system/" + service.DaemonLabel + " 2>/dev/null || true\n")
	for _, p := range []string{
		service.HelperInstallPath,
		service.DaemonPlistPath,
		service.DefaultConfigPath,
		service.DefaultCorePIDPath,
	} {
		b.WriteString("rm -f " + shQuote(p) + "\n")
	}
	return b.String()
}

// runElevatedScript 经 osascript do shell script 提权执行脚本文件（弹窗交互；
// osascript 参数只有脚本路径——密码不落盘/不落 argv/不落日志）。WHY 120s 有界：
// 弹窗等待含用户输入，超时明确报错不无限挂起（壳端 90s 档先到，此处兜底）。
func runElevatedScript(scriptPath string) error {
	as := fmt.Sprintf("do shell script %s with administrator privileges",
		quoteAS("/bin/sh "+shQuote(scriptPath)))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "osascript", "-e", as)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("app: 提权执行失败（弹窗取消或超时）: %w", err)
	}
	return nil
}
