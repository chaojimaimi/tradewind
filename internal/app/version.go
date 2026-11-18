package app

import (
	"fmt"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/version"
)

// VersionReport 返回 `tradewind version` 的输出（M1-plan §5.3）：
// app 版本 +（内核可达时）内核版本。只读，不失败：内核不可达打印提示而非报错，
// 因为 version 命令的价值在版本可见性，不应依赖内核运行状态。
func VersionReport(home paths.Home) string {
	report := fmt.Sprintf("tradewind %s\n", version.Version)
	if v, ok := probeCore(home); ok {
		report += fmt.Sprintf("core %s\n", v)
	} else {
		report += "core not reachable\n"
	}
	return report
}
