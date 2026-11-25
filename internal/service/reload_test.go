package service

// reload 的服务级编排测试（批⑥b §4.4 L4）：托管推送（payload 内联、内核接受才落
// 盘）、归一化拒绝与内核 400 两个 rejected 分支、未托管仅落盘。全部无特权：/tmp
// 短路径 + 桩内核。L2/L3/L5 的用例在 lifecycle_test.go，单元级在 snapshot_test.go，
// 协议编解码在 protocol_test.go。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rejectedUserConfig 是归一化必拒的用户文件（rejectKeys 成员 external-ui）。
const rejectedUserConfig = "proxies: []\nexternal-ui: /tmp/x\n"

// providerPathUserConfig 是带 proxy-provider path 的用户文件（ignored 剥离断言用）。
func providerPathUserConfig() []byte {
	return []byte("proxy-providers:\n  sub1:\n    type: http\n" +
		"    url: https://provider.example.invalid/subscription\n    path: ./sub1.yaml\n")
}

// putRecord 是 fakecore FAKE_CORE_PUT_RECORD 单行 JSON 的形状（与 fakecore 文档一致）。
type putRecord struct {
	Method string `json:"method"`
	Query  string `json:"query"`
	Body   string `json:"body"`
}

// readPutRecords 读入并逐行解码记录文件（缺失/空文件 → 0 条；恰一行每请求的计数
// 断言建立在它之上）。
func readPutRecords(t *testing.T, path string) []putRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []putRecord
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec putRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record line %d: %v", i+1, err)
		}
		out = append(out, rec)
	}
	return out
}

// withCoreEnv 起服务并在 coreEnv 缝里追加桩内核的模式变量（生产路径只有固定三变量
// ——追加只在测试发生，见 server_test.go 同款用法）。
func withCoreEnv(extra ...string) func(*Server, *trustRecorder) {
	return func(s *Server, _ *trustRecorder) {
		s.coreEnv = func(c ConfigFile) []string {
			return append(coreEnvProduction(c), extra...)
		}
	}
}

// TestReloadPushesPayloadToManagedKernel (L4 托管分支)：reload 把归一化产物内联推给
// 内核（恰一次 PUT，query force=true，body {"payload": 产物}）；内核接受后才提升
// 快照 + 落盘；ignored 带出被剥离的 provider path，payload 不再含它。
func TestReloadPushesPayloadToManagedKernel(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	record := filepath.Join(dir, "put-records.jsonl")
	srv, _ := startStubServerTuned(t, cfg, withCoreEnv("FAKE_CORE_PUT_RECORD="+record))
	cl := NewClient(cfg.SockPath, 30*time.Second)

	if _, err := cl.Start(1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := readPutRecords(t, record); len(got) != 0 {
		t.Fatalf("records after start = %d, want 0 (spawn never pushes /configs)", len(got))
	}
	if err := os.WriteFile(conf.UserRuntime, providerPathUserConfig(), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := cl.Reload()
	if err != nil || !resp.OK {
		t.Fatalf("Reload = %+v err %v, want ok", resp, err)
	}
	if got, want := resp.Ignored, []string{"proxy-providers.sub1.path"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("ignored = %v, want %v", got, want)
	}

	records := readPutRecords(t, record)
	if len(records) != 1 {
		t.Fatalf("records = %d, want exactly one PUT", len(records))
	}
	if records[0].Method != "PUT" || records[0].Query != "force=true" {
		t.Fatalf("record = %+v, want PUT force=true", records[0])
	}
	var body struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal([]byte(records[0].Body), &body); err != nil {
		t.Fatalf("record body %q: %v", records[0].Body, err)
	}
	product, rej := NormalizeRuntime(providerPathUserConfig())
	if rej != nil {
		t.Fatalf("normalize fixture rejected at %s: %s", rej.Key, rej.Reason)
	}
	if body.Payload != string(product.Bytes) {
		t.Fatalf("pushed payload = %q, want the normalized product %q", body.Payload, product.Bytes)
	}
	if strings.Contains(body.Payload, "path: ./sub1.yaml") {
		t.Fatal("pushed payload still carries the provider path")
	}
	staged, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil || string(staged) != string(product.Bytes) {
		t.Fatalf("R = %q err %v, want the product", staged, err)
	}
	if snap := srv.snapshotCopy(); !bytes.Equal(snap, product.Bytes) {
		t.Fatalf("snapshot = %q, want the product", snap)
	}
}

// TestReloadRejectedByNormalizer (L4 拒绝分支 1)：归一化拒绝 → rejected:true，零次
// 推送，磁盘与快照原封不动。
func TestReloadRejectedByNormalizer(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	record := filepath.Join(dir, "put-records.jsonl")
	srv, _ := startStubServerTuned(t, cfg, withCoreEnv("FAKE_CORE_PUT_RECORD="+record))
	cl := NewClient(cfg.SockPath, 30*time.Second)

	if _, err := cl.Start(1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	beforeDisk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil {
		t.Fatal(err)
	}
	beforeSnap := srv.snapshotCopy()
	if err := os.WriteFile(conf.UserRuntime, []byte(rejectedUserConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	resp, err := cl.Reload()
	if err != nil || resp.OK || !resp.Rejected || !strings.Contains(resp.Error, "external-ui") {
		t.Fatalf("Reload = %+v err %v, want rejected naming the key", resp, err)
	}
	if got := readPutRecords(t, record); len(got) != 0 {
		t.Fatalf("records = %d, want 0 (a rejected config is never pushed)", len(got))
	}
	afterDisk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil || !bytes.Equal(beforeDisk, afterDisk) {
		t.Fatalf("R changed across the rejected reload: %q err %v", afterDisk, err)
	}
	if after := srv.snapshotCopy(); !bytes.Equal(beforeSnap, after) {
		t.Fatal("snapshot changed across the rejected reload")
	}
}

// TestReloadRejectedByKernel (L4 拒绝分支 2)：内核 400 → rejected:true，错误以固定
// 前缀开头；磁盘 == 快照 == 初始已提交的产物（rejected 的 reload 什么都不改）。
func TestReloadRejectedByKernel(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	srv, _ := startStubServerTuned(t, cfg, withCoreEnv("FAKE_CORE_MODE=reject-config"))
	cl := NewClient(cfg.SockPath, 30*time.Second)

	if _, err := cl.Start(1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	beforeSnap := srv.snapshotCopy()
	beforeDisk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeDisk, beforeSnap) {
		t.Fatalf("R %q != snapshot %q after start (initial commit must land both)", beforeDisk, beforeSnap)
	}

	// reload 之前把用户文件换成另一份合法配置：reload 若越序提交（内核 400 之前就
	// 落盘/提升快照），R 与快照会变成这份新产物。WHY 必须换文件：不换的话磁盘上
	// 本就是这份 reload 将提交的产物，"原封不动"与"改写成同样的字节"无法区分
	// （批⑥b c3b 审查 P2——越序变异能通过全套测试）。
	if err := os.WriteFile(conf.UserRuntime, []byte("mode: rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	freshProduct, rej := NormalizeRuntime([]byte("mode: rule\n"))
	if rej != nil || bytes.Equal(freshProduct.Bytes, beforeDisk) {
		t.Fatalf("fixture must yield a product unlike the one on disk (rej %v)", rej)
	}

	resp, err := cl.Reload()
	if err != nil || resp.OK || !resp.Rejected {
		t.Fatalf("Reload = %+v err %v, want rejected", resp, err)
	}
	if !strings.HasPrefix(resp.Error, "service: 内核拒绝新配置：") {
		t.Fatalf("error = %q, want the kernel-rejection prefix", resp.Error)
	}
	afterDisk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil || !bytes.Equal(beforeDisk, afterDisk) {
		t.Fatalf("R changed across the kernel-rejected reload: %q err %v", afterDisk, err)
	}
	if after := srv.snapshotCopy(); !bytes.Equal(beforeSnap, after) {
		t.Fatal("snapshot changed across the kernel-rejected reload")
	}
}

// TestReloadUnmanagedPersistsOnly (L4 未托管分支)：从未启动 → ok:true、仅落盘
// （R == 产物 == 快照）、零推送（记录文件不存在）。
func TestReloadUnmanagedPersistsOnly(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	record := filepath.Join(dir, "put-records.jsonl")
	srv, _ := startStubServerTuned(t, cfg, withCoreEnv("FAKE_CORE_PUT_RECORD="+record))
	cl := NewClient(cfg.SockPath, 30*time.Second)

	resp, err := cl.Reload()
	if err != nil || !resp.OK || resp.Rejected {
		t.Fatalf("Reload = %+v err %v, want ok", resp, err)
	}
	product, rej := NormalizeRuntime([]byte("proxies: []\n"))
	if rej != nil {
		t.Fatalf("normalize fixture rejected at %s: %s", rej.Key, rej.Reason)
	}
	staged, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil || string(staged) != string(product.Bytes) {
		t.Fatalf("R = %q err %v, want the product", staged, err)
	}
	if snap := srv.snapshotCopy(); !bytes.Equal(snap, product.Bytes) {
		t.Fatalf("snapshot = %q, want the product", snap)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("record file must be absent without a managed kernel (err = %v)", err)
	}
}
