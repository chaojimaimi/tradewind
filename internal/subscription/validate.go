package subscription

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ProxyStub 只提取校验必需的字段，其余字段（加密方式/密码等）交给内核在启动时校验。
type ProxyStub struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// Validate 校验订阅体是合法的 clash yaml：可解析、proxies 存在且非空、
// 每个节点有非空 name/type（缺 type 的节点会让内核启动期 fatal）。
func Validate(body []byte) error {
	var doc struct {
		Proxies []ProxyStub `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("subscription: invalid yaml: %w", err)
	}
	if len(doc.Proxies) == 0 {
		return fmt.Errorf("subscription: no proxies found (not a clash subscription?)")
	}
	for i, p := range doc.Proxies {
		if p.Name == "" {
			return fmt.Errorf("subscription: proxies[%d] missing name", i)
		}
		if p.Type == "" {
			return fmt.Errorf("subscription: proxies[%d] (%s) missing type", i, p.Name)
		}
	}
	return nil
}
