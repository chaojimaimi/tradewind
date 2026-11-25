// install.go：helper 安装子命令（批⑥ §3.3；真机 fold-in 改布局；批⑥b §4.1 改
// root 专属目录布局；批⑥b §4.5 加显式确认旗标）。
//
// 提权脚本把 helper 副本 + 内核的 root 属主副本拷进执行目录并做执行自测后，调用
// 本包 Install——plist 与 service.json 由 root 身份的 helper 自写。{data_dir,
// data_uid, exec_dir, allow_insecure_tun} 经内联 argv 传入（argv 无秘密，符合红线；
// root 无法"自证"调用者数据目录，评审二轮 P2-c），不读用户可写文件（TOCTOU 不移回
// 配置面，R12）。
//
// LPE 修复（batch6 真机 fold-in）：service.json 的 core_bin/sidecar 指向执行目录下
// 的 root 属主副本，绝不指向用户可写的 data_dir——旧布局下任何用户态进程替换
// data_dir/bin 的二进制与校验文件再 stop/start 即可以 root 执行任意代码。
// 批⑥b（§4.1）：内核工作目录收进锚目录内的 service-data/（root:wheel 0700），服务
// 与内核日志收进锚目录内的 logs/——内核工作目录不再是用户数据目录。
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
	"syscall"

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
// （`tradewind install --data-dir <p> --data-uid <n> --exec-dir <e>
// [--allow-insecure-tun]`）：显式确认旗标在前（未确认即拒绝，不写任何文件、不碰
// launchd）→ 校验 argv → 执行目录归 root 所有 → root 内核副本一致性校验 → 建
// root 专属目录布局 → 写 service.json + daemon plist → bootout（幂等，失败忽略）
// → bootstrap。必须 root（提权脚本调用；非 root 直接报错，绝不静默降级为用户态
// 写系统目录）。
func Install(dataDir string, dataUID int, execDir string, allowInsecureTUN bool, stdout io.Writer) error {
	// 显式确认旗标（批⑥b §4.5，终审 P3-①；阶段一终闸，阶段二随联锁一并删除），
	// 先于 root 守卫：未确认 → 拒绝且零动作。WHY 由用户态 CLI 判而不是 helper：
	// `do shell script` 不继承用户环境，root 侧 helper 看不到
	// TRADEDWIND_ALLOW_INSECURE_TUN——只能由看得到该环境变量的 CLI 决定是否经
	// argv 传入这个旗标。
	if !allowInsecureTUN {
		return errors.New("service: 特权服务尚未完成加固（R16 提权边界），仅限本机自测；" +
			"如已知悉风险，请以 --allow-insecure-tun 显式传入确认")
	}
	if os.Geteuid() != 0 {
		return errors.New("service: install must run as root (via `tradewind tun install`)")
	}
	return installWith(installPlan{
		dataDir:          dataDir,
		dataUID:          dataUID,
		execDir:          execDir,
		configDir:        DefaultConfigDir,
		launchDaemonsDir: "/Library/LaunchDaemons",
		owner:            uidgid{uid: 0, gid: 0}, // 生产恒为 root:wheel
	}, realLaunchctl{}, stdout, verifyExecDirOwnership)
}

// installWith 是安装入口的可测形态：执行目录归属校验在前（注入——生产=
// verifyExecDirOwnership；非 root 测试环境经 installDaemon 直达或桩跳过），任一项
// 不达标则不写任何文件、不碰 launchd。
func installWith(p installPlan, lc launchctlRunner, stdout io.Writer, verifyOwnership func(string) error) error {
	if err := verifyOwnership(p.execDir); err != nil {
		return fmt.Errorf("service: refusing to install: %w", err)
	}
	return installDaemon(p, lc, stdout)
}

// uidgid 是属主收敛的目标。生产恒 {0, 0}（root:wheel）；测试填进程自身的
// {os.Getuid(), os.Getgid()}——非 root 进程 fchown 到别的 uid 必然 EPERM。
type uidgid struct {
	uid int
	gid int
}

// installPlan 是安装落点参数（生产=系统目录；测试指向临时目录）。
type installPlan struct {
	dataDir          string // 用户数据目录（服务对它只读：归一化输入与 geo 同步）
	dataUID          int    // 数据目录属主（两份日志的收敛属主）
	execDir          string // 执行目录（root 属主：helper 副本 + bin/mihomo + bin/mihomo.sha256）
	configDir        string // 锚目录（service.json；logs/ 与 service-data/ 由它推导）
	launchDaemonsDir string // plist 落点的系统目录
	owner            uidgid // 目录属主收敛的目标（生产 root:wheel；测试=进程自身）
}

// logsDir / coreHome 一律由锚目录推导（与生产常量、validate 的派生式、purge 的删除
// 目标同一份来源——安装落点和服务启动闸不会各说各话）。
func (p installPlan) logsDir() string  { return LogsDirIn(p.configDir) }
func (p installPlan) coreHome() string { return CoreHomeIn(p.configDir) }

// installDaemon 安装核心（目录落点参数化——测试指向临时目录，绝不触碰真实系统
// 目录/launchd）。先校验后落盘：任一校验失败不写任何锚文件、不碰 launchd。
func installDaemon(p installPlan, lc launchctlRunner, stdout io.Writer) error {
	cfg, plist, err := planInstall(p)
	if err != nil {
		return err
	}
	if err := prepareLayout(p); err != nil {
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
	if p.configDir == "" || p.launchDaemonsDir == "" {
		return ConfigFile{}, nil, errors.New("service: invalid install args (system dirs must be set)")
	}
	if pathWithin(p.execDir, p.dataDir) {
		return ConfigFile{}, nil, fmt.Errorf("service: exec_dir %q is inside data_dir %q (user-writable; the root-owned copies must live outside it)", p.execDir, p.dataDir)
	}
	// v3（批⑥b §4.1）：内核工作目录 = <configDir>/service-data，日志 = <configDir>/logs，
	// 代理入口 = <configDir>/core.sock，归一化输入 = 数据目录 runtime.yaml
	cfg := ConfigFile{
		Layout:      3,
		AllowedUIDs: []int{p.dataUID},
		DataDir:     p.dataDir,
		HelperPath:  HelperPathIn(p.execDir),
		CoreBin:     CorePathIn(p.execDir),
		Sidecar:     SidecarPathIn(p.execDir),
		CoreHome:    p.coreHome(),
		KernelSock:  KernelSockIn(p.coreHome()),
		BrokerSock:  BrokerSockIn(p.configDir),
		CoreLog:     filepath.Join(p.logsDir(), CoreLogName),
		UserRuntime: filepath.Join(p.dataDir, "runtime.yaml"),
		DataUID:     p.dataUID,
	}
	if err := cfg.validate(p.configDir); err != nil {
		return ConfigFile{}, nil, err
	}
	// 写锚前校验 root 副本一致性（提权脚本刚拷贝的三件套；数据目录侧的
	// bin/mihomo 与 mihomo.sha256 不一致在此暴露，而不是等服务 spawn 时才失败）
	if err := core.VerifyBinary(cfg.CoreBin, cfg.Sidecar); err != nil {
		return ConfigFile{}, nil, fmt.Errorf("service: root core copy failed verification: %w", err)
	}
	plist, err := daemonPlist(cfg.HelperPath, filepath.Join(p.logsDir(), ServiceLogName))
	if err != nil {
		return ConfigFile{}, nil, err
	}
	return cfg, plist, nil
}

// prepareLayout 建立 root 专属目录布局：锚目录 0755、logs 0755、service-data 0700，
// 再预创建两份日志。WHY 一律走 fd（fchown/fchmod）而不是按路径 chown/chmod：按路径
// 的 chown/chmod 会跟随符号链接——条目哪怕被换成指向系统文件的链接，root 的一次
// 路径操作就把属主/权限送出去；"服务里不存在按路径 chown/chmod"应当是一条能静态
// 检查的规则（本步锚目录尚由 root 独占可写，窗口实际不可利用，统一是为了规则简单）。
// 在 planInstall 全部校验通过之后、写任何锚文件之前执行：任一步失败不写
// service.json/plist、不碰 launchd。
func prepareLayout(p installPlan) error {
	dirs := []struct {
		path string
		perm os.FileMode
	}{
		{p.configDir, 0o755},
		{p.logsDir(), 0o755},
		{p.coreHome(), 0o700},
	}
	for _, d := range dirs {
		if err := ensureOwnedDir(d.path, d.perm, p.owner); err != nil {
			return err
		}
	}
	for _, name := range []string{ServiceLogName, CoreLogName} {
		if err := ensureLogFile(filepath.Join(p.logsDir(), name), p.dataUID); err != nil {
			return err
		}
	}
	return nil
}

// ensureOwnedDir 确保单个目录存在且属主/权限收敛：已存在先 Lstat——符号链接或
// 不是目录 → 报错（不删除、不跟随）；再经 fd 收敛（O_NOFOLLOW 打开后 fchown +
// fchmod）。打开失败直接返回错误，绝不回落到按路径的 chown/chmod。
func ensureOwnedDir(path string, perm os.FileMode, owner uidgid) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("service: refusing to install: %s is a symbolic link (not following)", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("service: refusing to install: %s exists and is not a directory", path)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, perm); err != nil {
			return fmt.Errorf("service: mkdir %s: %w", path, err)
		}
	} else {
		return fmt.Errorf("service: lstat %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("service: open dir %s (O_NOFOLLOW): %w", path, err)
	}
	defer f.Close()
	if err := f.Chown(owner.uid, owner.gid); err != nil {
		return fmt.Errorf("service: chown %s: %w", path, err)
	}
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("service: chmod %s: %w", path, err)
	}
	return nil
}

// ensureLogFile 预创建一份日志。WHY 属主给用户（fchown data_uid）：macOS 普通账号
// 的主组都是 staff，若按惯例给 0640 root:staff 就等于全员可读——给 data_uid 0600
// 才把可读面收到该用户与 root；日志目录是 root 属主，用户无法替换目录项，能做的
// 只是改自己那份日志的内容与权限。launchd 对已存在的 StandardOutPath 追加写，
// 权限得以保持。
func ensureLogFile(path string, dataUID int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		defer f.Close()
		if err := f.Chown(dataUID, -1); err != nil {
			return fmt.Errorf("service: chown %s: %w", path, err)
		}
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("service: create log %s: %w", path, err)
	}
	// 已存在（重装）：O_NOFOLLOW 打开后经 fd 校正，幂等。打开失败（含 ELOOP——路径
	// 被换成符号链接）直接返回错误，绝不按路径跟随。O_NONBLOCK：路径上若是 FIFO，
	// 只写打开会阻塞到有读方为止——带上它立即返回，再由下面的"必须普通文件"拦下。
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("service: open log %s (O_NOFOLLOW): %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("service: stat log %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service: %s is not a regular file (refusing)", path)
	}
	if err := f.Chown(dataUID, -1); err != nil {
		return fmt.Errorf("service: chown %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("service: chmod %s: %w", path, err)
	}
	return nil
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
// 批⑥ §3.1）+ 日志落锚目录 logs/（批⑥b §4.6：迁出 admin 可写的 /Library/Logs，R22；
// R3：service.json 非法时日志可观察）。
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
