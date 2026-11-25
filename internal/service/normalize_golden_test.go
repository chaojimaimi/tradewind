// normalize_golden_test.go：归一化器与合成器两份 golden 的对合 + 幂等断言
// （批⑥b §6-3：TUN 开产物逐字节不变；TUN 关产物 == 输入 + 末尾追加 tun 块；
// 对所有接受用例的产物再归一化必须逐字节相同且 Ignored 为空——§4.4 L1 的固定点）。
package service

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func readSynthGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "synth", "testdata", "golden", name))
	if err != nil {
		t.Fatalf("read synth golden %s: %v", name, err)
	}
	return b
}

// TestNormalizeGoldenTunOnByteIdentical：TUN-on golden 已含 tun 块与 external-controller，
// 归一化只做原位替换，产物逐字节等于输入（I3 的零漂移锚）。
func TestNormalizeGoldenTunOnByteIdentical(t *testing.T) {
	in := readSynthGolden(t, "runtime-tun.yaml")
	res, rej := NormalizeRuntime(in)
	if rej != nil {
		t.Fatalf("want accept, got rejection at %s: %s", rej.Key, rej.Reason)
	}
	if !bytes.Equal(res.Bytes, in) {
		t.Fatalf("tun-on golden must round-trip byte-identical:\n--- got ---\n%s\n--- want ---\n%s", res.Bytes, in)
	}
	if len(res.Ignored) != 0 {
		t.Fatalf("Ignored = %v, want empty", res.Ignored)
	}
}

// TestNormalizeGoldenTunOffAppendsTunBlock：TUN-off golden 没有 tun 键，归一化把
// tun 块追加到末尾。断言从两份 golden 的结构出发：产物 == TUN-off golden 原样 +
// TUN-on golden 里 "tun:" 起的尾段（追加段与合成器注入块逐字节一致）。
func TestNormalizeGoldenTunOffAppendsTunBlock(t *testing.T) {
	off := readSynthGolden(t, "runtime.yaml")
	on := readSynthGolden(t, "runtime-tun.yaml")
	idx := bytes.Index(on, []byte("\ntun:\n"))
	if idx < 0 {
		t.Fatal("tun block not found in tun-on golden")
	}
	tunTail := on[idx+1:]
	res, rej := NormalizeRuntime(off)
	if rej != nil {
		t.Fatalf("want accept, got rejection at %s: %s", rej.Key, rej.Reason)
	}
	want := append(append([]byte{}, off...), tunTail...)
	if !bytes.Equal(res.Bytes, want) {
		t.Fatalf("tun-off normalize must equal input + synth tun tail:\n--- got ---\n%s\n--- want ---\n%s", res.Bytes, want)
	}
	if len(res.Ignored) != 0 {
		t.Fatalf("Ignored = %v, want empty", res.Ignored)
	}
}

// fullHouseFixture 覆盖每类直通键、每个结构键与 provider path 剥离（域名一律
// .invalid，凭据为测试假值）。
func fullHouseFixture() []byte {
	return []byte(`mixed-port: 17890
port: 7890
socks-port: 7891
redir-port: 7892
tproxy-port: 7893
allow-lan: true
unified-delay: true
mode: rule
log-level: info
ipv6: false
tcp-concurrent: true
inbound-tfo: true
inbound-mptcp: false
etag-support: true
disable-keep-alive: false
geodata-mode: true
geodata-loader: standard
find-process-mode: strict
geosite-matcher: mrs
bind-address: 127.0.0.1
interface-name: en0
secret: test-secret
global-client-fingerprint: chrome
global-ua: clash.meta
keep-alive-idle: 15
keep-alive-interval: 30
routing-mark: 99
authentication:
  - user:pass
skip-auth-prefixes:
  - 127.0.0.1/32
lan-allowed-ips:
  - 0.0.0.0/0
lan-disallowed-ips:
  - 192.168.1.1/32
external-controller: "stale-value"
proxies:
  - name: n1
    type: ss
    server: 1.2.3.4
    port: 443
    password: test-password
proxy-groups:
  - name: g1
    type: select
    proxies:
      - n1
rules:
  - MATCH,DIRECT
sub-rules:
  r1:
    - MATCH,DIRECT
proxy-providers:
  sub1:
    type: http
    url: https://provider.example.invalid/subscription
    path: ./sub1.yaml
rule-providers:
  rp1:
    type: inline
    payload: []
hosts:
  a.example.invalid: 1.2.3.4
  b.example.invalid:
    - 1.2.3.4
    - 1.2.3.5
dns:
  enable: true
  listen: ""
  nameserver:
    - 223.5.5.5
sniffer:
  enable: true
profile:
  store-selected: true
  store-fake-ip: false
experimental:
  fingerprints:
    - test-fingerprint
`)
}

// acceptedFixtures 全部"应接受"的输入样例（幂等断言共用）。
func acceptedFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	fixtures := map[string][]byte{
		"fullHouse":    fullHouseFixture(),
		"emptyMapping": []byte("{}\n"),
		"minimal":      []byte("mode: rule\n"),
		"goldenTunOn":  readSynthGolden(t, "runtime-tun.yaml"),
		"goldenTunOff": readSynthGolden(t, "runtime.yaml"),
	}
	return fixtures
}

// TestNormalizeIdempotent：对每个接受用例的产物再归一化，逐字节相同且 Ignored 为空。
func TestNormalizeIdempotent(t *testing.T) {
	for name, in := range acceptedFixtures(t) {
		first, rej := NormalizeRuntime(in)
		if rej != nil {
			t.Fatalf("%s: want accept, got rejection at %s: %s", name, rej.Key, rej.Reason)
		}
		second, rej := NormalizeRuntime(first.Bytes)
		if rej != nil {
			t.Fatalf("%s: second pass rejected at %s: %s", name, rej.Key, rej.Reason)
		}
		if !bytes.Equal(first.Bytes, second.Bytes) {
			t.Fatalf("%s: not idempotent:\n--- first ---\n%s\n--- second ---\n%s", name, first.Bytes, second.Bytes)
		}
		if len(second.Ignored) != 0 {
			t.Fatalf("%s: second pass Ignored = %v, want empty", name, second.Ignored)
		}
	}
}

// TestNormalizeFullHouseProviderPathStripped：fullHouse 里 proxy-provider 的 path 被
// 剥离，Ignored 恰为一条；输出里不再有 path 键。
func TestNormalizeFullHouseProviderPathStripped(t *testing.T) {
	res, rej := NormalizeRuntime(acceptedFixtures(t)["fullHouse"])
	if rej != nil {
		t.Fatalf("want accept, got rejection at %s: %s", rej.Key, rej.Reason)
	}
	if got, want := res.Ignored, []string{"proxy-providers.sub1.path"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Ignored = %v, want %v", got, want)
	}
	var out yaml.Node
	if err := yaml.Unmarshal(res.Bytes, &out); err != nil {
		t.Fatalf("output not valid yaml: %v", err)
	}
	providers := out.Content[0]
	pp, ok := mappingGet(providers, "proxy-providers")
	if !ok {
		t.Fatal("proxy-providers missing from output")
	}
	sub1, ok := mappingGet(pp, "sub1")
	if !ok {
		t.Fatal("proxy-providers.sub1 missing from output")
	}
	if _, present := mappingGet(sub1, "path"); present {
		t.Fatal("path key must be stripped from provider http output")
	}
	if _, present := mappingGet(sub1, "url"); !present {
		t.Fatal("provider url must survive the path strip")
	}
}
