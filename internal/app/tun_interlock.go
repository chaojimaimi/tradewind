// tun_interlock.go：TUN 特权服务联锁（M3 batch6 interlock）。
//
// 背景（R16，已用真实内核实测确认）：root 内核以用户可写目录作工作目录，geo/provider
// 下载会顺悬空符号链接在任意路径创建文件——任何以当前账号运行的程序都可借它取得
// root。加固另行规划；在此之前，仓库里的 `tun on` 一旦成功就会装上带该漏洞的 root
// 服务。故对一切会安装或启用特权服务的入口默认拒绝，显式确认风险后才放行。
//
// 边界：逃生口与诊断（tun off / tun status / tun uninstall）不受联锁约束。
package app

import (
	"errors"
	"fmt"
	"os"
)

// tunInterlockAllowEnv 是显式确认风险的环境变量；仅精确值 "1" 放行。
const (
	tunInterlockAllowEnv = "TRADEDWIND_ALLOW_INSECURE_TUN"
	tunInterlockAllowVal = "1"
)

// errTunInterlocked 是联锁拒绝的哨兵（完整文案见 requireTunInterlockOpen）。
var errTunInterlocked = errors.New("app: TUN 特权服务暂未开放")

// tunInterlockEnv 读取确认环境变量（生产 os.LookupEnv；测试注入，不依赖进程真实
// 环境）。返回 (值, 是否设置)。
var tunInterlockEnv = func() (string, bool) { return os.LookupEnv(tunInterlockAllowEnv) }

// tunInterlockOpen 报告用户是否已显式确认风险：变量已设置且恰为 "1"。未设置或其他
// 任何取值（"0"、"true"、" 1"……）一律视为未确认。
func tunInterlockOpen() bool {
	value, set := tunInterlockEnv()
	return set && value == tunInterlockAllowVal
}

// requireTunInterlockOpen 是受控入口（tun on / install / reinstall）的第一道闸：
// 联锁关闭 → 返回拒绝。调用方必须把它放在一切动作之前——不持 mutation lock、不
// 提权、不写 settings。cmd 仅用于把出路文案里的命令补全成用户实际敲的那一条。
func requireTunInterlockOpen(cmd string) error {
	if tunInterlockOpen() {
		return nil
	}
	return fmt.Errorf("%w：root 内核仍以用户可写目录为工作目录，任何以当前账号运行的程序都可借它取得 root"+
		"（提权边界加固中）。本机自测请显式确认风险后重试：%s=%s tradewind tun %s",
		errTunInterlocked, tunInterlockAllowEnv, tunInterlockAllowVal, cmd)
}

// tunInterlockStatusLine 是 `tun status` 里说明联锁状态的一行（批⑥b §4.7 终审
// P3-②：按联锁状态分两态——关闭如实标注受控命令默认拒绝与本机自测的出路，放行
// 如实标注"仅限本机自测"并劝阻日常使用；标签列与其余状态行同样补齐到 12 列）。
func tunInterlockStatusLine() string {
	if tunInterlockOpen() {
		return "interlock:  已显式放行（仅限本机自测）——tun on/install/reinstall 可用；加固完成前不要在日常使用中开启 TUN"
	}
	return fmt.Sprintf("interlock:  联锁关闭（R16 加固中）——tun on/install/reinstall 默认拒绝；本机自测需显式确认 %s=%s",
		tunInterlockAllowEnv, tunInterlockAllowVal)
}
