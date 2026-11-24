// tun_install_check.go：特权服务安装态检查（批⑥ 真机 fold-in）——锚读取、两项
// 陈旧检测（helper / root 内核副本）、可达性探测、提权安装后的安装验证。
package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
)

var (
	// tunServiceProbeTimeout 可达性探测/安装验证的单次请求超时。
	tunServiceProbeTimeout = 3 * time.Second
	// tunInstallVerifyWindow 提权安装后等待服务可达的窗口（变量缝：测试缩短）。
	tunInstallVerifyWindow = 12 * time.Second
	// tunDaemonLogPath 服务自身日志（plist StandardOutPath；安装失败诊断读尾部）。
	// 批⑥b §4.6：日志在 root 属主的 <anchor>/logs/ 下，属主是数据目录用户、0600
	// ——用户仍可读自己的服务日志（安装失败的诊断面）。
	tunDaemonLogPath = filepath.Join(service.DefaultLogsDir, service.ServiceLogName)
)

// loadTunAnchor 读取并校验 service.json 锚。锚为 root 属主 0644（/Library/
// Application Support 下用户不可写）——helper_path/core_bin 是可信来源。旧布局锚
// （缺 layout 键或 ≠3；batch⑥b 前的 runtime/sock/log 都在数据目录）被
// service.ConfigFile.validate 拒绝 → 读不出 → 调用方按陈旧/无效处理。
func loadTunAnchor() (service.ConfigFile, bool) {
	cfg, err := service.LoadConfigFile(tunServiceConfigPath)
	if err != nil {
		return service.ConfigFile{}, false
	}
	return cfg, true
}

// sha256FilesEqual 两文件 sha256 相等（任一侧缺失/不可读 = 不等）。
func sha256FilesEqual(a, b string) bool {
	want, err := sha256SumFile(a)
	if err != nil {
		return false
	}
	got, err := sha256SumFile(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(want, got)
}

// helperBinaryMatches 安装位 helper（锚 helper_path）== 当前 CLI 的 sha256（R4
// self-update 陈旧检测）。锚缺 helper_path（旧布局）或任一侧不可读 = 陈旧。
func helperBinaryMatches() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return helperBinaryMatchesCLI(exe)
}

// helperBinaryMatchesCLI 是 helperBinaryMatches 的可注入形态（cliPath = 比对基准）。
func helperBinaryMatchesCLI(cliPath string) bool {
	cfg, ok := loadTunAnchor()
	return ok && cfg.HelperPath != "" && sha256FilesEqual(cliPath, cfg.HelperPath)
}

// coreBinaryMatches root 内核副本（锚 core_bin）== 数据目录 bin/mihomo 的 sha256。
// self-update --core 更新的是数据目录那份，root 副本停在旧版——不拦则服务以旧
// 内核跑新 runtime.yaml，行为不可预期。
func coreBinaryMatches(home paths.Home) bool {
	cfg, ok := loadTunAnchor()
	return ok && sha256FilesEqual(home.Bin(), cfg.CoreBin)
}

// tunServiceReachable 特权服务可达性探测（tun on 前置检查：不可达时在杀旧 start
// 之前拦下，避免白折腾一次用户态 core 重启）。
func tunServiceReachable() bool {
	cl, err := tunNewClient(tunServiceProbeTimeout)
	if err != nil {
		return false
	}
	_, err = cl.Status()
	return err == nil
}

// tunWaitForServiceReady 提权安装后的安装验证：窗口内轮询服务 status，可达即
// 通过；超时 → 报错并附服务日志尾部（可读时）+ 系统日志排查提示（真机首测的
// 根因 AppleSystemPolicy 拦截只在那里可见）。
func tunWaitForServiceReady() error {
	deadline := time.Now().Add(tunInstallVerifyWindow)
	for {
		if cl, err := tunNewClient(tunServiceProbeTimeout); err == nil {
			if _, err := cl.Status(); err == nil {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("app: 特权服务安装后 %s 内不可达（提权脚本已结束但服务未起来）%s；"+
		"可查 `log show --last 2m --predicate 'eventMessage CONTAINS \"AppleSystemPolicy\"'`",
		tunInstallVerifyWindow, tunDaemonLogTail())
}

// daemonLogTailBytes 是安装失败诊断携带的服务日志尾部上限。
const daemonLogTailBytes = 2000

// tunDaemonLogTail 读服务日志尾部（不可读/为空 → 空串）。可读性依据（批⑥b
// §4.6）：日志在 root 属主的 <anchor>/logs/ 下，但文件属主就是数据目录用户本人、
// 0600——Install 预创建时经 fd 收敛，所以用户读得到自己的服务日志。
func tunDaemonLogTail() string {
	f, err := os.Open(tunDaemonLogPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	offset := info.Size() - daemonLogTailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return "；服务日志尾部:\n" + strings.TrimSpace(string(buf))
}

// tunBinaryStatusLines `tun status` 的两项陈旧状态（helper / root 内核副本）。
func tunBinaryStatusLines(home paths.Home) []string {
	exe, err := os.Executable()
	if err != nil {
		exe = "" // 解析失败：helper 一律按陈旧呈现（sha256FilesEqual 对空路径读不出）
	}
	return tunBinaryStatusLinesFor(home, exe)
}

// tunBinaryStatusLinesFor 是 tunBinaryStatusLines 的可注入形态（cliPath = helper
// 的比对基准）。
func tunBinaryStatusLinesFor(home paths.Home, cliPath string) []string {
	cfg, ok := loadTunAnchor()
	if !ok {
		return []string{
			"helper:     unknown（service.json 不可读或为旧布局）",
			"warning: 服务锚无效或为旧布局（内核副本曾在用户可写目录），请执行 `tradewind tun reinstall`",
		}
	}
	helperCurrent := sha256FilesEqual(cliPath, cfg.HelperPath)
	coreCurrent := sha256FilesEqual(home.Bin(), cfg.CoreBin)
	lines := []string{
		fmt.Sprintf("helper:     %s（%s）", currentWord(helperCurrent), cfg.HelperPath),
		fmt.Sprintf("core copy: %s（%s）", currentWord(coreCurrent), cfg.CoreBin),
	}
	if !helperCurrent {
		lines = append(lines, "warning: 服务二进制已陈旧（self-update 后），请执行 `tradewind tun reinstall`")
	}
	if !coreCurrent {
		lines = append(lines, "warning: 服务内核副本已陈旧（self-update --core 后），请执行 `tradewind tun reinstall`")
	}
	return lines
}

func currentWord(ok bool) string {
	if ok {
		return "up to date"
	}
	return "stale"
}
