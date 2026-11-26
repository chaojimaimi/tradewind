// normalize_echo.go：键名回显进拒绝理由之前的净化（I6——错误文本会进 root 属主的
// service.log，绝不能让键名成为日志注入或超长刷屏的载体）。
package service

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// keyEchoBytes 键名回显截断（错误信息红线）
const keyEchoBytes = 64

// sanitizeName 键名回显前的清洁：非可打印字符替换为 '?'，截断到 64 字节。回显的
// 是"键在哪"，不能让键名成为日志注入或超长刷屏的载体。
//
// WHY 用 IsPrint 而不是 IsControl：控制字符只占一小类。双向覆盖符（U+202A–U+202E）
// 能让日志里一行文本的视觉顺序与实际字节顺序不一致，零宽字符与行/段分隔符
// （U+2028/U+2029）同理能骗过"一行一条"的阅读与 grep——这些都不是控制字符，但都
// 不该原样进 root 属主的日志。只放行字母/数字/标点/符号与 ASCII 空格。
func sanitizeName(name string) string {
	var b strings.Builder
	size := 0
	for _, r := range name {
		if !isEchoable(r) {
			r = '?'
		}
		n := utf8.RuneLen(r)
		if size+n > keyEchoBytes {
			break
		}
		b.WriteRune(r)
		size += n
	}
	return b.String()
}

// isEchoable 判定字符可原样回显（RuneError 兼作非法 UTF-8 字节的占位）。
func isEchoable(r rune) bool {
	return r != utf8.RuneError && (r == ' ' || unicode.IsPrint(r))
}
