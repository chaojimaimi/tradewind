// Package synth 将模板 + 订阅 + 设置合成为喂给内核的 runtime.yaml。
//
// 白名单语义（M0-plan §5.2）：订阅只取 proxies/proxy-groups/rules 三键，
// 防止机场订阅自带 tun/external-ui/端口等键污染 runtime。
// 实现用 yaml.Node 做键级覆盖：键序稳定（golden 测试的前提），订阅原文件只读。
package synth

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/templates"
	"gopkg.in/yaml.v3"
)

// subscriptionKeys 是订阅白名单键，按此顺序追加到模板之后（输出键序稳定）。
var subscriptionKeys = []string{"proxies", "proxy-groups", "rules"}

// Injection 是运行时注入项：settings 覆盖一切 + secret。
type Injection struct {
	Settings store.Settings
	Secret   string
	// Warnings 非空指针时收集增强链跳过/撞名告警（纯函数不打印，调用方落 stderr；
	// batch3 §3.2 未命中/撞名跳过不静默）。
	Warnings *[]string
}

// Synth 合成 runtime.yaml 字节流。
// 订阅缺失 proxies/proxy-groups/rules 中任一键时不报错——订阅合法性与 validate 层把关，
// 这里只做"有则取之"（例如用户自定义的最小 rules 文件）。
// enh 为增强链各层原文（nil = 无增强；空槽零开销，输出与无增强逐字节一致——golden 锚）。
func Synth(subscription []byte, inj Injection, enh *Enhance) ([]byte, error) {
	if err := inj.Settings.Validate(); err != nil {
		return nil, fmt.Errorf("synth: %w", err)
	}
	root, err := parseMapping(templates.Config())
	if err != nil {
		return nil, fmt.Errorf("synth: template: %w", err)
	}
	// 订阅为空（无 current 订阅，裸内核场景）→ 跳过订阅层，纯模板 + 注入项即产物；
	// 既有行为声明见 app.Synthesize（"无 current 订阅时合成纯模板"）。
	// WHY 修复：空订阅此前会因 parseMapping(空) 报 "top level is 0"——从未被覆盖的路径。
	if len(subscription) > 0 {
		subRoot, err := parseMapping(subscription)
		if err != nil {
			return nil, fmt.Errorf("synth: subscription: %w", err)
		}
		for _, key := range subscriptionKeys {
			if val, ok := mappingValue(subRoot, key); ok {
				// 模板不含这三键，实际走 append 分支；setMappingValue 的 replace 分支仅为完备性
				setMappingValue(root, key, val)
			}
		}
	}
	// 增强链（batch3 §3.2 顺序）：三槽在前、全局 merge 在后，settings 注入仍在链末最高优先
	if err := applyEnhanceSlots(root, enh); err != nil {
		return nil, err
	}
	if err := applyGlobalMerge(root, enhGlobalMerge(enh), inj.Warnings); err != nil {
		return nil, err
	}
	// 注入顺序固定，保证输出键序确定性
	setMappingValue(root, "mixed-port", scalarNode(inj.Settings.MixedPort))
	// allow-lan：模板内静态占位 false，运行时按 settings.allow_lan 注入覆盖（M3-plan §11.2-6）
	setMappingValue(root, "allow-lan", scalarNode(inj.Settings.AllowLan))
	setMappingValue(root, "mode", scalarNode(inj.Settings.Mode))
	setMappingValue(root, "log-level", scalarNode(inj.Settings.LogLevel))
	setMappingValue(root, "secret", scalarNode(inj.Secret))
	// P0 #7：HTTP 控制器无条件保持关闭，控制面仅 unix socket
	setMappingValue(root, "external-controller", scalarNode(""))

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		enc.Close()
		return nil, fmt.Errorf("synth: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("synth: encode close: %w", err)
	}
	return buf.Bytes(), nil
}

// parseMapping 解析 yaml 并返回顶层映射节点。
func parseMapping(data []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	node := &root
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, fmt.Errorf("empty document")
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top level is %v, want mapping", node.Kind)
	}
	return node, nil
}

// mappingValue 在映射节点中按键取值。
func mappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1], true
		}
	}
	return nil, false
}

// setMappingValue 键存在则原地覆盖值（键序不变），不存在则追加到末尾。
func setMappingValue(mapping *yaml.Node, key string, val *yaml.Node) {
	if _, ok := mappingValue(mapping, key); ok {
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				mapping.Content[i+1] = val
				return
			}
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	mapping.Content = append(mapping.Content, keyNode, val)
}

// scalarNode 构造普通样式标量节点。
func scalarNode(v any) *yaml.Node {
	switch t := v.(type) {
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(t)}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(v)}
	}
}
