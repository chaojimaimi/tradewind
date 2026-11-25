// normalize_struct.go：归一化器的结构校验器（批⑥b §4.2 结构校验细则）。
//
// 每个结构键的值逐键判定：缺键、形状不对都拒（fail-closed，内核继续跑上一份有效
// 配置）。错误信息只含键名/下标/枚举集合/上限数字，绝不含配置值（值红线见 normalize.go）。
package service

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// checkStruct 结构校验键分发。
func checkStruct(key string, val *yaml.Node, ignored *[]string) *Rejection {
	switch key {
	case "proxies":
		return checkProxies(val)
	case "proxy-groups":
		return checkGroups(val)
	case "rules":
		return checkRules("rules", val, maxRules)
	case "sub-rules":
		return checkSubRules(val)
	case "proxy-providers":
		return checkProviders("proxy-providers", val, ignored)
	case "rule-providers":
		return checkProviders("rule-providers", val, ignored)
	case "dns":
		return checkDNS(val)
	case "sniffer":
		return checkNestedKeys("sniffer", val, snifferKeys)
	case "profile":
		return checkProfile(val)
	case "experimental":
		return checkNestedKeys("experimental", val, experimentalKeys)
	case "hosts":
		return checkHosts(val)
	}
	return nil // structKeys 之外的键不会进来
}

// checkProxies 序列 ≤4096 项；每项映射；name ∈ [1,256] 字节；type 在出站白名单内；
// 任何类型下出现 state-dir 键即拒（M11：全部出站里只有三个类型会落盘，白名单已
// 排除，state-dir 是纵深防御——防止内核后续给其它类型加同名字段时漏拦）。
func checkProxies(val *yaml.Node) *Rejection {
	if val.Kind != yaml.SequenceNode {
		return &Rejection{Key: "proxies", Reason: "must be a sequence"}
	}
	if len(val.Content) > maxProxies {
		return &Rejection{Key: "proxies", Reason: fmt.Sprintf("sequence longer than %d items", maxProxies)}
	}
	for i, item := range val.Content {
		path := fmt.Sprintf("proxies[%d]", i)
		if item.Kind != yaml.MappingNode {
			return &Rejection{Key: path, Reason: "must be a mapping"}
		}
		if _, ok := mappingGet(item, "state-dir"); ok {
			return &Rejection{Key: path + ".state-dir", Reason: "state-dir is not allowed in TUN mode"}
		}
		name, ok := mappingGet(item, "name")
		if !ok {
			return &Rejection{Key: path + ".name", Reason: "name is required"}
		}
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" ||
			len(name.Value) < 1 || len(name.Value) > maxProxyNameBytes {
			return &Rejection{Key: path + ".name",
				Reason: fmt.Sprintf("must be a string of 1 to %d bytes", maxProxyNameBytes)}
		}
		typ, ok := mappingGet(item, "type")
		if !ok {
			return &Rejection{Key: path + ".type", Reason: "type is required"}
		}
		if typ.Kind != yaml.ScalarNode || typ.Tag != "!!str" || !outboundTypeAllowed[typ.Value] {
			return &Rejection{Key: path + ".type", Reason: "outbound type is not in the TUN mode whitelist"}
		}
	}
	return nil
}

// checkGroups 序列 ≤1024 项；每项映射；name ∈ [1,256] 字节（与 proxies 的 name 同限——
// 组名会进内核的 selector 状态与缓存，没有理由比节点名宽松）；type 在策略组白名单内。
func checkGroups(val *yaml.Node) *Rejection {
	if val.Kind != yaml.SequenceNode {
		return &Rejection{Key: "proxy-groups", Reason: "must be a sequence"}
	}
	if len(val.Content) > maxGroups {
		return &Rejection{Key: "proxy-groups", Reason: fmt.Sprintf("sequence longer than %d items", maxGroups)}
	}
	for i, item := range val.Content {
		path := fmt.Sprintf("proxy-groups[%d]", i)
		if item.Kind != yaml.MappingNode {
			return &Rejection{Key: path, Reason: "must be a mapping"}
		}
		name, ok := mappingGet(item, "name")
		if !ok {
			return &Rejection{Key: path + ".name", Reason: "name is required"}
		}
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" ||
			len(name.Value) < 1 || len(name.Value) > maxProxyNameBytes {
			return &Rejection{Key: path + ".name",
				Reason: fmt.Sprintf("must be a string of 1 to %d bytes", maxProxyNameBytes)}
		}
		typ, ok := mappingGet(item, "type")
		if !ok {
			return &Rejection{Key: path + ".type", Reason: "type is required"}
		}
		if typ.Kind != yaml.ScalarNode || typ.Tag != "!!str" || !proxyGroupTypes[typ.Value] {
			return &Rejection{Key: path + ".type", Reason: "must be one of: " + enumList(proxyGroupTypes)}
		}
	}
	return nil
}

// checkRules 全部条目为 !!str 且 ≤4096 字节，总数 ≤ limit。
func checkRules(key string, val *yaml.Node, limit int) *Rejection {
	if val.Kind != yaml.SequenceNode {
		return &Rejection{Key: key, Reason: "must be a sequence of strings"}
	}
	if len(val.Content) > limit {
		return &Rejection{Key: key, Reason: fmt.Sprintf("sequence longer than %d items", limit)}
	}
	for i, item := range val.Content {
		if rej := checkRuleItem(fmt.Sprintf("%s[%d]", key, i), item); rej != nil {
			return rej
		}
	}
	return nil
}

func checkRuleItem(path string, item *yaml.Node) *Rejection {
	if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || len(item.Value) > maxRuleBytes {
		return &Rejection{Key: path, Reason: fmt.Sprintf("must be a string of at most %d bytes", maxRuleBytes)}
	}
	return nil
}

// checkSubRules 名称→规则序列的映射；全部值的总条数 ≤65536。
func checkSubRules(val *yaml.Node) *Rejection {
	if val.Kind != yaml.MappingNode {
		return &Rejection{Key: "sub-rules", Reason: "must be a mapping"}
	}
	total := 0
	for i := 0; i+1 < len(val.Content); i += 2 {
		name, seq := val.Content[i], val.Content[i+1]
		path := "sub-rules." + sanitizeName(name.Value)
		if seq.Kind != yaml.SequenceNode {
			return &Rejection{Key: path, Reason: "must be a sequence of strings"}
		}
		for j, item := range seq.Content {
			total++
			if total > maxRules {
				return &Rejection{Key: "sub-rules", Reason: fmt.Sprintf("more than %d rules in total", maxRules)}
			}
			if rej := checkRuleItem(fmt.Sprintf("%s[%d]", path, j), item); rej != nil {
				return rej
			}
		}
	}
	return nil
}

// checkProviders 映射 ≤256 项；名称 ∈ [1,128] 字节；type ∈ {http, inline}。
// type: file 拒绝（TUN 模式没有用户文件可读，改用 enhance 槽）；type: http 时剥离
// 用户 path 键并记入 Ignored——内核回落到自己的默认哈希路径（M9），provider 下载
// 永远不可能覆盖 runtime.yaml / geo / cache.db（R16 的核心闸之一）。
func checkProviders(kind string, val *yaml.Node, ignored *[]string) *Rejection {
	if val.Kind != yaml.MappingNode {
		return &Rejection{Key: kind, Reason: "must be a mapping"}
	}
	if len(val.Content)/2 > maxProviders {
		return &Rejection{Key: kind, Reason: fmt.Sprintf("more than %d entries", maxProviders)}
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		name, body := val.Content[i], val.Content[i+1]
		path := kind + "." + sanitizeName(name.Value)
		if len(name.Value) < 1 || len(name.Value) > maxProviderNameBytes {
			return &Rejection{Key: path, Reason: fmt.Sprintf("name must be 1 to %d bytes", maxProviderNameBytes)}
		}
		if body.Kind != yaml.MappingNode {
			return &Rejection{Key: path, Reason: "must be a mapping"}
		}
		typ, ok := mappingGet(body, "type")
		if !ok {
			return &Rejection{Key: path + ".type", Reason: "type is required"}
		}
		if typ.Kind != yaml.ScalarNode || typ.Tag != "!!str" {
			return &Rejection{Key: path + ".type", Reason: "type must be http or inline"}
		}
		switch typ.Value {
		case "http":
			if _, ok := mappingGet(body, "path"); ok {
				removePair(body, "path")
				*ignored = append(*ignored, path+".path")
			}
		case "inline":
			// 内容随配置内联，无文件面
		case "file":
			return &Rejection{Key: path + ".type",
				Reason: "file providers are not allowed in TUN mode (no user files readable); use the enhance slot instead"}
		default:
			return &Rejection{Key: path + ".type", Reason: "type must be http or inline"}
		}
	}
	return nil
}

// checkDNS 键 ⊂ RawDNS 27 键；listen 必须缺省、!!null 或空 !!str（DNS 监听面收紧，
// 批⑥b §4.2）。
func checkDNS(val *yaml.Node) *Rejection {
	if val.Kind != yaml.MappingNode {
		return &Rejection{Key: "dns", Reason: "must be a mapping"}
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		k, v := val.Content[i], val.Content[i+1]
		if !dnsKeys[k.Value] {
			return &Rejection{Key: "dns." + sanitizeName(k.Value), Reason: "unknown key"}
		}
		if k.Value == "listen" {
			if v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || (v.Tag == "!!str" && v.Value == "")) {
				continue
			}
			return &Rejection{Key: "dns.listen", Reason: "listen must be absent, null or empty in TUN mode"}
		}
	}
	return nil
}

// checkNestedKeys 键集合封闭的嵌套映射（sniffer / experimental）。
func checkNestedKeys(key string, val *yaml.Node, allowed map[string]bool) *Rejection {
	if val.Kind != yaml.MappingNode {
		return &Rejection{Key: key, Reason: "must be a mapping"}
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		if !allowed[val.Content[i].Value] {
			return &Rejection{Key: key + "." + sanitizeName(val.Content[i].Value), Reason: "unknown key"}
		}
	}
	return nil
}

// checkProfile 键 ⊂ 2 键且值为 !!bool。
func checkProfile(val *yaml.Node) *Rejection {
	if val.Kind != yaml.MappingNode {
		return &Rejection{Key: "profile", Reason: "must be a mapping"}
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		k, v := val.Content[i], val.Content[i+1]
		if !profileKeys[k.Value] {
			return &Rejection{Key: "profile." + sanitizeName(k.Value), Reason: "unknown key"}
		}
		if v.Kind != yaml.ScalarNode || v.Tag != "!!bool" {
			return &Rejection{Key: "profile." + sanitizeName(k.Value), Reason: "must be a boolean"}
		}
	}
	return nil
}

// checkHosts 值为标量或标量序列（键类型已由全树纪律保证为 !!str）。
func checkHosts(val *yaml.Node) *Rejection {
	if val.Kind != yaml.MappingNode {
		return &Rejection{Key: "hosts", Reason: "must be a mapping"}
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		v := val.Content[i+1]
		switch v.Kind {
		case yaml.ScalarNode:
			// 标量放行
		case yaml.SequenceNode:
			for _, item := range v.Content {
				if item.Kind != yaml.ScalarNode {
					return &Rejection{Key: "hosts." + sanitizeName(val.Content[i].Value),
						Reason: "must be a scalar or a sequence of scalars"}
				}
			}
		default:
			return &Rejection{Key: "hosts." + sanitizeName(val.Content[i].Value),
				Reason: "must be a scalar or a sequence of scalars"}
		}
	}
	return nil
}

// mappingGet 在映射节点中按键取值（键类型/重复性已由全树纪律保证）。
func mappingGet(m *yaml.Node, key string) (*yaml.Node, bool) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], true
		}
	}
	return nil, false
}

// removePair 从映射节点中原位删除一对键值（provider path 剥离用）。
func removePair(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
