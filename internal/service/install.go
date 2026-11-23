// install.go：helper 安装子命令（批⑥ §3.3；真机 fold-in 改布局）。
//
// 提权脚本把 helper 副本 + 内核的 root 属主副本拷进执行目录并做执行自测后，调用
// 本包 Install——plist 与 service.json 由 root 身份的 helper 自写。{data_dir,
// data_uid, exec_dir} 经内联 argv 传入（argv 无秘密，符合红线；root 无法"自证"
// 调用者数据目录，评审二轮 P2-c），不读用户可写文件（TOCTOU 不移回配置面，R12）。
//
// LPE 修复（batch6 真机 fold-in）：service.json 的 core_bin/sidecar 指向执行目录下
// 的 root 属主副本，绝不指向用户可写的 data_dir——旧布局下任何用户态进程替换
// data_dir/bin 的二进制与校验文件再 stop/start 即可以 root 执行任意代码。
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

	"github.com/chaojimaimi/tradewind/internal/core"
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

// Install 执行 helper 安装子命令
// （`tradewind install --data-dir <p> --data-uid <n> --exec-dir <e>`）：校验 argv →
// root 内核副本一致性校验 → 写 service.json + daemon plist → bootout（幂等，失败
// 忽略）→ bootstrap。必须 root（提权脚本调用；非 root 直接报错，绝不静默降级为
// 用户态写系统目录）。
func Install(dataDir string, dataUID int, execDir string, stdout io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("service: install must run as root (via `tradewind tun install`)")
	}
	return installDaemon(installPlan{
		dataDir:          dataDir,
		dataUID:          dataUID,
		execDir:          execDir,
		configDir:        DefaultConfigDir,
		launchDaemonsDir: "/Library/LaunchDaemons",
		logsDir:          "/Library/Logs",
	}, realLaunchctl{}, stdout)
}

// installPlan 是安装落点参数（生产=系统目录；测试指向临时目录）。
type installPlan struct {
	dataDir          string // 用户数据目录（runtime.yaml/sock/log 仍在此——配置面）
	dataUID          int
	execDir          string // 执行目录（root 属主：helper 副本 + bin/mihomo + bin/mihomo.sha256）
	configDir        string // 锚目录（service.json）
	launchDaemonsDir string // plist 落点的系统目录
	logsDir          string // 服务日志落点的系统目录
}

// installDaemon 安装核心（目录落点参数化——测试指向临时目录，绝不触碰真实系统
// 目录/launchd）。先校验后落盘：任一校验失败不写任何文件。
func installDaemon(p installPlan, lc launchctlRunner, stdout io.Writer) error {
	cfg, plist, err := planInstall(p)
	if err != nil {
		return err
	}
	if err := writeInstallFiles(p, cfg, plist); err != nil {
		return err
	}
	if err := lc.Run("bootout", "system/"+DaemonLabel); err != nil {
		fmt.Fprintf(stdout, "service: bootout skipped (%v)\n", err) // 未加载时的幂等分支
	}
	plistPath := filepath.Join(p.launchDaemonsDir, DaemonLabel+".plist")
	if err := lc.Run("bootstrap", "system", plistPath); err != nil {
		return fmt.Errorf("service: bootstrap daemon: %w", err)
	}
	fmt.Fprintf(stdout, "service: installed (helper=%s)\n", cfg.HelperPath)
	return nil
}

// planInstall 校验参数并生成锚内容（service.json 结构 + plist 字节）。
// LPE 纵深防御在两层：此处对 exec_dir 的显式拒绝 + ConfigFile.validate 对 core_bin
// 的通用拒绝（服务启动闸同样生效——旧布局锚直接 fail-closed）。
func planInstall(p installPlan) (ConfigFile, []byte, error) {
	if !filepath.IsAbs(p.dataDir) || p.dataUID <= 0 {
		return ConfigFile{}, nil, fmt.Errorf("service: invalid install args (data_dir=%q must be absolute, data_uid=%d must be > 0)", p.dataDir, p.dataUID)
	}
	if !filepath.IsAbs(p.execDir) {
		return ConfigFile{}, nil, fmt.Errorf("service: invalid install args (exec_dir=%q must be absolute)", p.execDir)
	}
	if p.configDir == "" || p.launchDaemonsDir == "" || p.logsDir == "" {
		return ConfigFile{}, nil, errors.New("service: invalid install args (system dirs must be set)")
	}
	if pathWithin(p.execDir, p.dataDir) {
		return ConfigFile{}, nil, fmt.Errorf("service: exec_dir %q is inside data_dir %q (user-writable; the root-owned copies must live outside it)", p.execDir, p.dataDir)
	}
	cfg := ConfigFile{
		AllowedUIDs: []int{p.dataUID},
		DataDir:     p.dataDir,
		HelperPath:  HelperPathIn(p.execDir),
		CoreBin:     CorePathIn(p.execDir),
		Sidecar:     SidecarPathIn(p.execDir),
		Runtime:     filepath.Join(p.dataDir, "runtime.yaml"),
		Sock:        filepath.Join(p.dataDir, "core.sock"),
		Log:         filepath.Join(p.dataDir, "logs", "core-service.log"),
		DataUID:     p.dataUID,
	}
	if err := cfg.validate(); err != nil {
		return ConfigFile{}, nil, err
	}
	// 写锚前校验 root 副本一致性（提权脚本刚拷贝的三件套；数据目录侧的
	// bin/mihomo 与 mihomo.sha256 不一致在此暴露，而不是等服务 spawn 时才失败）
	if err := core.VerifyBinary(cfg.CoreBin, cfg.Sidecar); err != nil {
		return ConfigFile{}, nil, fmt.Errorf("service: root core copy failed verification: %w", err)
	}
	plist, err := daemonPlist(cfg.HelperPath, filepath.Join(p.logsDir, "tradewind-service.log"))
	if err != nil {
		return ConfigFile{}, nil, err
	}
	return cfg, plist, nil
}

// writeInstallFiles 写 service.json（0644）+ daemon plist（0644）。
func writeInstallFiles(p installPlan, cfg ConfigFile, plist []byte) error {
	if err := os.MkdirAll(p.configDir, 0o755); err != nil {
		return fmt.Errorf("service: mkdir %s: %w", p.configDir, err)
	}
	if err := os.MkdirAll(p.launchDaemonsDir, 0o755); err != nil {
		return fmt.Errorf("service: mkdir %s: %w", p.launchDaemonsDir, err)
	}
	if err := writeJSONFile(filepath.Join(p.configDir, "service.json"), cfg); err != nil {
		return err
	}
	target := filepath.Join(p.launchDaemonsDir, DaemonLabel+".plist")
	// plist 非凭据（无 secret/URL），launchd 惯例 644
	if err := os.WriteFile(target, plist, 0o644); err != nil {
		return fmt.Errorf("service: write plist %s: %w", target, err)
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
