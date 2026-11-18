package subscription

import (
	"strings"
	"testing"
)

func TestValidateAcceptsMinimalClash(t *testing.T) {
	body := `proxies:
  - name: node-a
    type: ss
    server: 1.2.3.4
    port: 443
rules:
  - MATCH,DIRECT
`
	if err := Validate([]byte(body)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsNotYaml(t *testing.T) {
	// 机场对陌生 UA 常回退 base64 节点列表——必须拒绝而非带病入库
	if err := Validate([]byte("cmF3IGJhc2U2NA==")); err == nil {
		t.Fatal("base64 garbage: want error")
	}
}

func TestValidateRejectsScalarYaml(t *testing.T) {
	if err := Validate([]byte("just a string")); err == nil {
		t.Fatal("scalar yaml: want error")
	}
}

func TestValidateRejectsEmptyProxies(t *testing.T) {
	if err := Validate([]byte("proxies: []\nrules:\n  - MATCH,DIRECT\n")); err == nil {
		t.Fatal("empty proxies: want error")
	}
}

func TestValidateRejectsMissingProxies(t *testing.T) {
	if err := Validate([]byte("port: 7890\nmode: rule\n")); err == nil {
		t.Fatal("missing proxies: want error")
	}
}

func TestValidateRejectsProxyWithoutType(t *testing.T) {
	// 缺 type 的节点会让内核启动期 fatal，必须在导入时拦截
	body := "proxies:\n  - name: broken\n    server: 1.2.3.4\n    port: 1\n"
	err := Validate([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v, want missing type error naming proxy", err)
	}
}
