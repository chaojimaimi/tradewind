package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// LaunchAgentLabel 是 launchd agent 标识（M2-plan §10 记录表，终局不迁移）。
const LaunchAgentLabel = "io.github.chaojimaimi.tradewind.agent"

// launchctlRunner 抽象 launchctl 执行（测试注入假 runner，绝不触碰真实 launchd）。
type launchctlRunner interface {
	Run(args ...string) (stdout string, err error)
}

// realLaunchctl 真实 launchctl 执行器（legacy load/unload 写死，M2-plan §2.3——
// 不取现代 bootstrap/bootstop，避免实现自选）。
type realLaunchctl struct{}

func (realLaunchctl) Run(args ...string) (string, error) {
	cmd := exec.Command("launchctl", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("app: launchctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// launchAgentsDir 返回用户 LaunchAgents 目录（plist 落点）。
func launchAgentsDir() (string, error) {
	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("app: resolve home: %w", err)
	}
	return filepath.Join(u, "Library", "LaunchAgents"), nil
}

// plistPathFor 某 LaunchAgents 目录下的 plist 落点。
func plistPathFor(dir string) string {
	return filepath.Join(dir, LaunchAgentLabel+".plist")
}

// Autostart 编排 `tradewind autostart on|off|status`。
func Autostart(home paths.Home, action string) error {
	dir, err := launchAgentsDir()
	if err != nil {
		return err
	}
	switch action {
	case "on":
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("app: resolve executable: %w", err)
		}
		return enableAutostart(home, dir, exe, realLaunchctl{})
	case "off":
		return disableAutostart(dir, realLaunchctl{})
	case "status":
		return autostartStatus(dir, realLaunchctl{})
	default:
		return errors.New("app: usage: tradewind autostart on|off|status")
	}
}

// enableAutostart 写 plist + launchctl load。
// 迁移语义（M2-plan §2.3）：已有手动实例在跑 → 先 stop 再 load——防 RunAtLoad
// 立即拉起第二实例撞 pidfile 锁、非 0 退出触发 KeepAlive 节流循环。
func enableAutostart(home paths.Home, dir, exePath string, r launchctlRunner) error {
	if err := stopManualInstance(home); err != nil {
		return fmt.Errorf("app: autostart migration: %w", err)
	}
	if _, err := os.Stat(exePath); err != nil {
		return fmt.Errorf("app: executable %s: %w", exePath, err)
	}
	content, err := launchdPlist(LaunchAgentLabel, exePath, home.LogsDir())
	if err != nil {
		return err
	}
	p := plistPathFor(dir)
	// plist 非凭据（无 secret/URL），launchd 惯例 644
	if err := os.WriteFile(p, content, 0o644); err != nil {
		return fmt.Errorf("app: write plist %s: %w", p, err)
	}
	if _, err := r.Run("load", p); err != nil {
		return err
	}
	fmt.Printf("autostart enabled (%s loaded; starts on login, restarts on crash)\n", LaunchAgentLabel)
	return nil
}

// disableAutostart launchctl unload + 删除 plist。
// unload 失败仅告警仍继续删除——半残状态（plist 已损坏等）也必须能被 off 关闭。
func disableAutostart(dir string, r launchctlRunner) error {
	p := plistPathFor(dir)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		fmt.Println("autostart is not enabled")
		return nil
	}
	if _, err := r.Run("unload", p); err != nil {
		fmt.Printf("warning: unload failed (%v); removing plist anyway\n", err)
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("app: remove plist %s: %w", p, err)
	}
	fmt.Println("autostart disabled")
	return nil
}

// autostartStatus 报告 plist 存在性 + launchd 加载态。
func autostartStatus(dir string, r launchctlRunner) error {
	p := plistPathFor(dir)
	_, statErr := os.Stat(p)
	loaded := false
	if _, err := r.Run("list", LaunchAgentLabel); err == nil {
		loaded = true
	}
	switch {
	case statErr == nil && loaded:
		fmt.Println("autostart: enabled (plist installed, loaded)")
	case statErr == nil:
		fmt.Println("autostart: installed but not loaded (takes effect next login)")
	default:
		fmt.Println("autostart: disabled")
	}
	return nil
}

// stopManualInstance 迁移前置：pidfile 存在则走 Stop（stale pidfile 由 Stop 自清）。
func stopManualInstance(home paths.Home) error {
	if _, err := os.Stat(home.PIDFile()); errors.Is(err, os.ErrNotExist) {
		return nil // 无手动实例
	}
	fmt.Println("autostart: stopping existing manual instance first...")
	return Stop(home)
}

// launchdPlist 生成 LaunchAgent plist 内容。
// KeepAlive={SuccessfulExit:false} 裁决（M2-plan §2.3）：优雅 stop 捕获信号正常返回
// → exit 0 → launchd 不拉回；崩溃 exit 1 → 自动拉起——与既有退出码语义精确对齐。
// Standard*Path 落 logs/：launchd 下 stdout/stderr 不丢。
func launchdPlist(label, exePath, logsDir string) ([]byte, error) {
	for _, v := range []string{label, exePath, logsDir} {
		if strings.ContainsAny(v, "&<>") {
			return nil, fmt.Errorf("app: plist path %q contains XML-special chars", v)
		}
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + exePath + `</string>
		<string>start</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>` + logsDir + `/launchd.out.log</string>
	<key>StandardErrorPath</key>
	<string>` + logsDir + `/launchd.err.log</string>
</dict>
</plist>
`), nil
}

// autostartEnabled 报告 plist 是否已安装（stop 提示语用，best-effort）。
func autostartEnabled() bool {
	dir, err := launchAgentsDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(plistPathFor(dir))
	return err == nil
}
