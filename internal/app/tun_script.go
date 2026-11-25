// tun_script.go：提权脚本的纯函数生成器（批⑥ §3.3；真机 fold-in 改布局）。
//
// 产物是 POSIX sh（经 /bin/sh 执行，不用数组/local 等 bashism——候选路径含空格，
// 不走 "空格分隔字符串遍历"）。系统路径经 tunScriptLayout 注入：生产=固定常量，
// 行为测试指向临时目录（绝不触碰真实系统目录/提权/launchd）。
package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/service"
)

// tunScriptLayout 是提权脚本涉及的系统路径。
type tunScriptLayout struct {
	// anchorDir 锚目录（service.json/core.pid；同时也是第一执行目录候选）。
	anchorDir string
	// legacy 遗留 helper 位置（/Library/PrivilegedHelperTools/<label>）——旧布局
	// 落点，受 Gatekeeper 执行评估拦截；不再落位，安装/卸载脚本均清理。
	legacy string
	// candidates 执行目录候选（按序探测；每个内放 tradewind-service、bin/mihomo、
	// bin/mihomo.sha256 三个 root 属主文件）。
	candidates []string

	// 以下仅卸载脚本使用。
	plist      string   // LaunchDaemon plist
	logsDir    string   // 锚目录内的日志目录（service.log / core.log / core.log.1）
	anchorSock string   // 锚目录内的代理入口 socket（服务被 kill -9 时的残留）
	corePID    string   // 锚目录内的 core.pid（与 service.json 同为锚文件）
	rmdirDirs  []string // 可能由安装脚本新建的父目录（仅在空时回收）
}

// installScriptLayout 安装脚本的生产布局，候选换成过滤后的安全集（review P2）。
// 卸载脚本不受影响：它仍遍历全部候选做清理。
func installScriptLayout(candidates []string) tunScriptLayout {
	lay := productionTunScriptLayout()
	lay.candidates = candidates
	return lay
}

// tunSafeExecDirCandidates 生成安装脚本前的候选过滤（用户态 stat）：祖先目录不安全
// 的候选不进脚本；一个都不剩 → 报错带各候选被排除的原因，不生成脚本。包级缝隙——
// 经 productionTunEnv 的测试不依赖本机系统目录的实际属主。
var tunSafeExecDirCandidates = func() ([]string, error) {
	safe, rejected := service.SafeExecDirCandidates()
	return composeSafeCandidates(safe, rejected)
}

// composeSafeCandidates 是 tunSafeExecDirCandidates 的纯函数形态（"一个不剩"时的
// 报错文案在此拼装）。
func composeSafeCandidates(safe []string, rejected []service.ExecDirCandidateRejection) ([]string, error) {
	if len(safe) > 0 {
		return safe, nil
	}
	reasons := make([]string, 0, len(rejected))
	for _, r := range rejected {
		reasons = append(reasons, fmt.Sprintf("%s（%v）", r.Candidate, r.Reason))
	}
	return nil, fmt.Errorf("app: 没有祖先目录安全的安装位置（执行目录及其各级祖先目录须归 root 所有且对 group/other 不可写）：%s；"+
		"修正对应目录的属主/权限后重试", strings.Join(reasons, "；"))
}

// productionTunScriptLayout 生产布局（路径全部来自 service 包常量）。
func productionTunScriptLayout() tunScriptLayout {
	return tunScriptLayout{
		anchorDir:  service.DefaultConfigDir,
		legacy:     service.HelperInstallPath,
		candidates: service.ExecDirCandidates(),
		plist:      service.DaemonPlistPath,
		logsDir:    service.DefaultLogsDir,
		anchorSock: service.DefaultBrokerSock,
		corePID:    service.DefaultCorePIDPath,
		rmdirDirs:  []string{service.LibexecDir},
	}
}

// scriptLines 把脚本切成非空行（内容断言用）。
func scriptLines(script string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// 候选清理函数（两脚本共用）：一律显式文件路径——红线"禁止 rm -rf"；目录仅在
// 空时移除（锚目录若还有别的文件则保留）。
const tunScriptCleanHelper = `
clean_candidate() {
	rm -f "$1/tradewind-service" "$1/bin/mihomo" "$1/bin/mihomo.sha256"
	rmdir "$1/bin" 2>/dev/null
	rmdir "$1" 2>/dev/null
	return 0
}
`

// installSources 是安装脚本的输入来源及其在命令开始时算出的摘要（批⑥b §4.5）：
// cliSHA = sha256(CLI)、coreSHA = sha256(<data_dir>/bin/mihomo)——嵌入脚本后由
// root 侧在拷贝完成时复核，关闭"授权弹窗期间替换源文件"的窗口。
type installSources struct {
	cliPath string
	cliSHA  string // sha256(CLI)
	coreSHA string // sha256(<data_dir>/bin/mihomo)
	dataDir string
	dataUID int
}

// tunInstallScript 按全候选的生产布局生成安装脚本（纯函数）。生产编排不直接用它
// ——tunInstallWith 先过滤候选再调 tunInstallScriptFor；本函数服务于内容断言
// （全候选布局即脚本的最大形态）。
func tunInstallScript(src installSources, allowInsecureTUN bool) string {
	return tunInstallScriptFor(src, productionTunScriptLayout(), allowInsecureTUN)
}

// tunInstallScriptFor 生成安装提权脚本（纯函数）：
//
//	⓪ 源文件存在性预检（缺失 → exit 4，不误报成"系统策略拦截"）
//	① 锚目录（root:wheel 0755）+ 清理遗留位置 helper
//	② 逐候选：mkdir bin → rm+cp 三文件 → 摘要复核（批⑥b §4.5：校验的是拷贝后的
//	   目标文件——授权弹窗等待期间源文件可能被同账号进程替换；失配 → 清理该候选
//	   + exit 5，不试下一候选——源文件是同一份，换位置没有意义）→ root:wheel →
//	   0755/0755/0644 → 执行自测（`tradewind-service version` 与 `bin/mihomo -v`
//	   均成功才算该位置可用）；被拦 → stderr 一行 + 清理该候选 + 下一处；
//	   全部被拦 → stderr 说明 + exit 3
//	③ 清理其余候选里的陈旧三文件（历史安装可能落在别的候选）
//	④ `<exec-dir>/tradewind-service install --data-dir --data-uid --exec-dir
//	   [--allow-insecure-tun]`（旗标仅当 CLI 侧联锁放行时追加——批⑥b §4.5：
//	   helper 自己判不了环境变量，由看得到 TRADEDWIND_ALLOW_INSECURE_TUN 的
//	   用户态 CLI 决定是否传入）
func tunInstallScriptFor(src installSources, lay tunScriptLayout, allowInsecureTUN bool) string {
	q := shQuote
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# tradewind 特权服务安装脚本（自动生成；经 osascript 以 root 执行；POSIX sh）\n")
	b.WriteString("set -u\n")
	b.WriteString("umask 022\n\n")
	fmt.Fprintf(&b, "CLI=%s\n", q(src.cliPath))
	fmt.Fprintf(&b, "DATA=%s\n", q(src.dataDir))
	fmt.Fprintf(&b, "DATA_UID=%d\n", src.dataUID)
	// 摘要绑定（批⑥b §4.5）：命令开始时算出的期望摘要；root 侧校验的是拷贝后的
	// 目标文件（已落进 root 属主目录），弹窗期间源文件之后的替换无关紧要
	fmt.Fprintf(&b, "CLI_SHA=%s\n", q(src.cliSHA))
	fmt.Fprintf(&b, "CORE_SHA=%s\n", q(src.coreSHA))
	b.WriteString(`CORE="$DATA/bin/mihomo"
SIDECAR="$DATA/bin/mihomo.sha256"
`)
	fmt.Fprintf(&b, "ANCHOR=%s\n", q(lay.anchorDir))
	fmt.Fprintf(&b, "LEGACY=%s\n", q(lay.legacy))
	b.WriteString("EXE=''\n\n")
	b.WriteString(`
fail() {
	echo "tradewind: $1" >&2
	exit "$2"
}
# 安装在"锚目录已建、尚未写锚"阶段失败时的收场：锚目录若是本次脚本新建的空目录
# 就回收（rmdir 只删空目录——既有安装留下的非空锚原样保留）。不回收的话它会一直
# 留在 /Library 下，而卸载的判据是 service.json 的存在性，永远看不到它。
abort() {
	rmdir "$ANCHOR" 2>/dev/null
	exit "$1"
}
`)
	b.WriteString(tunScriptCleanHelper)
	b.WriteString(`
# ⓪ 源文件预检：缺失属用户侧问题，须与"位置被系统策略拦截"区分开
[ -f "$CLI" ] || fail "缺少 CLI 二进制 $CLI" 4
[ -f "$CORE" ] || fail "数据目录缺少内核 $CORE（内核尚未安装？）" 4
[ -f "$SIDECAR" ] || fail "数据目录缺少内核校验文件 $SIDECAR" 4

# ① 锚目录 + 遗留位置清理
mkdir -p "$ANCHOR" || fail "无法创建锚目录 $ANCHOR" 1
chown root:wheel "$ANCHOR" || fail "无法设置锚目录属主 $ANCHOR" 1
chmod 0755 "$ANCHOR" || fail "无法设置锚目录权限 $ANCHOR" 1
rm -f "$LEGACY"

# ② 单个候选的落位 + 执行自测
try_candidate() {
	CAND="$1"
	mkdir -p "$CAND/bin" || return 1
	chown root:wheel "$CAND" "$CAND/bin" || return 1
	chmod 0755 "$CAND" "$CAND/bin" || return 1
	# rm+cp 换新 inode（cp 原地覆写运行中二进制 = ETXTBSY）
	rm -f "$CAND/tradewind-service" "$CAND/bin/mihomo" "$CAND/bin/mihomo.sha256"
	if ! cp "$CLI" "$CAND/tradewind-service" || ! cp "$CORE" "$CAND/bin/mihomo" || ! cp "$SIDECAR" "$CAND/bin/mihomo.sha256"; then
		echo "tradewind: 拷贝到 $CAND 失败，尝试下一处" >&2
		clean_candidate "$CAND"
		return 1
	fi
	# 摘要复核（批⑥b §4.5）：WHY 校验拷贝后的目标文件——授权弹窗等待期间，同一
	# 账号的进程可以替换用户目录里的源文件；此刻三文件已落进 root 属主目录，源
	# 文件之后的替换无关紧要。失配即中止（exit 5，不试下一候选——源文件是同一
	# 份，换位置没有意义）。
	if ! printf '%s  %s\n' "$CLI_SHA" "$CAND/tradewind-service" | /usr/bin/shasum -a 256 -c - >/dev/null 2>&1; then
		echo "tradewind: CLI 副本的摘要与开始安装时不一致（授权弹窗期间源文件被替换？），中止" >&2
		clean_candidate "$CAND"
		abort 5
	fi
	if ! printf '%s  %s\n' "$CORE_SHA" "$CAND/bin/mihomo" | /usr/bin/shasum -a 256 -c - >/dev/null 2>&1; then
		echo "tradewind: 内核副本的摘要与开始安装时不一致（授权弹窗期间源文件被替换？），中止" >&2
		clean_candidate "$CAND"
		abort 5
	fi
	# sidecar 前 64 字符与内核摘要同口径比较：CORE_SHA 恒为小写（Go 侧 hex 编码），
	# 而校验文件的大写十六进制对 core.VerifyBinary 是合法的（它先转小写）——这里
	# 同样先转小写，否则大写 sidecar 会被误判成失配
	if [ "$(/usr/bin/head -c 64 "$CAND/bin/mihomo.sha256" | /usr/bin/tr 'A-F' 'a-f')" != "$CORE_SHA" ]; then
		echo "tradewind: 内核校验文件的摘要与开始安装时不一致（授权弹窗期间源文件被替换？），中止" >&2
		clean_candidate "$CAND"
		abort 5
	fi
	chown root:wheel "$CAND/tradewind-service" "$CAND/bin/mihomo" "$CAND/bin/mihomo.sha256" || { clean_candidate "$CAND"; return 1; }
	chmod 0755 "$CAND/tradewind-service" "$CAND/bin/mihomo" || { clean_candidate "$CAND"; return 1; }
	chmod 0644 "$CAND/bin/mihomo.sha256" || { clean_candidate "$CAND"; return 1; }
	# 执行自测：系统策略（Gatekeeper/ASP）按位置评估未公证二进制——两个二进制
	# 都能执行才算该位置可用；被 SIGKILL 的进程在这里表现为非零退出
	if "$CAND/tradewind-service" version >/dev/null 2>&1 && "$CAND/bin/mihomo" -v >/dev/null 2>&1; then
		EXE="$CAND"
		echo "tradewind: 安装位置 $CAND" >&2
		return 0
	fi
	echo "tradewind: $CAND 被系统策略拦截，尝试下一处" >&2
	clean_candidate "$CAND"
	return 1
}

`)
	b.WriteString("# ② 逐候选探测（顺序固定）；全部被拦 → exit 3\n")
	for _, cand := range lay.candidates {
		fmt.Fprintf(&b, "try_candidate %s || ", q(cand))
	}
	fmt.Fprintf(&b, "fail %s 3\n\n", q(allBlockedMessage(lay)))
	b.WriteString("# ③ 清理其余候选里的陈旧三文件\n")
	for _, cand := range lay.candidates {
		fmt.Fprintf(&b, `if [ "$EXE" != %s ]; then clean_candidate %s; fi`+"\n", q(cand), q(cand))
	}
	// ④ 显式确认旗标（批⑥b §4.5）：仅当 CLI 侧联锁放行时追加——`do shell script`
	// 不继承用户环境，helper 自己判不了 TRADEDWIND_ALLOW_INSECURE_TUN
	b.WriteString(`
# ④ 写锚 + 注册 daemon（plist/service.json 由 root 身份的 helper 自写）
"$EXE/tradewind-service" install --data-dir "$DATA" --data-uid "$DATA_UID" --exec-dir "$EXE"`)
	if allowInsecureTUN {
		b.WriteString(" --allow-insecure-tun")
	}
	b.WriteString("\nexit \"$?\"\n")
	return b.String()
}

// allBlockedMessage 全部候选被系统策略拦截时的 stderr 说明（真机 fold-in 2 补
// quarantine 提示）：浏览器下载后解压的二进制常带 com.apple.quarantine，cp 会把该
// 扩展属性带到副本上，三个候选会一并被拦——提示先对解压目录去 quarantine 再重试。
func allBlockedMessage(lay tunScriptLayout) string {
	return "所有候选安装位置均被 macOS 系统策略拦截（未公证二进制受限），未安装任何文件：" +
		strings.Join(lay.candidates, " / ") +
		"；若 CLI/内核二进制带 com.apple.quarantine 扩展属性（浏览器下载后解压常见），副本会继承并被拦" +
		"——先对解压目录执行 xattr -dr com.apple.quarantine <目录> 再重试"
}

// tunUninstallScript 生成生产卸载提权脚本（纯函数）。
func tunUninstallScript() string {
	return tunUninstallScriptFor(productionTunScriptLayout())
}

// tunUninstallScriptFor 生成卸载提权脚本（纯函数）：bootout → 逐候选探测 helper
// 并调用它的 purge（删除 root 专属内核工作目录——helper 还在位才能调用）→ rm -f
// plist/三份日志/锚文件/代理 socket 残留/遗留 helper → 逐候选清三文件并回收空目录
// → 回收 logs 与锚目录（仅空时）→ 回收可能由安装新建的父目录。
// 禁止 rm -rf：一律显式文件路径；末尾 exit 0（残留的非空目录不算失败）。
func tunUninstallScriptFor(lay tunScriptLayout) string {
	q := shQuote
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# tradewind 特权服务卸载脚本（自动生成；经 osascript 以 root 执行；POSIX sh）\n")
	b.WriteString("set -u\n")
	b.WriteString(tunScriptCleanHelper)
	fmt.Fprintf(&b, "\nlaunchctl bootout system/%s 2>/dev/null || true\n", service.DaemonLabel)
	// 逐候选调 helper 的 purge（批⑥b §4.1：内核产物含哈希文件名，脚本无法显式
	// 枚举；`|| true` 保证候选缺失/被拦时继续）。purge 必须排在 clean_candidate
	// 之前——clean 之后 helper 就没了
	b.WriteString("# 逐候选：helper 在位则调它的 purge（删除 root 专属内核工作目录）\n")
	for _, cand := range lay.candidates {
		helper := q(filepath.Join(cand, service.HelperFileName))
		fmt.Fprintf(&b, "[ -x %s ] && %s purge || true\n", helper, helper)
	}
	// rm -f 清单：三份日志（含轮转出的 .1）、两个锚文件、代理 socket 残留（服务被
	// kill -9 时来不及自摘）、遗留 helper
	fmt.Fprintf(&b, "rm -f %s %s %s %s %s %s %s %s\n",
		q(lay.plist),
		q(filepath.Join(lay.logsDir, service.ServiceLogName)),
		q(filepath.Join(lay.logsDir, service.CoreLogName)),
		q(filepath.Join(lay.logsDir, service.CoreLogName+".1")),
		q(filepath.Join(lay.anchorDir, "service.json")),
		q(lay.corePID),
		q(lay.anchorSock),
		q(lay.legacy))
	b.WriteString("# 逐候选：显式三文件 + 空目录回收（绝不 rm -rf）\n")
	for _, cand := range lay.candidates {
		fmt.Fprintf(&b, "clean_candidate %s\n", q(cand))
	}
	// logs 与锚目录仅在空时回收（purge 已删 service-data；锚里若有别的文件则保留）
	fmt.Fprintf(&b, "rmdir %s 2>/dev/null\n", q(lay.logsDir))
	fmt.Fprintf(&b, "rmdir %s 2>/dev/null\n", q(lay.anchorDir))
	b.WriteString("\n# 可能由安装脚本新建的父目录：仅在空时回收\n")
	for _, dir := range lay.rmdirDirs {
		fmt.Fprintf(&b, "rmdir %s 2>/dev/null\n", q(dir))
	}
	b.WriteString("exit 0\n")
	return b.String()
}
