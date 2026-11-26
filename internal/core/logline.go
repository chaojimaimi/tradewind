// logline.go：core 包自身的运行日志行助手（c8 ①/⑤）。
//
// WHY 与 internal/service 的 svcErrf/svcOutf 同格式但独立实现：分层红线——core 是
// 被 service 依赖的底层包，反向 import service 会成环。两包的运行日志又落进同一批
// 日志文件（service.log / 用户态形态的 stdout），排障要能按时间戳对齐两包产出的事件
// 时间线（2026-11-26 真机事故：Monitor 静默自愈出的内核在日志里零痕迹，定位耗时的
// 大头）。格式约定两处一致：行首 "2006-01-02 15:04:05 " 时间戳，内容前缀各用各的
// 身份（"core: " / "service: "）。漂移面由两处注释互指封堵（service 侧在 svclog.go）。
package core

import (
	"fmt"
	"os"
	"time"
)

// logTimestamp 与 service 包 svclog.go 的格式常量一致（见文件头的格式约定）。
const logTimestamp = "2006-01-02 15:04:05"

// coreOutf 向 os.Stdout 写一行带时间戳的 core 运行日志（内容自带 "core: " 前缀，
// 行尾换行由本函数统一补）。
func coreOutf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "%s %s\n", time.Now().Format(logTimestamp), fmt.Sprintf(format, args...))
}

// coreErrf 向 os.Stderr 写一行带时间戳的 core 运行日志（错误与诊断行；读 os.Stderr
// 变量而非缓存，与 service 包 svcErrf 的测试缝语义一致）。守护形态下 stderr 与
// stdout 都落 service.log，错误行同样要带时间戳才能与 launchd 时间线对齐。
func coreErrf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format(logTimestamp), fmt.Sprintf(format, args...))
}
