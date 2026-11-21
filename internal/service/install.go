// install.go：helper 安装子命令（批⑥ §3.3）。
//
// 提权脚本只做两动作：拷 helper（root:wheel 0755）+ 执行本包 Install——plist 与
// service.json 由 root 身份的 helper 自写。{data_dir, data_uid} 经内联 argv 传入
// （argv 无秘密，符合红线；root 无法"自证"调用者数据目录，评审二轮 P2-c），
// 不读用户可写文件（TOCTOU 不移回配置面，R12）。
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchctlRunner 抽象 launchctl 执行（测试注入假 runner，绝不触碰真实 launchd）。
type launchctlRunner interface {
	Run(args ...string) error
}

type realLaunchctl struct{}

func (realLaunchctl) Run(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("service: launchctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Install 执行 helper 安装子命令（`tradewind install --data-dir <p> --data-uid <n>`）：
// 校验 argv → 写 service.json + daemon plist → bootout（幂等，失败忽略）→ bootstrap。
// 必须 root（提权脚本调用；非 root 直接报错，绝不静默降级为用户态写系统目录）。
func Install(dataDir string, dataUID int, stdout io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("service: install must run as root (via `tradewind tun install`)")
	}
	if !filepath.IsAbs(dataDir) || dataUID <= 0 {
		return fmt.Errorf("service: invalid install args (data_dir=%q must be absolute, data_uid=%d must be > 0)", dataDir, dataUID)
	}
	if err := installFiles(dataDir, dataUID, DefaultConfigDir, "/Library/LaunchDaemons", "/Library/Logs"); err != nil {
		return err
	}
	helper, err := os.Executable()
	if err != nil {
		return fmt.Errorf("service: resolve executable: %w", err)
	}
	fmt.Fprintf(stdout, "service: installed (helper=%s)\n", helper)
	lc := realLaunchctl{}
	if err := lc.Run("bootout", "system/"+DaemonLabel); err != nil {
		fmt.Fprintf(stdout, "service: bootout skipped (%v)\n", err) // 未加载时的幂等分支
	}
	if err := lc.Run("bootstrap", "system", DaemonPlistPath); err != nil {
		return fmt.Errorf("service: bootstrap daemon: %w", err)
	}
	fmt.Fprintf(stdout, "service: daemon bootstrapped (%s)\n", DaemonLabel)
	return nil
}

// installFiles 写 service.json（0644）+ daemon plist（0644）。目录/落点参数化——
// 测试指向临时目录，绝不触碰真实系统目录。
func installFiles(dataDir string, dataUID int, configDir, launchDaemonsDir, logsDir string) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("service: mkdir %s: %w", configDir, err)
	}
	if err := os.MkdirAll(launchDaemonsDir, 0o755); err != nil {
		return fmt.Errorf("service: mkdir %s: %w", launchDaemonsDir, err)
	}
	helper := HelperInstallPath
	if err := writeJSONFile(filepath.Join(configDir, "service.json"), ConfigFile{
		AllowedUIDs: []int{dataUID},
		DataDir:     dataDir,
		CoreBin:     filepath.Join(dataDir, "bin", "mihomo"),
		Sidecar:     filepath.Join(dataDir, "bin", "mihomo.sha256"),
		Runtime:     filepath.Join(dataDir, "runtime.yaml"),
		Sock:        filepath.Join(dataDir, "core.sock"),
		Log:         filepath.Join(dataDir, "logs", "core-service.log"),
		DataUID:     dataUID,
	}); err != nil {
		return err
	}
	plist, err := daemonPlist(helper, filepath.Join(logsDir, "tradewind-service.log"))
	if err != nil {
		return err
	}
	p := filepath.Join(launchDaemonsDir, DaemonLabel+".plist")
	// plist 非凭据（无 secret/URL），launchd 惯例 644
	if err := os.WriteFile(p, plist, 0o644); err != nil {
		return fmt.Errorf("service: write plist %s: %w", p, err)
	}
	return nil
}

// daemonPlist 生成 LaunchDaemon plist：KeepAlive=true（launchd 内建 10s 节流拉活，
// 批⑥ §3.1）+ 日志落 /Library/Logs（R3：service.json 非法时 /Library/Logs 可观察）。
func daemonPlist(helperPath, logPath string) ([]byte, error) {
	for _, v := range []string{helperPath, logPath} {
		if strings.ContainsAny(v, "&<>") {
			return nil, fmt.Errorf("service: plist path %q contains XML-special chars", v)
		}
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + DaemonLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + helperPath + `</string>
		<string>service</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + logPath + `</string>
	<key>StandardErrorPath</key>
	<string>` + logPath + `</string>
</dict>
</plist>
`), nil
}

// writeJSONFile 写 service.json（0644 root-owned 锚，服务启动时只读校验）。
func writeJSONFile(path string, cfg ConfigFile) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("service: marshal service.json: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("service: write %s: %w", path, err)
	}
	return nil
}
