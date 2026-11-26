// svclog.go：service.log 的行格式助手（批⑥b c8 ①）。
//
// WHY 全部行带时间戳（2026-11-26 真机排障教训）：service.log 是排障的第一手证据，
// 无时间戳的行无法与 launchd/内核日志对齐时间线——那次定位恢复链断裂根因的最大
// 障碍。统一格式：行首 "2006-01-02 15:04:05 " 时间戳，之后才是既有内容（自带
// "service: " 前缀，保持不变）。WHY 不用 Go log 标志位：那是 2006/01/02 斜杠形态
// 且 LstdFlags 会落在前缀位——本包自己格式化，格式一处可控。时间戳本身无换行；
// 内容的单行纪律照旧由调用方保证（自由文本先过 sanitizeForLog 再进来）。
package service

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// logTimestamp 是 service.log 每行行首的时间戳格式（core 包 logline.go 同款——
// 两包的行落进同一批日志文件，排障要能按时间戳对齐两包产出的事件时间线）。
const logTimestamp = "2006-01-02 15:04:05"

// svcLinef 向 w 写一行带时间戳的日志：时间戳 + 空格 + 格式化内容 + 换行（换行由
// 本函数统一补，调用方的格式串不再自带）。
func svcLinef(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s %s\n", time.Now().Format(logTimestamp), fmt.Sprintf(format, args...))
}

// svcErrf 向 os.Stderr 写一行带时间戳的服务日志（错误与诊断行）。读取 os.Stderr
// 变量而非缓存：测试经 captureServiceLog 换变量截获。
func svcErrf(format string, args ...any) {
	svcLinef(os.Stderr, format, args...)
}

// svcOutf 向 os.Stdout 写一行带时间戳的服务日志（成功行，如 "reload pushed to
// kernel"）。
func svcOutf(format string, args ...any) {
	svcLinef(os.Stdout, format, args...)
}

// timestampWriter 给每一笔写入补时间戳前缀。
type timestampWriter struct{ w io.Writer }

// Write 按 io.Writer 约定整笔透传。约定调用方（log.Logger，flags 为空）每笔写恰好
// 一行、自带结尾换行——去掉结尾换行重新格式化再补回，行内内容原样保留。
func (t *timestampWriter) Write(p []byte) (int, error) {
	svcLinef(t.w, "%s", strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// svcLogWriter 把 w 包成带时间戳的 io.Writer。http.Server 的 ErrorLog 用它接
// log.New(w, "", 0)：保持 log 包的既有接线与单行格式（broker 的错误文本在上游
// 已过 sanitizeForLog），只补时间戳。
func svcLogWriter(w io.Writer) io.Writer {
	return &timestampWriter{w: w}
}
