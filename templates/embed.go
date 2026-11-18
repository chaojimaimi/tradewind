// Package templates 提供内置配置模板。
//
// go:embed 只能引用包目录内文件，故在 templates/ 目录放一个薄包装，
// 保持 config.yaml 位于方案 §2 规定的仓库根 templates/ 路径。
package templates

import _ "embed"

//go:embed config.yaml
var config []byte

// Config 返回内置模板原始字节。调用方不得修改返回切片。
func Config() []byte { return config }
