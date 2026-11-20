// enhance.go 实现增强链合成层（批③ B2）：当前订阅三槽 + 全局 merge.yaml
// 四指令键 + 深覆盖（batch3 §3.1/§3.2）。
//
// 合成链顺序（写死，评审 P3 相对次序钉死）：
//
//	模板 → 订阅三键（白名单） → 当前订阅三槽（rules prepend / proxies append /
//	groups append） → 全局 merge 指令与深覆盖 → settings 注入（仍最高优先；
//	external-controller 空值注入在 merge 之后执行——任何增强层都无法重开 HTTP 控制器）
//
// 全部为纯函数：不读盘、不打印；告警经 Injection.Warnings 槽外送 CLI。
package synth

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Enhance 是增强链各层的原文输入（nil 或 len==0 = 该槽空转；纯注释文档亦空转）。
type Enhance struct {
	GlobalMerge []byte // <数据目录>/enhance/merge.yaml（槽名 global-merge）
	Rules       []byte // <uid>.rules.yaml（prepend）
	Proxies     []byte // <uid>.proxies.yaml（append）
	Groups      []byte // <uid>.groups.yaml（append）
}

// IsEmpty 空转判定（评审 P3）：len==0 或解析后无顶层内容（纯注释/null 文档）→ 空转。
func (e *Enhance) IsEmpty() bool {
	if e == nil {
		return true
	}
	for _, layer := range [][]byte{e.GlobalMerge, e.Rules, e.Proxies, e.Groups} {
		if len(layer) > 0 && hasTopLevelContent(layer) {
			return false
		}
	}
	return true
}

// hasTopLevelContent 判断 yaml 文档是否有顶层内容（纯注释/空文档 → false）。
func hasTopLevelContent(data []byte) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return true // 语法错误交给后续解析报错，不按空转吞掉
	}
	return doc.Kind != 0 && !(doc.Kind == yaml.ScalarNode && doc.Tag == "!!null")
}

// enhanceWarningPrefix 是告警行前缀（CLI 侧输出 "warning: <w>"）。
const enhanceWarningPrefix = "enhance: "

// enhGlobalMerge 取全局 merge 层原文（nil 安全）。
func enhGlobalMerge(e *Enhance) []byte {
	if e == nil {
		return nil
	}
	return e.GlobalMerge
}

// warn 把告警追加到注入的收集槽（nil = 丢弃；纯函数不打印）。
func warn(sink *[]string, format string, args ...any) {
	if sink == nil {
		return
	}
	*sink = append(*sink, enhanceWarningPrefix+fmt.Sprintf(format, args...))
}

// parseEnhanceLayer 宽松解析一层：len==0 或纯注释/null 文档返回 (nil, false) 空转；
// 有内容但非 sequence 返回错误（槽层必须是顶层序列，batch3 §3.2 三槽语义）。
func parseEnhanceLayer(data []byte, label string) (*yaml.Node, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("synth: enhance %s: %w", label, err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, nil
		}
		node = node.Content[0]
	}
	if node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
		return nil, nil // 纯注释/null 文档：空转早退（评审 P3）
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("synth: enhance %s: top level is %v, want sequence", label, node.Kind)
	}
	if len(node.Content) == 0 {
		return nil, nil // 空序列：空转
	}
	return node, nil
}

// parseMergeMapping 宽松解析 merge.yaml：空转返回 (nil, false)；非映射返回错误。
func parseMergeMapping(data []byte) (*yaml.Node, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("synth: enhance merge.yaml: %w", err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil, nil
		}
		node = node.Content[0]
	}
	if node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("synth: enhance merge.yaml: top level is %v, want mapping", node.Kind)
	}
	return node, nil
}

// applyEnhanceSlots 当前订阅三槽（作用于 root）：
// rules=prepend（自定义分流优先于机场，社区惯例）；proxies/groups=append。
func applyEnhanceSlots(root *yaml.Node, enh *Enhance) error {
	if enh == nil {
		return nil
	}
	if rules, err := parseEnhanceLayer(enh.Rules, "rules"); err != nil {
		return err
	} else if rules != nil {
		prependToSequence(root, "rules", rules)
	}
	if proxies, err := parseEnhanceLayer(enh.Proxies, "proxies"); err != nil {
		return err
	} else if proxies != nil {
		appendToSequence(root, "proxies", proxies.Content)
	}
	if groups, err := parseEnhanceLayer(enh.Groups, "proxy-groups"); err != nil {
		return err
	} else if groups != nil {
		appendToSequence(root, "proxy-groups", groups.Content)
	}
	return nil
}

// mergeDirectiveKeys 是 merge.yaml 的最小 DSL 四指令键（batch3 §3.2）：
// 其余任意键 = 对全配置深覆盖。
var mergeDirectiveKeys = map[string]bool{
	"rules-prepend":      true,
	"rules-append":       true,
	"proxies-append":     true,
	"proxy-groups-patch": true,
}

// applyGlobalMerge 全局 merge.yaml（作用于 root；三槽之后调用）：
// 1) 其余键按书写序深覆盖；2) 四指令统一装配——rules-prepend 置于最前
// （全局公共分流优先级最高，高于订阅三槽 prepend，评审 P3）；
// proxy-groups-patch 作用于订阅原始 groups + 三槽 append 之后的合并列表。
func applyGlobalMerge(root *yaml.Node, merge []byte, sink *[]string) error {
	mroot, err := parseMergeMapping(merge)
	if err != nil {
		return err
	}
	if mroot == nil {
		return nil
	}
	directives := map[string]*yaml.Node{}
	for i := 0; i+1 < len(mroot.Content); i += 2 {
		key, val := mroot.Content[i].Value, mroot.Content[i+1]
		if mergeDirectiveKeys[key] {
			directives[key] = val
			continue
		}
		deepMergeKey(root, key, val)
	}
	if node := directives["rules-prepend"]; node != nil {
		if err := checkSequence(node, "rules-prepend"); err != nil {
			return err
		}
		prependToSequence(root, "rules", node)
	}
	if node := directives["rules-append"]; node != nil {
		if err := checkSequence(node, "rules-append"); err != nil {
			return err
		}
		appendToSequence(root, "rules", node.Content)
	}
	if node := directives["proxies-append"]; node != nil {
		if err := checkSequence(node, "proxies-append"); err != nil {
			return err
		}
		appendProxiesDedup(root, node, sink)
	}
	if node := directives["proxy-groups-patch"]; node != nil {
		if err := patchProxyGroups(root, node, sink); err != nil {
			return err
		}
	}
	return nil
}

// checkSequence 校验指令值为顶层序列。
func checkSequence(node *yaml.Node, label string) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("synth: enhance merge.yaml %s: top level is %v, want sequence", label, node.Kind)
	}
	return nil
}

// prependToSequence 把 prepend 序列整体置于 root[key] 现序列之前（键缺失则创建）。
// WHY 取内容快照而非追加 nil：mappingValue 未命中返回 typed-nil，直接混入 Content
// 会让编码器 SIGSEGV。
func prependToSequence(root *yaml.Node, key string, prepend *yaml.Node) {
	var oldContent []*yaml.Node
	if existing, ok := mappingValue(root, key); ok && existing.Kind == yaml.SequenceNode {
		oldContent = existing.Content
	}
	combined := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	combined.Content = append(append([]*yaml.Node{}, prepend.Content...), oldContent...)
	setMappingValue(root, key, combined)
}

// appendToSequence 把 items 追加到 root[key] 现序列之后（键缺失则创建）。
func appendToSequence(root *yaml.Node, key string, items []*yaml.Node) {
	if len(items) == 0 {
		return
	}
	var existing *yaml.Node
	if node, ok := mappingValue(root, key); ok && node.Kind == yaml.SequenceNode {
		existing = node
	}
	if existing == nil {
		combined := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		combined.Content = append(combined.Content, items...)
		setMappingValue(root, key, combined)
		return
	}
	existing.Content = append(existing.Content, items...)
}

// appendProxiesDedup proxies-append 防重名：与既有 proxies name 撞名 → 跳过该条
// （不静默，告警外送）。
func appendProxiesDedup(root, list *yaml.Node, sink *[]string) {
	existing, ok := mappingValue(root, "proxies")
	names := map[string]bool{}
	if ok && existing.Kind == yaml.SequenceNode {
		for _, item := range existing.Content {
			if n, found := nodeStringField(item, "name"); found {
				names[n] = true
			}
		}
	}
	for _, item := range list.Content {
		name, found := nodeStringField(item, "name")
		if found && names[name] {
			warn(sink, "proxies-append 撞名 %q，已跳过", name)
			continue
		}
		if found {
			names[name] = true
		}
		appendToSequence(root, "proxies", []*yaml.Node{item})
	}
}

// patchProxyGroups proxy-groups-patch 按 name 项级合并：数组每项 {name, ...字段}，
// 对合并后 proxy-groups 同名组做字段覆盖/新增；未命中组跳过 + 告警
// （Auto 组 interval 修改的解锁通道：[{name: 自动选择, interval: 60}]）。
func patchProxyGroups(root, patch *yaml.Node, sink *[]string) error {
	if patch.Kind != yaml.SequenceNode {
		return fmt.Errorf("synth: enhance merge.yaml proxy-groups-patch: top level is %v, want sequence", patch.Kind)
	}
	groups, ok := mappingValue(root, "proxy-groups")
	for _, item := range patch.Content {
		if item.Kind != yaml.MappingNode {
			return fmt.Errorf("synth: enhance merge.yaml proxy-groups-patch: item is %v, want mapping", item.Kind)
		}
		name, found := nodeStringField(item, "name")
		if !found {
			return fmt.Errorf("synth: enhance merge.yaml proxy-groups-patch: item missing name")
		}
		if !ok || groups.Kind != yaml.SequenceNode {
			warn(sink, "proxy-groups-patch 未命中组 %q（配置无 proxy-groups），已跳过", name)
			continue
		}
		target := findGroupByName(groups, name)
		if target == nil {
			warn(sink, "proxy-groups-patch 未命中组 %q，已跳过", name)
			continue
		}
		for i := 0; i+1 < len(item.Content); i += 2 {
			key := item.Content[i].Value
			if key == "name" {
				continue
			}
			setMappingValue(target, key, item.Content[i+1])
		}
	}
	return nil
}

// findGroupByName 在 proxy-groups 序列中按 name 定位组（未命中 nil）。
func findGroupByName(groups *yaml.Node, name string) *yaml.Node {
	for _, item := range groups.Content {
		if n, found := nodeStringField(item, "name"); found && n == name {
			return item
		}
	}
	return nil
}

// nodeStringField 读取映射节点的字符串字段（缺失/非映射返回 false）。
func nodeStringField(node *yaml.Node, field string) (string, bool) {
	if node.Kind != yaml.MappingNode {
		return "", false
	}
	if val, ok := mappingValue(node, field); ok && val.Kind == yaml.ScalarNode {
		return val.Value, true
	}
	return "", false
}

// deepMergeKey 其余任意键深覆盖：dst 有同名键且双方均为映射 → 递归合并；
// 否则标量/序列整体覆盖或新增（batch3 §3.2：map+map 递归、标量覆盖、list 整体替换）。
func deepMergeKey(root *yaml.Node, key string, src *yaml.Node) {
	if existing, ok := mappingValue(root, key); ok &&
		existing.Kind == yaml.MappingNode && src.Kind == yaml.MappingNode {
		mergeMapping(existing, src)
		return
	}
	setMappingValue(root, key, src)
}

// mergeMapping 递归深合并（src 键序稳定输出）。
func mergeMapping(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, val := src.Content[i].Value, src.Content[i+1]
		if existing, ok := mappingValue(dst, key); ok &&
			existing.Kind == yaml.MappingNode && val.Kind == yaml.MappingNode {
			mergeMapping(existing, val)
			continue
		}
		setMappingValue(dst, key, val)
	}
}
