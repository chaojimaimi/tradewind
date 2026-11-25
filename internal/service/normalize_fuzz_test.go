// normalize_fuzz_test.go：归一化器的模糊测试（批⑥b §4.2 幂等、§4.4 L1）。
//
// 归一化器是特权服务面对用户输入的解析前端，守两条不变量：
//   - 任意字节不 panic（root 守护进程里一次 panic = 一次被 launchd 拉起的重启）；
//   - 只要接受，产物就是不动点：再归一化一次逐字节相同、Ignored 为空。L1 启动时
//     "用当前归一化器复验磁盘产物"靠它——不是不动点，服务会在重启时拒掉自己落盘
//     的那份配置。
//
// 常规 go test 只跑种子语料；要真 fuzz：go test -run '^$' -fuzz FuzzNormalize
// -fuzztime 60s ./internal/service/
package service

import (
	"bytes"
	"testing"

	"gopkg.in/yaml.v3"
)

func FuzzNormalize(f *testing.F) {
	f.Add([]byte("mode: rule\nallow-lan: true\n"))
	f.Add([]byte("proxies:\n  - name: n\n    type: ss\n    server: 1.2.3.4\n    port: 1\n    password: p\n"))
	f.Add([]byte("tun: x\nexternal-controller: y\ndns:\n  listen: \"\"\n"))
	f.Add([]byte("proxy-providers:\n  p:\n    type: http\n    url: https://x.invalid/a\n    path: ./p.yaml\n"))
	f.Add([]byte("a: &x 1\nb: *x\nc: !!binary aGk=\nd: !custom v\n"))
	f.Add([]byte("mode: rule\n---\nmode: global\n"))
	f.Add([]byte("{a: [1, {b: \"x\\ny\", c: |-\n  l1\n  l2\n}], d: ~}\n"))
	f.Add([]byte("\xef\xbb\xbf---\r\nmode: rule\r\n...\r\n"))
	f.Add(fullHouseFixture())
	f.Fuzz(func(t *testing.T, in []byte) {
		res, rej := NormalizeRuntime(in)
		if rej != nil {
			if len(rej.Key) > keyPathMaxBytes {
				t.Fatalf("rejection key is %d bytes, want at most %d", len(rej.Key), keyPathMaxBytes)
			}
			return
		}
		second, rej2 := NormalizeRuntime(res.Bytes)
		if rej2 != nil {
			t.Fatalf("output rejected on second pass at %s: %s\ninput: %q\noutput: %q", rej2.Key, rej2.Reason, in, res.Bytes)
		}
		if !bytes.Equal(res.Bytes, second.Bytes) || len(second.Ignored) != 0 {
			t.Fatalf("output is not a fixed point (Ignored %v)\ninput: %q\nfirst: %q\nsecond: %q", second.Ignored, in, res.Bytes, second.Bytes)
		}
		// 产物里恰有一个 tun、一个 external-controller，且无别名/锚点
		var doc yaml.Node
		if err := yaml.Unmarshal(res.Bytes, &doc); err != nil {
			t.Fatalf("output does not parse: %v\ninput: %q\noutput: %q", err, in, res.Bytes)
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			t.Fatalf("output root is not a mapping\ninput: %q\noutput: %q", in, res.Bytes)
		}
		root := doc.Content[0]
		pins := map[string]int{}
		for i := 0; i+1 < len(root.Content); i += 2 {
			if pinnedKeys[root.Content[i].Value] {
				pins[root.Content[i].Value]++
			}
		}
		if pins["tun"] != 1 || pins["external-controller"] != 1 {
			t.Fatalf("pinned key counts = %v, want exactly one each\noutput: %q", pins, res.Bytes)
		}
		var walk func(*yaml.Node)
		walk = func(n *yaml.Node) {
			if n.Kind == yaml.AliasNode || n.Anchor != "" {
				t.Fatalf("output carries an alias/anchor\ninput: %q\noutput: %q", in, res.Bytes)
			}
			for _, c := range n.Content {
				walk(c)
			}
		}
		walk(root)
	})
}
