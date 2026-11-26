// normalize.go：配置白名单归一化器（批⑥b §4.2，I3——内核只加载服务归一化过的配置）。
//
// 纯函数：输入用户 runtime.yaml 字节，输出"合成器同款编码"的产物或首个拒绝原因。
// 策略表在 normalize_policy.go，结构校验器在 normalize_struct.go，键名回显的净化
// （sanitizeName 等）在 normalize_echo.go。两条硬红线：
//   - 错误信息只含键名/下标/枚举集合/上限数字，绝不含配置值（订阅节点的密码、uuid
//     就在这份文件里）；键名回显前截断 64 字节并替换掉非可打印字符，整条键路径
//     封顶 256 字节（错误文本会进 root 属主的 service.log，批⑥b I6）；
//   - 幂等：对自身产物再归一化必须逐字节相同且 Ignored 为空——服务启动时"用当前
//     归一化器复验磁盘产物"（§4.4 L1）靠它是固定点。
package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/chaojimaimi/tradewind/internal/synth"
	"gopkg.in/yaml.v3"
)

// Result 是归一化产物。
type Result struct {
	Bytes   []byte   // 归一化后的 runtime.yaml
	Ignored []string // 被剥离的用户键（点路径，如 "proxy-providers.my-provider.path"），按出现顺序
}

// Rejection 是拒绝原因。Key 是键路径（如 "dns.listen"、"proxies[3].state-dir"；
// 文档级问题用 "document"/"root"），Reason 不含任何配置值。
type Rejection struct {
	Key    string
	Reason string
}

func (r *Rejection) Error() string {
	return "service: config rejected at " + r.Key + ": " + r.Reason
}

// 上限（批⑥b §4.2）。字节粗闸 runtimeMaxBytes 在 safeopen.go，与本节条目上限互不对齐——
// 谁先到谁生效。
const (
	maxDepth             = 128 // 顶层映射记为第 1 层；任一节点超过即拒
	maxNodeCount         = 1_000_000
	maxSeqItems          = 64  // 直通·序列的项数
	maxSeqItemBytes      = 256 // 直通·序列的单项字节
	maxProxies           = 4096
	maxProxyNameBytes    = 256
	maxGroups            = 1024
	maxRules             = 65536 // rules 与 sub-rules 总条数共用
	maxRuleBytes         = 4096
	maxProviders         = 256
	maxProviderNameBytes = 128
	keyPathMaxBytes      = 256 // Rejection.Key 整条路径的上限（每段 ≤64 字节，但层数会累加）
)

// scalarTagsAllowed：核心 schema 标量 tag；!!binary 与自定义 tag 一律拒
// （批⑥b §4.2 解析纪律：二进制与自定义 tag 的解码结果不可预期）。
var scalarTagsAllowed = map[string]bool{
	"!!str": true, "!!int": true, "!!float": true, "!!bool": true, "!!null": true, "!!timestamp": true,
}

// yamlLineRe 从 yaml.v3 错误文本里提取行号（原文可能带出 token 内容，绝不透传）。
var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// NormalizeRuntime 是纯函数：输入用户 runtime.yaml 字节，输出归一化产物或首个拒绝原因。
// 处理序：字节粗闸 → 解析纪律（单文档/根映射）→ 全树结构纪律（别名/锚点/键类型/
// 重复键/深度/节点数/标量 tag）→ 顶层键按策略表分发（按文档顺序，首个拒绝即返回）→
// 钉死键替换/追加 → 合成器同款编码。
func NormalizeRuntime(in []byte) (Result, *Rejection) {
	res, rej := normalizeRuntime(in)
	return res, rej.bounded()
}

// bounded 给拒绝的键路径封顶。每段键名至多回显 64 字节，但路径随嵌套加深而变长
// （128 层 × 65 字节 ≈ 8KB）——调用方会把拒绝文本写进日志与错误响应，整条路径
// 也得有界。超长时保留最外层前缀并以 "…" 收尾（最外层的键最能说明"哪一块出问题"）。
func (r *Rejection) bounded() *Rejection {
	if r == nil || len(r.Key) <= keyPathMaxBytes {
		return r
	}
	cut := keyPathMaxBytes - len("…")
	for cut > 0 && !utf8.RuneStart(r.Key[cut]) {
		cut--
	}
	return &Rejection{Key: r.Key[:cut] + "…", Reason: r.Reason}
}

func normalizeRuntime(in []byte) (Result, *Rejection) {
	if len(in) > runtimeMaxBytes {
		return Result{}, &Rejection{Key: "document", Reason: fmt.Sprintf("input exceeds the %d byte limit", runtimeMaxBytes)}
	}
	root, rej := parseDocument(in)
	if rej != nil {
		return Result{}, rej
	}
	var walker treeWalker
	if rej := walker.visit(root, "", 1); rej != nil {
		return Result{}, rej
	}
	var ignored []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if pinnedKeys[key.Value] {
			// 钉死键的值不参与策略校验，pinKeys 统一替换/追加。全树结构纪律
			// （别名/锚点/重复键/tag）在上面已经对它执行过——含别名的 tun 块是
			// 拒绝而不是替换：合成器产物不含这些，fail-closed。
			continue
		}
		if rej := applyPolicy(key.Value, val, &ignored); rej != nil {
			return Result{}, rej
		}
	}
	pinKeys(root)
	out, rej := encodeRoot(root)
	if rej != nil {
		return Result{}, rej
	}
	return Result{Bytes: out, Ignored: ignored}, nil
}

// parseDocument 解析纪律 1/2：单文档（第二次 Decode 必须 io.EOF）、根为映射；
// 空输入拒绝。语法错误只提取行号——yaml.v3 的错误文本可能带出 token 内容。
func parseDocument(in []byte) (*yaml.Node, *Rejection) {
	dec := yaml.NewDecoder(bytes.NewReader(in))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Rejection{Key: "document", Reason: "empty document"}
		}
		return nil, &Rejection{Key: "document", Reason: "invalid YAML" + lineNote(err)}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, &Rejection{Key: "document", Reason: "expected exactly one document"}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, &Rejection{Key: "root", Reason: "top level is not a mapping"}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &Rejection{Key: "document", Reason: "multiple YAML documents are not allowed"}
	}
	return doc.Content[0], nil
}

// lineNote 只保留 yaml.v3 错误里的行号。
func lineNote(err error) string {
	if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
		return fmt.Sprintf(" (line %s)", m[1])
	}
	return ""
}

// treeWalker 遍历全树执行解析纪律 3。WHY 全树先于策略分发：别名、锚点、合并键、
// 自定义 tag 会让"我们校验的结构"和内核解码出的结构不一致——这是白名单可信的
// 前提，与具体键无关（用户的真实配置不含这些，P3 实证）。
type treeWalker struct{ count int }

func (w *treeWalker) bump() *Rejection {
	w.count++
	if w.count > maxNodeCount {
		return &Rejection{Key: "document", Reason: fmt.Sprintf("more than %d nodes", maxNodeCount)}
	}
	return nil
}

func (w *treeWalker) visit(n *yaml.Node, path string, depth int) *Rejection {
	if rej := w.bump(); rej != nil {
		return rej
	}
	if depth > maxDepth {
		return &Rejection{Key: keyOrRoot(path), Reason: fmt.Sprintf("nesting deeper than %d levels", maxDepth)}
	}
	if n.Kind == yaml.AliasNode {
		return &Rejection{Key: keyOrRoot(path), Reason: "aliases are not allowed"}
	}
	if n.Anchor != "" {
		return &Rejection{Key: keyOrRoot(path), Reason: "anchors are not allowed"}
	}
	switch n.Kind {
	case yaml.MappingNode:
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if rej := w.bump(); rej != nil { // 键节点计入节点总数
				return rej
			}
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
				// !!merge（<<）键与非标量键由此一并拦下
				return &Rejection{Key: keyOrRoot(path), Reason: "mapping keys must be plain strings"}
			}
			if seen[k.Value] {
				return &Rejection{Key: childPath(path, k.Value), Reason: "duplicate key"}
			}
			seen[k.Value] = true
			if rej := w.visit(v, childPath(path, k.Value), depth+1); rej != nil {
				return rej
			}
		}
	case yaml.SequenceNode:
		for i, item := range n.Content {
			if rej := w.visit(item, fmt.Sprintf("%s[%d]", path, i), depth+1); rej != nil {
				return rej
			}
		}
	case yaml.ScalarNode:
		if !scalarTagsAllowed[n.Tag] {
			return &Rejection{Key: keyOrRoot(path),
				Reason: "scalar type is not allowed (allowed: str/int/float/bool/null/timestamp)"}
		}
	}
	return nil
}

// childPath 拼接键路径；键名先过 sanitizeName（键名可回显，但要截断、去控制字符）。
func childPath(parent, key string) string {
	key = sanitizeName(key)
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// keyOrRoot 文档根（path 为空）的键路径固定为 "root"。
func keyOrRoot(path string) string {
	if path == "" {
		return "root"
	}
	return path
}

// applyPolicy 顶层键按策略表分发（批⑥b §4.2 策略表；"直通"表示键被允许，值仍按表校验）。
func applyPolicy(key string, val *yaml.Node, ignored *[]string) *Rejection {
	if rejectKeys[key] {
		return &Rejection{Key: key, Reason: key + " is not allowed in TUN mode"}
	}
	if max, ok := strMaxKeys[key]; ok {
		return checkStr(key, val, max)
	}
	if choices, ok := enumKeys[key]; ok {
		return checkEnum(key, val, choices)
	}
	if portKeys[key] {
		return checkInt(key, val, "must be an integer in [0, 65535]",
			func(v int64) bool { return v >= 0 && v <= 65535 })
	}
	if nonNegIntKeys[key] {
		return checkInt(key, val, "must be a non-negative integer",
			func(v int64) bool { return v >= 0 })
	}
	if boolKeys[key] {
		return checkBool(key, val)
	}
	if seqStrKeys[key] {
		return checkStrSeq(key, val)
	}
	if structKeys[key] {
		return checkStruct(key, val, ignored)
	}
	return &Rejection{Key: sanitizeName(key), Reason: "unknown key"}
}

// checkInt 校验 !!int 标量并按谓词放行（端口与 ≥0 整数共用；base 0 兼容 yaml.v3
// 的 0x/0o/下划线整数字面量）。tag 不对与值越界共用一条 Reason——都不含值。
func checkInt(key string, val *yaml.Node, reason string, ok func(int64) bool) *Rejection {
	if val.Kind != yaml.ScalarNode || val.Tag != "!!int" {
		return &Rejection{Key: key, Reason: reason}
	}
	v, err := strconv.ParseInt(val.Value, 0, 64)
	if err != nil || !ok(v) {
		return &Rejection{Key: key, Reason: reason}
	}
	return nil
}

// checkStr 校验 !!str 标量长度（len(Value) 按字节）。
func checkStr(key string, val *yaml.Node, maxBytes int) *Rejection {
	if val.Kind != yaml.ScalarNode || val.Tag != "!!str" {
		return &Rejection{Key: key, Reason: "must be a string"}
	}
	if len(val.Value) > maxBytes {
		return &Rejection{Key: key, Reason: fmt.Sprintf("string longer than %d bytes", maxBytes)}
	}
	return nil
}

func checkEnum(key string, val *yaml.Node, choices map[string]bool) *Rejection {
	if val.Kind != yaml.ScalarNode || val.Tag != "!!str" || !choices[val.Value] {
		return &Rejection{Key: key, Reason: "must be one of: " + enumList(choices)}
	}
	return nil
}

// enumList 输出排序后的枚举集合（错误信息允许枚举集合；排序保证文案确定）。
func enumList(choices map[string]bool) string {
	names := make([]string, 0, len(choices))
	for name := range choices {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func checkBool(key string, val *yaml.Node) *Rejection {
	if val.Kind != yaml.ScalarNode || val.Tag != "!!bool" {
		return &Rejection{Key: key, Reason: "must be a boolean"}
	}
	return nil
}

// checkStrSeq 直通·序列：≤64 项，每项 !!str 且 ≤256 字节。
func checkStrSeq(key string, val *yaml.Node) *Rejection {
	if val.Kind != yaml.SequenceNode {
		return &Rejection{Key: key, Reason: "must be a sequence of strings"}
	}
	if len(val.Content) > maxSeqItems {
		return &Rejection{Key: key, Reason: fmt.Sprintf("sequence longer than %d items", maxSeqItems)}
	}
	for i, item := range val.Content {
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || len(item.Value) > maxSeqItemBytes {
			return &Rejection{Key: fmt.Sprintf("%s[%d]", key, i),
				Reason: fmt.Sprintf("must be a string of at most %d bytes", maxSeqItemBytes)}
		}
	}
	return nil
}

// pinKeys 写入两个钉死键：已存在则原位替换值节点（无论用户写了什么形状）；缺失则
// 按 pinnedAppendOrder 追加到顶层映射末尾——追加位置固定才有幂等性。tun 块与合成
// 器同源（synth.TunBlockNode）；external-controller 编码成 ""（与合成器产物同形）。
func pinKeys(root *yaml.Node) {
	for _, key := range pinnedAppendOrder {
		var val *yaml.Node
		if key == "tun" {
			val = synth.TunBlockNode()
		} else {
			val = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"} // 空值编码为 ""
		}
		replaced := false
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				root.Content[i+1] = val
				replaced = true
				break
			}
		}
		if !replaced {
			root.Content = append(root.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
		}
	}
}

// encodeRoot 与合成器同一种编码：yaml.NewEncoder + SetIndent(2) + Encode(顶层映射) + Close。
func encodeRoot(root *yaml.Node) ([]byte, *Rejection) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		enc.Close()
		return nil, &Rejection{Key: "document", Reason: "failed to encode normalized config"}
	}
	if err := enc.Close(); err != nil {
		return nil, &Rejection{Key: "document", Reason: "failed to encode normalized config"}
	}
	return buf.Bytes(), nil
}
