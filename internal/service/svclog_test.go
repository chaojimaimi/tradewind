package service

// svclog_test.go：service.log 行格式助手的单测（c8 ①）——时间戳格式恰为
// "2006-01-02 15:04:05 "、内容原样、单行（换行由助手统一补）。既有日志用例的
// 适配在 broker_conn_test.go（行长度的计量段）。

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStd 换 os.Stderr/os.Stdout 变量截获（captureServiceLog 同款原理——`go test
// -json` 下 testing 包把 os.Stderr 指到 os.Stdout，fd 层重定向截不到）。只在没有
// 任何服务协程存活的用例里用；调用点自己负责还原。
func captureStd(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close captured log: %v", err)
		}
	})
	return f
}

// assertTimestampedLine 断言 log 是恰好一行、行首 19 字节是合法时间戳 + 1 个空格、
// 其后内容为 wantContent（时间戳 + 空格共 20 字节）。
func assertTimestampedLine(t *testing.T, log, wantContent string) {
	t.Helper()
	if !strings.HasSuffix(log, "\n") || strings.Count(log, "\n") != 1 {
		t.Fatalf("log must be exactly one newline-terminated line, got %q", log)
	}
	body := strings.TrimSuffix(log, "\n")
	if len(body) < 20 {
		t.Fatalf("log too short to carry a timestamp: %q", log)
	}
	if _, err := time.Parse(logTimestamp, body[:19]); err != nil {
		t.Fatalf("timestamp prefix %q does not parse as %q: %v", body[:19], logTimestamp, err)
	}
	if got := body[20:]; got != wantContent {
		t.Fatalf("content after timestamp = %q, want %q", got, wantContent)
	}
}

func TestSvcErrfWritesTimestampedSingleLine(t *testing.T) {
	dir := shortTempDir(t)
	logPath := filepath.Join(dir, "svc-stderr.log")
	f := captureStd(t, logPath)
	prev := os.Stderr
	os.Stderr = f
	svcErrf("service: probe %d %s", 7, "x")
	os.Stderr = prev
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	assertTimestampedLine(t, string(data), "service: probe 7 x")
}

func TestSvcOutfWritesTimestampedSingleLine(t *testing.T) {
	dir := shortTempDir(t)
	logPath := filepath.Join(dir, "svc-stdout.log")
	f := captureStd(t, logPath)
	prev := os.Stdout
	os.Stdout = f
	svcOutf("service: success line")
	os.Stdout = prev
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	assertTimestampedLine(t, string(data), "service: success line")
}

// svcErrf 读的是 os.Stderr 变量本身：captureServiceLog 换变量后写进替换后的流，
// 还原后写回真实 stderr（截获语义与既有 Fprintf 调用点一致）。
func TestSvcErrfFollowsStderrVariableSwap(t *testing.T) {
	dir := shortTempDir(t)
	logPath := filepath.Join(dir, "svc-swapped.log")
	f := captureStd(t, logPath)
	prev := os.Stderr
	os.Stderr = f
	svcErrf("service: into captured stream")
	os.Stderr = prev
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "service: into captured stream") {
		t.Fatalf("captured stream missing the line: %q", data)
	}
}

// TestSvcLogWriterTimestampsEachWrite ErrorLog 的接线形态：log.New(w, "", 0) 的每笔
// 写入（恰好一行、自带换行）补上时间戳前缀，内容原样、报告的写入长度不缩水。
func TestSvcLogWriterTimestampsEachWrite(t *testing.T) {
	var buf bytes.Buffer
	w := svcLogWriter(&buf)
	n, err := io.WriteString(w, "service: http server error\n")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("service: http server error\n") {
		t.Fatalf("Write reported %d bytes, want the full input length", n)
	}
	assertTimestampedLine(t, buf.String(), "service: http server error")
}
