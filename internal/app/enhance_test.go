package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// seedEnhanceHome 是 enhance 编排测试的最小数据目录（current 订阅 + settings + secret）。
func seedEnhanceHome(t *testing.T) paths.Home {
	t.Helper()
	home := paths.Home(t.TempDir())
	seedTwoProfiles(t, home)
	if err := store.SaveSettings(home, store.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	return home
}

// withMihomoCheckStub 替换 runMihomoCheck 缝隙并记录调用；t.Cleanup 还原。
func withMihomoCheckStub(t *testing.T, fail bool) *[]string {
	t.Helper()
	var calls []string
	old := runMihomoCheck
	runMihomoCheck = func(binPath, configPath, dataDir string) (string, error) {
		calls = append(calls, configPath)
		if fail {
			return "configuration file <candidate>: yaml error at line 9\n", errStubbedCheck
		}
		return "TEST ok\n", nil
	}
	t.Cleanup(func() { runMihomoCheck = old })
	return &calls
}

var errStubbedCheck = errStubbedCheckImpl{}

type errStubbedCheckImpl struct{}

func (errStubbedCheckImpl) Error() string { return "stubbed check failure" }

// fakeCoreBinary 写一个可执行的假 mihomo（存在性即可触发校验链，不真执行）。
func fakeCoreBinary(t *testing.T, home paths.Home) {
	t.Helper()
	binDir := filepath.Join(home.String(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(home.Bin(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestEnhanceSetWritesValidatesAndSynthesizes(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	calls := withMihomoCheckStub(t, false)
	var progress strings.Builder
	content := []byte("- DOMAIN-SUFFIX,example.com,DIRECT\n")
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", content, &progress); err != nil {
		t.Fatalf("EnhanceSet: %v", err)
	}
	// 槽文件 600 落位
	path := home.EnhanceSlotFile("old01", EnhanceSlotRules)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(content) {
		t.Fatalf("slot file = %q, err = %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("slot perm = %v, want 600", info.Mode().Perm())
	}
	// 校验链被挂载且候选临时文件位于数据目录内（0600）
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], home.String()) {
		t.Fatalf("check calls = %v, want candidate inside data dir", *calls)
	}
	// runtime.yaml 已重合成（含槽内容）
	runtime, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !strings.Contains(string(runtime), "DOMAIN-SUFFIX,example.com,DIRECT") {
		t.Fatalf("runtime missing slot rule: %v", err)
	}
	// 内核未运行：ReloadIfRunning 无操作（不报错即语义成立）
}

// TestEnhanceSetKeepsStdoutPureWithCoreRunning（批③ code-reviewer P1-1）：内核运行中
// reload.go 既有 stdout 打印不得混入 enhance 编排——全程 progress 承载，os.Stdout
// 编排期零字节（CLI 层 stdout 仅最终一行 JSON，壳 decodeJSON 才不会假失败）。
func TestEnhanceSetKeepsStdoutPureWithCoreRunning(t *testing.T) {
	// WHY 短路径 home（评审折入实测）：macOS unix socket bind 有 104 字符路径上限，
	// testing.T.TempDir（/var/folders/...）超限 bind: invalid argument——socket 必须
	// 落在 /tmp 短前缀下
	base, err := os.MkdirTemp("/tmp", "tw-enhcore-")
	if err != nil {
		t.Fatalf("short tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	home := paths.Home(filepath.Join(base, "data"))
	seedTwoProfiles(t, home)
	if err := store.SaveSettings(home, store.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	fakeCoreBinary(t, home)
	withMihomoCheckStub(t, false)

	// 假内核：unix socket 上 /version、/configs 全 200（CoreReachable+PutConfigs 通过）
	listener, err := net.Listen("unix", home.SockFile())
	if err != nil {
		t.Fatalf("listen fake core sock: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"v1.19.34"}`))
		case "/configs":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	// 劫持 os.Stdout 捕获编排期泄漏
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	stdoutCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		stdoutCh <- string(data)
	}()
	t.Cleanup(func() {
		os.Stdout = oldStdout
		r.Close()
	})

	var progress strings.Builder
	content := []byte("- DOMAIN-SUFFIX,example.com,DIRECT\n")
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", content, &progress); err != nil {
		t.Fatalf("EnhanceSet with core running: %v", err)
	}
	w.Close() // 关写侧让读 goroutine 收尾（编排已结束，无后续写入）
	if got := <-stdoutCh; got != "" {
		t.Fatalf("编排期 stdout 泄漏 %q（--json 契约：stdout 仅 CLI 层最终一行）", got)
	}
	if !strings.Contains(progress.String(), "core hot-reloaded") {
		t.Fatalf("progress 缺少热重载回执: %q", progress.String())
	}
}

func TestEnhanceSetSyntaxErrorNotWritten(t *testing.T) {
	// yaml 语法错误在写盘前预检（§3.3）：报错不落盘
	home := seedEnhanceHome(t)
	calls := withMihomoCheckStub(t, false)
	var progress strings.Builder
	bad := []byte("rules: [broken\n")
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", bad, &progress); err == nil {
		t.Fatal("syntax error: want error")
	}
	if _, err := os.Stat(home.EnhanceSlotFile("old01", EnhanceSlotRules)); !os.IsNotExist(err) {
		t.Fatal("broken yaml must not be written to slot")
	}
	if len(*calls) != 0 {
		t.Fatalf("precheck failure must not reach mihomo -t: %v", *calls)
	}
}

func TestEnhanceSetShapeErrorNotWritten(t *testing.T) {
	// 三槽顶层必须是序列（映射形态拒绝且不落盘）
	home := seedEnhanceHome(t)
	var progress strings.Builder
	bad := []byte("rules-prepend: []\n")
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", bad, &progress); err == nil {
		t.Fatal("mapping-shaped slot: want error")
	}
	if _, err := os.Stat(home.EnhanceSlotFile("old01", EnhanceSlotRules)); !os.IsNotExist(err) {
		t.Fatal("wrong shape must not be written")
	}
}

func TestEnhanceSetValidationFailureRollsBack(t *testing.T) {
	// -t 校验失败 → 槽回滚旧内容（batch3 §3.2）
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	withMihomoCheckStub(t, true)
	slotPath := home.EnhanceSlotFile("old01", EnhanceSlotRules)
	old := []byte("- DOMAIN-SUFFIX,keep.example,DIRECT\n")
	if err := store.WriteFileRaw(slotPath, old); err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", []byte("- MATCH,REJECT\n"), &progress); err == nil {
		t.Fatal("validation failure: want error")
	}
	data, err := os.ReadFile(slotPath)
	if err != nil || string(data) != string(old) {
		t.Fatalf("slot after rollback = %q, err = %v; want old content", data, err)
	}
}

func TestEnhanceClearClearsSlotAndRollsBackOnFailure(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	slotPath := home.EnhanceSlotFile("old01", EnhanceSlotRules)
	old := []byte("- DOMAIN-SUFFIX,keep.example,DIRECT\n")
	if err := store.WriteFileRaw(slotPath, old); err != nil {
		t.Fatal(err)
	}
	// 成功清除：写 0 字节
	stub := withMihomoCheckStub(t, false)
	var progress strings.Builder
	if err := EnhanceClear(context.Background(), home, EnhanceSlotRules, "", &progress); err != nil {
		t.Fatalf("EnhanceClear: %v", err)
	}
	data, err := os.ReadFile(slotPath)
	if err != nil || len(data) != 0 {
		t.Fatalf("slot after clear = %q, err = %v; want 0 bytes", data, err)
	}
	// 失败清除：回滚旧内容
	_ = stub
	fails := withMihomoCheckStub(t, true)
	_ = fails
	if err := store.WriteFileRaw(slotPath, old); err != nil {
		t.Fatal(err)
	}
	if err := EnhanceClear(context.Background(), home, EnhanceSlotRules, "", &progress); err == nil {
		t.Fatal("clear validation failure: want error")
	}
	data, err = os.ReadFile(slotPath)
	if err != nil || string(data) != string(old) {
		t.Fatalf("slot after failed clear = %q, err = %v; want old content", data, err)
	}
}

func TestEnhanceBinaryMissingSkipsCheck(t *testing.T) {
	// 二进制缺失（裸机/未安装）跳过校验不阻断（best-effort，batch3 §3.2）
	home := seedEnhanceHome(t) // 无 bin/mihomo
	calls := withMihomoCheckStub(t, false)
	var progress strings.Builder
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "", []byte("- MATCH,DIRECT\n"), &progress); err != nil {
		t.Fatalf("EnhanceSet without binary: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("check must be skipped without binary: %v", *calls)
	}
	if !strings.Contains(progress.String(), "skip semantic validation") {
		t.Fatalf("progress = %q, want skip note", progress.String())
	}
}

func TestEnhanceShowJSONContract(t *testing.T) {
	home := seedEnhanceHome(t)
	// 缺文件 → exists=false（空转态不是错误）
	line, err := EnhanceShowJSONLine(home, EnhanceSlotRules, "")
	if err != nil {
		t.Fatalf("show missing slot: %v", err)
	}
	var report enhanceShowJSON
	if err := json.Unmarshal([]byte(line), &report); err != nil || report.Exists {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if report.Slot != "rules" || report.UID != "old01" {
		t.Fatalf("slot/uid = %s/%s, want rules/old01 (current)", report.Slot, report.UID)
	}
	// 有文件 → exists=true + 原文
	content := []byte("- DOMAIN-SUFFIX,x.example,DIRECT\n")
	if err := store.WriteFileRaw(home.EnhanceSlotFile("old01", EnhanceSlotRules), content); err != nil {
		t.Fatal(err)
	}
	line, err = EnhanceShowJSONLine(home, EnhanceSlotRules, "old01")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil || !report.Exists || report.Content != string(content) {
		t.Fatalf("report = %+v", report)
	}
	// 全局槽：uid 空、路径 enhance/merge.yaml
	if err := store.WriteFileRaw(home.EnhanceGlobalMerge(), []byte("rules-prepend: []\n")); err != nil {
		t.Fatal(err)
	}
	line, err = EnhanceShowJSONLine(home, EnhanceSlotGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil || !report.Exists || report.UID != "" {
		t.Fatalf("global report = %+v", report)
	}
	// 非法槽名 → 错误（--json 行内折叠由 EnhanceShowJSONContractLine 保证恒退出 0）
	if bad := EnhanceShowJSONContractLine(home, "nope", ""); !strings.HasPrefix(bad, `{"error":`) {
		t.Fatalf("bad slot line = %q", bad)
	}
}

func TestEnhanceSetJSONLineContract(t *testing.T) {
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	withMihomoCheckStub(t, false)
	// 成功 → {"ok":true}；失败 → {"error":...}（单行、恒退出 0）
	if line := EnhanceSetJSONLine(context.Background(), home, EnhanceSlotRules, "", []byte("- MATCH,DIRECT\n")); line != `{"ok":true}` {
		t.Fatalf("ok line = %q", line)
	}
	fails := withMihomoCheckStub(t, true)
	_ = fails
	line := EnhanceSetJSONLine(context.Background(), home, EnhanceSlotRules, "", []byte("- MATCH,REJECT\n"))
	if !strings.HasPrefix(line, `{"error":`) || strings.ContainsAny(line, "\n") {
		t.Fatalf("error line = %q", line)
	}
	// 恢复成功 stub：clear 走同一契约
	withMihomoCheckStub(t, false)
	if line := EnhanceClearJSONLine(context.Background(), home, EnhanceSlotRules, ""); line != `{"ok":true}` {
		t.Fatalf("clear ok line = %q", line)
	}
}

func TestEnhanceSetUnknownUIDRejected(t *testing.T) {
	// --uid 校验存在（resolveProfile）
	home := seedEnhanceHome(t)
	var progress strings.Builder
	if err := EnhanceSet(context.Background(), home, EnhanceSlotRules, "ghost", []byte("- MATCH,DIRECT\n"), &progress); err == nil {
		t.Fatal("unknown uid: want error")
	}
}

func TestUseAndSettingsBypassSemanticValidation(t *testing.T) {
	// 既有写路径（use/settings）失败面维持现状（评审二轮 P1-2 折入）：
	// 不挂 -t 校验；坏增强文件在场时 use/settings 照常落盘
	home := seedEnhanceHome(t)
	fakeCoreBinary(t, home)
	// 预置增强文件（对 synth 合法；-t 若被挂载会拦"配其他订阅非法"的组合——
	// 断言点：既有写路径根本不调用 -t，失败面与批③之前一致）
	if err := store.WriteFileRaw(home.EnhanceSlotFile("old01", EnhanceSlotRules), []byte("- DOMAIN-SUFFIX,enhanced.example,DIRECT\n")); err != nil {
		t.Fatal(err)
	}
	calls := withMihomoCheckStub(t, false)
	// use：不触发校验、照常切换
	if err := Use(context.Background(), home, "new02"); err != nil {
		t.Fatalf("Use with enhance files present: %v", err)
	}
	idx, err := store.LoadIndex(home)
	if err != nil || idx.Current != "new02" {
		t.Fatalf("current = %q, err = %v", idx.Current, err)
	}
	// settings：同样不触发校验
	if err := SettingsSet(context.Background(), home, "log_retention_days", "21"); err != nil {
		t.Fatalf("SettingsSet with enhance files present: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("existing write paths must not invoke mihomo -t: %v", *calls)
	}
}
