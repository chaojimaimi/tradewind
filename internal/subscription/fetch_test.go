package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchSendsClashUAAndParsesUserinfo(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		// 机场标准格式（实测本机订阅响应头）
		w.Header().Set("Subscription-Userinfo",
			"upload=226100036; download=3525157109; total=107374182400; expire=1877644800")
		w.Write([]byte("proxies: []\n"))
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// UA 必须是 clash 兼容（陌生 UA 会被机场回退成 base64 节点列表）
	if !strings.HasPrefix(gotUA, "clash.meta/") {
		t.Fatalf("UA = %q, want clash.meta prefix", gotUA)
	}
	if string(res.Body) != "proxies: []\n" {
		t.Fatalf("body = %q", string(res.Body))
	}
	if !res.HasExtra {
		t.Fatal("HasExtra = false, want true")
	}
	if res.Extra.Upload != 226100036 || res.Extra.Download != 3525157109 ||
		res.Extra.Total != 107374182400 || res.Extra.Expire != 1877644800 {
		t.Fatalf("extra = %+v", res.Extra)
	}
}

func TestFetchNon200Fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token expired", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want status 401 error", err)
	}
}

func TestFetchNoUserinfoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("proxies: []\n"))
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.HasExtra || res.Extra != (Extra{}) {
		t.Fatalf("extra = %+v hasExtra=%v, want zero/false", res.Extra, res.HasExtra)
	}
}

func TestParseUserinfoEdgeCases(t *testing.T) {
	cases := []struct {
		raw     string
		wantOK  bool
		version Extra
	}{
		{"upload=1; download=2; total=3; expire=4", true, Extra{1, 2, 3, 4}},
		{"UPLOAD=1; Download=2", true, Extra{Upload: 1, Download: 2}},
		{"upload=abc; download=2", true, Extra{Download: 2}},
		{"nothing", false, Extra{}},
		{"", false, Extra{}},
	}
	for _, c := range cases {
		got, ok := ParseUserinfo(c.raw)
		if ok != c.wantOK || got != c.version {
			t.Fatalf("ParseUserinfo(%q) = %+v,%v want %+v,%v", c.raw, got, ok, c.version, c.wantOK)
		}
	}
}

func TestFetchErrorMasksQueryToken(t *testing.T) {
	// 订阅 URL 内嵌 token，错误信息必须脱敏（DESIGN §2.7.3）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.Client(), srv.URL+"?token=SECRETVALUE")
	if err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("err = %v, must not leak token", err)
	}
}
