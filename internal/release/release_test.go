package release

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// fakeGitHub 是列表端点假 API（httptest 全假 API，不出网）。
// 请求记录供断言：端点形态、Accept 头、Authorization 头。
type fakeGitHub struct {
	server  *httptest.Server
	client  *http.Client
	mu      safeRecorder
	servers []recordedRequest
}

type recordedRequest struct {
	path   string
	accept string
	auth   string
}

// safeRecorder 是并发安全的请求记录（httptest handler goroutine 与断言方互斥）。
type safeRecorder struct {
	ch chan recordedRequest
}

func (r *safeRecorder) add(req recordedRequest) { r.ch <- req }

// drain 取走全部记录（channel 传递天然免锁，测试串行消费）。
func (r *safeRecorder) drain(t *testing.T) []recordedRequest {
	t.Helper()
	var out []recordedRequest
	for {
		select {
		case req := <-r.ch:
			out = append(out, req)
		default:
			return out
		}
	}
}

func newFakeGitHub(t *testing.T, handler http.HandlerFunc) *fakeGitHub {
	t.Helper()
	rec := safeRecorder{ch: make(chan recordedRequest, 64)}
	full := func(w http.ResponseWriter, r *http.Request) {
		rec.add(recordedRequest{path: r.URL.Path + "?" + r.URL.RawQuery, accept: r.Header.Get("Accept"), auth: r.Header.Get("Authorization")})
		handler(w, r)
	}
	server := httptest.NewServer(http.HandlerFunc(full))
	t.Cleanup(server.Close)
	return &fakeGitHub{server: server, client: server.Client(), mu: rec}
}

// recorded 取记录（drain 的语义化包装）。
func (f *fakeGitHub) recorded(t *testing.T) []recordedRequest { return f.mu.drain(t) }

// releasesBody 组装列表端点响应体。
func releasesBody(t *testing.T, releases []Release) []byte {
	t.Helper()
	data, err := json.Marshal(releases)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLatestReleaseUsesListEndpointAndSkipsDraft(t *testing.T) {
	// 评审 P0 断言：查询命中 /releases?per_page=10 列表端点（勿按 latest 端点造假），
	// 且 draft 在前、prerelease 在次（三审 P3 用例）时取 prerelease 首条
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "per_page=10") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(releasesBody(t, []Release{
			{Tag: "v9.9.9-draft", Draft: true},
			{Tag: "v0.5.0-m3c1", Prerelease: true, Assets: []Asset{{ID: 7, Name: "tradewind-v0.5.0-m3c1-darwin-arm64.tar.gz", Size: 1234}}},
		}))
	})
	client := &Client{APIBase: fake.server.URL, Token: "test-token-value", HTTP: fake.client}
	rel, err := client.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Tag != "v0.5.0-m3c1" {
		t.Fatalf("tag = %q, want prerelease first entry (draft filtered)", rel.Tag)
	}
	if rel.Prerelease != true {
		t.Fatal("prerelease release must not be filtered")
	}
	reqs := fake.recorded(t)
	if len(reqs) == 0 || !strings.Contains(reqs[0].path, "/repos/chaojimaimi/tradewind/releases?per_page=10") {
		t.Fatalf("requests = %+v, want list endpoint", reqs)
	}
	if reqs[0].auth != "Bearer test-token-value" {
		t.Fatalf("auth header = %q, want Bearer", reqs[0].auth)
	}
}

func TestLatestReleasePrereleaseDirectlyFirst(t *testing.T) {
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(releasesBody(t, []Release{{Tag: "v0.5.0-m3c1", Prerelease: true}}))
	})
	client := &Client{APIBase: fake.server.URL, HTTP: fake.client}
	rel, err := client.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Tag != "v0.5.0-m3c1" {
		t.Fatalf("tag = %q", rel.Tag)
	}
}

func TestLatestReleaseEmptyListIsError(t *testing.T) {
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(releasesBody(t, []Release{}))
	})
	client := &Client{APIBase: fake.server.URL, HTTP: fake.client}
	if _, err := client.LatestRelease(context.Background()); err == nil {
		t.Fatal("empty release list: want error")
	}
}

func TestRateLimitErrorMentionsResetTime(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusForbidden)
	})
	client := &Client{APIBase: fake.server.URL, HTTP: fake.client}
	_, err := client.LatestRelease(context.Background())
	if err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("err = %v, want explicit rate limit error", err)
	}
	if !strings.Contains(err.Error(), "重置") {
		t.Fatalf("err = %v, want reset time hint", err)
	}
}

func TestUnauthorizedErrorNeverEchoesToken(t *testing.T) {
	// 红线：token 缺失或无效类错误绝不回显内容
	const token = "super-secret-token-value-abc"
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	client := &Client{APIBase: fake.server.URL, Token: token, HTTP: fake.client}
	_, err := client.LatestRelease(context.Background())
	if err == nil {
		t.Fatal("401: want error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "token 缺失或无效") {
		t.Fatalf("err = %v, want blind wording", err)
	}
}

func TestDownloadAssetUsesOctetStreamEndpointOnly(t *testing.T) {
	// 三审 P3 否定式断言：下载请求命中 /releases/assets/<id>（octet-stream），
	// 绝不产生 browser_download_url 形态的请求路径
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/chaojimaimi/tradewind/releases" && strings.Contains(r.URL.RawQuery, "per_page=10"):
			_, _ = w.Write(releasesBody(t, []Release{{Tag: "v9.9.9", Assets: []Asset{{ID: 42, Name: "tradewind-v9.9.9-darwin-arm64.tar.gz", Size: 5}}}}))
		case r.URL.Path == "/repos/chaojimaimi/tradewind/releases/assets/42":
			if r.Header.Get("Accept") != "application/octet-stream" {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			_, _ = w.Write([]byte("bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client := &Client{APIBase: fake.server.URL, Token: "test-token-value", HTTP: fake.client}
	rel, err := client.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	asset, ok := rel.AssetByName("tradewind-v9.9.9-darwin-arm64.tar.gz")
	if !ok {
		t.Fatal("asset not found")
	}
	var buf strings.Builder
	if _, err := client.DownloadAsset(context.Background(), asset, &buf); err != nil {
		t.Fatalf("DownloadAsset: %v", err)
	}
	if buf.String() != "bytes" {
		t.Fatalf("downloaded = %q", buf.String())
	}
	reqs := fake.recorded(t)
	for _, req := range reqs {
		if strings.Contains(req.path, "/releases/download/") || strings.Contains(req.path, "browser_download") {
			t.Fatalf("browser_download_url channel used: %q", req.path)
		}
	}
}

func TestEgressRetryAfterTransportFailure(t *testing.T) {
	// 自举重试：直连传输层失败一次 → 经 Egress 出口重试成功
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(releasesBody(t, []Release{{Tag: "v0.5.0-m3c1"}}))
	})
	direct := &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused (simulated)")
		}),
	}
	client := &Client{
		APIBase: fake.server.URL,
		HTTP:    direct,
		Egress:  func() *http.Client { return fake.client },
	}
	rel, err := client.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease with egress retry: %v", err)
	}
	if rel.Tag != "v0.5.0-m3c1" {
		t.Fatalf("tag = %q", rel.Tag)
	}
}

func TestEgressNilFailsAfterDirectFailure(t *testing.T) {
	fake := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server must not be reached when direct transport always fails")
	})
	direct := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused (simulated)")
		}),
	}
	client := &Client{APIBase: fake.server.URL, HTTP: direct, Egress: func() *http.Client { return nil }}
	if _, err := client.LatestRelease(context.Background()); err == nil {
		t.Fatal("want error when both egress paths unavailable")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// --- ReadToken（600 强制 + 引导文案 + 不回显内容）---

func TestReadTokenRequiresFileWith600(t *testing.T) {
	home := paths.Home(t.TempDir())
	if _, err := ReadToken(home); err == nil || !strings.Contains(err.Error(), "release_token") {
		t.Fatalf("missing file err = %v, want guidance mentioning release_token", err)
	}
	path := filepath.Join(home.String(), "release_token")
	if err := os.WriteFile(path, []byte("ghs_fake_test_token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadToken(home); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("wide permission err = %v, want chmod guidance", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := ReadToken(home)
	if err != nil || token != "ghs_fake_test_token" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
}

func TestReadTokenEmptyFileIsBlindError(t *testing.T) {
	const token = "ghs_fake_empty_file_probe"
	home := paths.Home(t.TempDir())
	path := filepath.Join(home.String(), "release_token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadToken(home)
	if err == nil || !strings.Contains(err.Error(), "token 缺失或无效") {
		t.Fatalf("err = %v, want blind wording", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks token literal: %q", err.Error())
	}
}
