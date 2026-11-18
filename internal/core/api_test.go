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

func TestClientGetProxies(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"proxies":{
			"🚀 节点选择":{"name":"🚀 节点选择","type":"Selector","now":"FAKE-HK-01","all":["FAKE-HK-01","FAKE-JP-01"],"history":[{"time":"t","delay":120}]},
			"AUTO":{"name":"AUTO","type":"URLTest","now":"FAKE-JP-01","all":["FAKE-HK-01"]},
			"FAKE-HK-01":{"name":"FAKE-HK-01","type":"Socks5","history":[]}
		}}`))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	proxies, err := c.GetProxies(context.Background())
	if err != nil {
		t.Fatalf("GetProxies: %v", err)
	}
	g := proxies["🚀 节点选择"]
	if !g.IsGroup() || g.Now != "FAKE-HK-01" || len(g.All) != 2 {
		t.Fatalf("group entry = %+v", g)
	}
	if d, ok := g.LastDelay(); !ok || d != 120 {
		t.Fatalf("LastDelay = %d %v, want 120 true", d, ok)
	}
	if d, ok := proxies["FAKE-HK-01"].LastDelay(); ok {
		t.Fatalf("empty history LastDelay = %d, want false", d)
	}
	if proxies["FAKE-HK-01"].IsGroup() {
		t.Fatal("Socks5 entry must not be a group")
	}
}

func TestClientSelectProxyEscapesGroupName(t *testing.T) {
	// emoji+空格组名必须以 path 段转义形式上线（断言原始 RequestURI，M1-plan §5.1）
	mux := http.NewServeMux()
	var gotURI, gotBody string
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	if err := c.SelectProxy(context.Background(), "🚀 节点选择", "FAKE-JP-01"); err != nil {
		t.Fatalf("SelectProxy: %v", err)
	}
	if gotURI != "/proxies/%F0%9F%9A%80%20%E8%8A%82%E7%82%B9%E9%80%89%E6%8B%A9" {
		t.Fatalf("request URI = %q, want escaped emoji group segment", gotURI)
	}
	if gotBody != `{"name":"FAKE-JP-01"}` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestClientProxyDelay(t *testing.T) {
	mux := http.NewServeMux()
	var gotQuery string
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"delay":230}`))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	d, err := c.ProxyDelay(context.Background(), "🇭🇰 HK-01", 5000, "http://www.gstatic.com/generate_204")
	if err != nil {
		t.Fatalf("ProxyDelay: %v", err)
	}
	if d != 230 {
		t.Fatalf("delay = %d, want 230", d)
	}
	if gotQuery != "timeout=5000&url=http%3A%2F%2Fwww.gstatic.com%2Fgenerate_204" {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestClientProxyDelayEmojiNameEscaped(t *testing.T) {
	mux := http.NewServeMux()
	var gotURI string
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		w.Write([]byte(`{"delay":1}`))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	if _, err := c.ProxyDelay(context.Background(), "🇭🇰 HK-01", 1000, "http://x"); err != nil {
		t.Fatalf("ProxyDelay: %v", err)
	}
	if !strings.HasPrefix(gotURI, "/proxies/%F0%9F%87%AD%F0%9F%87%B0%20HK-01/delay") {
		t.Fatalf("request URI = %q, want escaped emoji node segment", gotURI)
	}
}

func TestClientProxyDelayFailureIsError(t *testing.T) {
	// 实测 mihomo v1.19.34：不可达节点返回 503 + {"message":...}，必须折为 error
	mux := http.NewServeMux()
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"An error occurred in the delay test"}`))
	})
	c, cleanup := newStubAPI(t, mux)
	defer cleanup()

	if _, err := c.ProxyDelay(context.Background(), "n1", 100, "http://x"); err == nil {
		t.Fatal("delay failure: want error")
	}
}
