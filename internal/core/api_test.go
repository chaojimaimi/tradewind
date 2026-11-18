package core

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStubAPI 在 unix socket 上起一个最小控制器，返回 (客户端, 清理函数)。
func newStubAPI(t *testing.T, handler http.Handler) (*Client, func()) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln) //nolint:errcheck // 测试桩，Serve 在 Close 时返回 ErrServerClosed
	cleanup := func() { srv.Close(); ln.Close() }
	return NewClient(sock, "testsecret"), cleanup
}

func TestClientVersion(t *testing.T) {
	mux := http.NewServeMux()
	var gotAuth string
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"meta":true,"version":"fake-v9.9.9"}`))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "fake-v9.9.9" {
		t.Fatalf("version = %q", v)
	}
	if gotAuth != "Bearer testsecret" {
		t.Fatalf("auth header = %q, want Bearer testsecret", gotAuth)
	}
}

func TestClientPutConfigs(t *testing.T) {
	mux := http.NewServeMux()
	var gotPath, gotQuery, gotMethod string
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		var body struct {
			Path string `json:"path"`
		}
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		gotPath = body.Path
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	if err := c.PutConfigs(context.Background(), "/data/runtime.yaml"); err != nil {
		t.Fatalf("PutConfigs: %v", err)
	}
	if gotMethod != http.MethodPut || gotQuery != "force=true" {
		t.Fatalf("method=%s query=%s, want PUT force=true", gotMethod, gotQuery)
	}
	if gotPath != "/data/runtime.yaml" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestClientNon2xxIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "parse config failed", http.StatusBadRequest)
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	err := c.PutConfigs(context.Background(), "/data/runtime.yaml")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want status 400 error", err)
	}
}

func TestClientDeadSockFails(t *testing.T) {
	dir := shortTempDir(t)
	c := NewClient(filepath.Join(dir, "never.sock"), "s")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Version(ctx); err == nil {
		t.Fatal("Version on dead sock: want error")
	}
}

func TestClientSlowResponseRespectsCtx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(`{"version":"x"}`))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Version(ctx); err == nil {
		t.Fatal("Version with tight ctx deadline: want error")
	}
}

func TestNewClientMissingSockFileAllowed(t *testing.T) {
	// 构造时不要求 sock 已存在（握手期间 sock 尚未创建是常态），失败推迟到请求时
	dir := shortTempDir(t)
	c := NewClient(filepath.Join(dir, "late.sock"), "s")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "late.sock")); !os.IsNotExist(err) {
		t.Fatal("NewClient must not create sock file")
	}
}
