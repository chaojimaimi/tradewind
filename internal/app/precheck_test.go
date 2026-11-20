package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// TestUsePrecheckFailureKeepsIndexUntouched 批④ §2.2：Use 预检失败（-t 注入报错）
// → 报错返回且 index/runtime/snapshot 全不动（快照回写未发生、current 不变）。
func TestUsePrecheckFailureKeepsIndexUntouched(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	stub := withMihomoCheckStub(t, true) // -t 恒失败
	_ = stub
	idxBefore, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	runtimeBefore, err := os.ReadFile(home.RuntimeFile())
	if err != nil {
		runtimeBefore = nil // 未合成过：nil 基线
	}

	err = Use(context.Background(), home, "second sub")
	if err == nil || !strings.Contains(err.Error(), "目标订阅合成后未通过 mihomo -t 校验") {
		t.Fatalf("err = %v, want precheck failure message", err)
	}
	// 脱敏：错误不泄露候选临时文件路径
	if strings.Contains(err.Error(), home.String()) {
		t.Fatalf("error must redact candidate path: %v", err)
	}
	idxAfter, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	if idxAfter.Current != idxBefore.Current || len(idxAfter.Items) != len(idxBefore.Items) {
		t.Fatalf("index changed on precheck failure: %+v vs %+v", idxAfter, idxBefore)
	}
	for i := range idxAfter.Items {
		if idxAfter.Items[i].Selected != nil && idxBefore.Items[i].Selected == nil {
			t.Fatalf("snapshot side effect on precheck failure: %+v", idxAfter.Items[i])
		}
	}
	runtimeAfter, err := os.ReadFile(home.RuntimeFile())
	if runtimeBefore == nil {
		if err == nil {
			t.Fatal("runtime.yaml must not be created on precheck failure")
		}
	} else if string(runtimeAfter) != string(runtimeBefore) {
		t.Fatal("runtime.yaml changed on precheck failure")
	}
	// 错误文案不含订阅 URL/token（纪律 6）
	for _, item := range idxAfter.Items {
		if item.URL != "" && strings.Contains(err.Error(), item.URL) {
			t.Fatalf("error must not contain subscription url: %v", err)
		}
	}
}

// TestUsePrecheckComposeFailureKeepsIndexUntouched 合成失败（read profile 路径缺失）
// 同样零落盘。
func TestUsePrecheckComposeFailureKeepsIndexUntouched(t *testing.T) {
	home := seedEnhanceHome(t)
	idxBefore, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	// 目标订阅原文件缺失 → 合成读盘失败
	if err := os.Remove(home.ProfileFile("new02.yaml")); err != nil {
		t.Fatal(err)
	}
	err = Use(context.Background(), home, "second sub")
	// 目标原文件缺失：Use 在预检读盘阶段显式报错（报错文案与 Synthesize 读盘同款），
	// 关键断言是零落盘
	if err == nil || !strings.Contains(err.Error(), "read profile") {
		t.Fatalf("err = %v, want profile read failure", err)
	}
	idxAfter, _ := store.LoadIndex(home)
	if idxAfter.Current != idxBefore.Current {
		t.Fatalf("current changed on compose failure: %q", idxAfter.Current)
	}
}

// TestUpdatePrecheckFailureKeepsOldFile 批④ §2.2 评审 P1-3：update 的校验前置于
// SaveProfileFile——失败=旧文件原样、Updated/Extra 不动（不留下换血后的订阅文件）。
func TestUpdatePrecheckFailureKeepsOldFile(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	withMihomoCheckStub(t, true) // -t 恒失败

	oldBody := []byte("proxies:\n  - name: old-node\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n")
	if err := store.SaveProfileFile(home, "old01.yaml", oldBody); err != nil {
		t.Fatal(err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := idx.ByUID("old01")
	item.Type = store.ProfileTypeRemote
	item.URL = "placeholder" // 由下方 stub 服务器改写
	item.Updated = 12345
	if err := store.SaveIndex(home, idx); err != nil {
		t.Fatal(err)
	}

	// 本地 HTTP 服务器供 fetch（直连；update_via_proxy 默认开但内核不可达 → 回退直连）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=100; expire=0")
		_, _ = w.Write([]byte("proxies:\n  - name: new-node\n    type: socks5\n    server: 127.0.0.1\n    port: 1081\n"))
	}))
	defer srv.Close()
	idx, _ = store.LoadIndex(home)
	item, _ = idx.ByUID("old01")
	item.URL = srv.URL
	if err := store.SaveIndex(home, idx); err != nil {
		t.Fatal(err)
	}

	settings, err := store.LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	idx, _ = store.LoadIndex(home)
	item, _ = idx.ByUID("old01")
	err = updateOne(context.Background(), home, settings, item)
	if err == nil || !strings.Contains(err.Error(), "目标订阅合成后未通过 mihomo -t 校验，已保持当前状态不变") {
		t.Fatalf("err = %v, want precheck failure", err)
	}
	data, err := os.ReadFile(home.ProfileFile("old01.yaml"))
	if err != nil || string(data) != string(oldBody) {
		t.Fatalf("profile file changed on precheck failure: %q err %v", data, err)
	}
	if item.Updated != 12345 {
		t.Fatalf("Updated mutated on precheck failure: %d", item.Updated)
	}
	if item.Extra.Total != 0 {
		t.Fatalf("Extra mutated on precheck failure: %+v", item.Extra)
	}
}

// TestUpdatePrecheckPassesWritesFile 校验通过 → 正常落盘（预检不误伤合法更新）。
func TestUpdatePrecheckPassesWritesFile(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	withMihomoCheckStub(t, false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxies:\n  - name: new-node\n    type: socks5\n    server: 127.0.0.1\n    port: 1081\n"))
	}))
	defer srv.Close()

	idx, err := store.LoadIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := idx.ByUID("old01")
	item.Type = store.ProfileTypeRemote
	item.URL = srv.URL
	if err := store.SaveIndex(home, idx); err != nil {
		t.Fatal(err)
	}
	settings, err := store.LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	idx, _ = store.LoadIndex(home)
	item, _ = idx.ByUID("old01")
	if err := updateOne(context.Background(), home, settings, item); err != nil {
		t.Fatalf("updateOne with passing check: %v", err)
	}
	data, err := os.ReadFile(home.ProfileFile("old01.yaml"))
	if err != nil || !strings.Contains(string(data), "new-node") {
		t.Fatalf("new content not saved: %q err %v", data, err)
	}
}

// TestPrecheckBinaryMissingSkipsCheck 二进制缺失零开销跳过（Use 照常可用）。
func TestPrecheckBinaryMissingSkipsCheck(t *testing.T) {
	home := seedEnhanceHome(t) // 无 bin/mihomo
	calls := withMihomoCheckStub(t, true)
	if err := Use(context.Background(), home, "second sub"); err != nil {
		t.Fatalf("Use without binary: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("check must be skipped without binary: %v", *calls)
	}
	idx, _ := store.LoadIndex(home)
	if idx.Current != "new02" {
		t.Fatalf("current = %q, want new02", idx.Current)
	}
}
