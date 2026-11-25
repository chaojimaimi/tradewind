package core

// reload 支撑面（批⑥b §4.4）：do() 的非 2xx 携带体（StatusError，legacy 文案逐字
// 钉死）、PutConfigsPayload 的线上字节形态、fakecore 的 PUT 记录与 reject-config
// 两种测试模式的行为。
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClientNon2xxReturnsStatusError 非 2xx → *StatusError（errors.As 可取回 Code），
// 且 Error() 逐字复现 do() 的既有文案——既有调用方与测试钉死了这段文本，不得漂移。
func TestClientNon2xxReturnsStatusError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("boom body"))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	err := c.PutConfigs(context.Background(), "/data/runtime.yaml")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *StatusError", err, err)
	}
	if se.Code != 400 {
		t.Fatalf("Code = %d, want 400", se.Code)
	}
	if want := "core: PUT /configs?force=true: status 400: boom body"; err.Error() != want {
		t.Fatalf("err = %q, want the legacy text %q", err, want)
	}
}

// TestClientPutConfigsPayloadWireShape 内联推送的线上形态：PUT /configs?force=true，
// body 逐字节等于 {"payload": <payload>}——payload 里的引号与换行以 JSON 转义出现。
func TestClientPutConfigsPayloadWireShape(t *testing.T) {
	mux := http.NewServeMux()
	var gotMethod, gotQuery, gotBody string
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	payload := "line1\nhas \"quotes\""
	if err := c.PutConfigsPayload(context.Background(), payload); err != nil {
		t.Fatalf("PutConfigsPayload: %v", err)
	}
	if gotMethod != http.MethodPut || gotQuery != "force=true" {
		t.Fatalf("method=%s query=%s, want PUT force=true", gotMethod, gotQuery)
	}
	if want := `{"payload":"line1\nhas \"quotes\""}`; gotBody != want {
		t.Fatalf("body = %q, want %q (quote/newline JSON-escaped)", gotBody, want)
	}
}

// waitFakeCoreSock 等待桩内核的 sock 出现（子进程 listen 需要一点时间）。
func waitFakeCoreSock(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fakecore sock %s did not appear within 5s", sock)
}

// startFakeCore 以指定模式拉起桩内核子进程（本测试自己拉起的进程，Cleanup 里由
// 本测试 kill + 收尸）。返回 sock 路径。
func startFakeCore(t *testing.T, dir, mode string, extraEnv ...string) string {
	t.Helper()
	sock := filepath.Join(dir, "core.sock")
	runtime := writeTempFile(t, dir, "runtime.yaml", "proxies: []\n")
	cmd := exec.Command(fakeCoreBin, "-d", dir, "-f", runtime, "-ext-ctl-unix", sock)
	cmd.Env = fakeCoreEnv(append([]string{"FAKE_CORE_MODE=" + mode}, extraEnv...)...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fakecore: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait() // 收尸：不留僵尸，processAlive 类判据不受干扰
	})
	waitFakeCoreSock(t, sock)
	return sock
}

// fakeRecordLine 是 FAKE_CORE_PUT_RECORD 单行 JSON 的形状（与 fakecore 文档一致）。
type fakeRecordLine struct {
	Method string `json:"method"`
	Query  string `json:"query"`
	Body   string `json:"body"`
}

// readFakeRecords 读入并逐行解码记录文件（缺失 → 0 条）。
func readFakeRecords(t *testing.T, path string) []fakeRecordLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []fakeRecordLine
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec fakeRecordLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestFakeCorePutRecordOneLinePerConfigRequest 记录模式：恰一行每个 /configs 请求、
// 字段为 method/query/body；/version 与其余路径零写入（记录文件都不创建）。
func TestFakeCorePutRecordOneLinePerConfigRequest(t *testing.T) {
	dir := shortTempDir(t)
	rec := filepath.Join(dir, "put-records.jsonl")
	sock := startFakeCore(t, dir, "normal", "FAKE_CORE_PUT_RECORD="+rec)
	c := NewClient(sock, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got := readFakeRecords(t, rec); len(got) != 0 {
		t.Fatalf("records after /version = %d, want 0 (nothing written for other paths)", len(got))
	}
	if err := c.PutConfigsPayload(ctx, "proxies: []\n"); err != nil {
		t.Fatalf("PutConfigsPayload: %v", err)
	}
	if err := c.PutConfigsPayload(ctx, "proxies: []\n"); err != nil {
		t.Fatalf("PutConfigsPayload #2: %v", err)
	}
	records := readFakeRecords(t, rec)
	if len(records) != 2 {
		t.Fatalf("records = %d, want exactly one line per /configs request", len(records))
	}
	for i, rec := range records {
		if rec.Method != "PUT" || rec.Query != "force=true" {
			t.Fatalf("record %d = %+v, want PUT force=true", i+1, rec)
		}
	}
	// 键序按文档钉死（{"method":...,"query":...,"body":...}）
	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"method":"PUT","query":"force=true","body":`) {
		t.Fatalf("record line prefix = %q, want the documented key order", raw)
	}
}

// TestFakeCoreRejectConfigAnswers400 reject-config 模式：/version 照常（握手路径通），
// PUT /configs 一律 400 + {"message":"fakecore: config rejected"}。
func TestFakeCoreRejectConfigAnswers400(t *testing.T) {
	dir := shortTempDir(t)
	sock := startFakeCore(t, dir, "reject-config")
	c := NewClient(sock, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := c.Version(ctx)
	if err != nil || v == "" {
		t.Fatalf("Version = %q err %v, want success (the handshake path must work)", v, err)
	}
	err = c.PutConfigsPayload(ctx, "proxies: []\n")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v, want *StatusError with Code 400", err)
	}
	if !strings.Contains(se.Body, "fakecore: config rejected") {
		t.Fatalf("body = %q, want the fakecore rejection message", se.Body)
	}
}
