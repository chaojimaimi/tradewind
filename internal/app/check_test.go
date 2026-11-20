package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/release"
	"github.com/chaojimaimi/tradewind/internal/version"
)

// pointCheckAtFake 把 release 客户端与版本号指到测试态（httptest 假 API，不出网）；
// 清理由 t.Cleanup 完成。
func pointCheckAtFake(t *testing.T, ver string, server *httptest.Server) {
	t.Helper()
	oldVersion := version.Version
	version.Version = ver
	newReleaseClient = func(token string, egress func() *http.Client) *release.Client {
		c := release.NewClient(token, egress)
		c.APIBase = server.URL
		c.HTTP = server.Client()
		return c
	}
	t.Cleanup(func() {
		version.Version = oldVersion
		newReleaseClient = productionReleaseClient
	})
}

// checkFakeAPI 是 version --check 的列表端点假 API（draft 之后唯一发布条目）。
func checkFakeAPI(t *testing.T, tag string, assetSize int64, withAsset bool) *httptest.Server {
	t.Helper()
	asset := ""
	if withAsset {
		size, _ := json.Marshal(assetSize)
		asset = `{"id":42,"name":"tradewind-` + tag + `-darwin-arm64.tar.gz","size":` + string(size) + `}`
	}
	body := `[{"tag_name":"v9.9.9-draft","draft":true},{"tag_name":"` + tag + `","draft":false,"prerelease":true,"assets":[` + asset + `]}]`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "per_page=10") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

// writeTokenFile 在临时数据目录写入 600 token 文件。
func writeTokenFile(t *testing.T, home paths.Home, content string) {
	t.Helper()
	path := filepath.Join(home.String(), "release_token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckUpdateDevBuildIsExplicitError(t *testing.T) {
	server := checkFakeAPI(t, "v9.9.9", 1, true)
	pointCheckAtFake(t, "dev", server)
	home := paths.Home(t.TempDir())
	_, err := CheckUpdate(context.Background(), home, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "开发构建无法比对") {
		t.Fatalf("err = %v, want explicit dev-build error", err)
	}
}

func TestCheckUpdateTokenMissingMentionsGuidance(t *testing.T) {
	server := checkFakeAPI(t, "v9.9.9", 1, true)
	pointCheckAtFake(t, "v0.5.0-m3b5", server)
	const secret = "ghs_probe_token_do_not_echo"
	home := paths.Home(t.TempDir())
	_, err := CheckUpdate(context.Background(), home, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "release_token") {
		t.Fatalf("err = %v, want release_token guidance", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error must never echo token content")
	}
	// token 就位后走到列表查询（假 API 返回正常 release，应成功）
	writeTokenFile(t, home, "ghs_probe_ok_token")
	res, err := CheckUpdate(context.Background(), home, &strings.Builder{})
	if err != nil || res.Latest != "v9.9.9" {
		t.Fatalf("res = %+v, err = %v; want fake API reached", res, err)
	}
}

func TestCheckUpdateAvailableAndEqual(t *testing.T) {
	server := checkFakeAPI(t, "v0.5.0-m3c1", 27848296, true)
	pointCheckAtFake(t, "v0.5.0-m3b5", server)
	home := paths.Home(t.TempDir())
	writeTokenFile(t, home, "ghs_probe_ok_token")
	res, err := CheckUpdate(context.Background(), home, &strings.Builder{})
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if !res.UpdateAvailable || res.Latest != "v0.5.0-m3c1" || res.Current != "v0.5.0-m3b5" {
		t.Fatalf("result = %+v", res)
	}
	if res.Asset != "tradewind-v0.5.0-m3c1-darwin-arm64.tar.gz" || res.AssetSize != 27848296 {
		t.Fatalf("asset fields = %+v", res)
	}

	// 相等 → 无更新（显式相等性，不猜 semver 排序）
	pointCheckAtFake(t, "v0.5.0-m3c1", server)
	res, err = CheckUpdate(context.Background(), home, &strings.Builder{})
	if err != nil {
		t.Fatalf("CheckUpdate equal: %v", err)
	}
	if res.UpdateAvailable {
		t.Fatalf("equal tags must not be update-available: %+v", res)
	}
}

func TestCheckUpdateMissingAssetAborts(t *testing.T) {
	server := checkFakeAPI(t, "v0.5.0-m3c1", 0, false)
	pointCheckAtFake(t, "v0.5.0-m3b5", server)
	home := paths.Home(t.TempDir())
	writeTokenFile(t, home, "ghs_probe_ok_token")
	_, err := CheckUpdate(context.Background(), home, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "缺资产") {
		t.Fatalf("err = %v, want missing-asset abort", err)
	}
}

func TestCheckUpdateJSONLineErrorAndSuccess(t *testing.T) {
	const secret = "ghs_probe_json_contract_token"
	// 失败 → {"error":...} 单行（恒退出 0 契约）
	server := checkFakeAPI(t, "v9.9.9", 1, true)
	pointCheckAtFake(t, "dev", server)
	home := paths.Home(t.TempDir())
	line := CheckUpdateJSONLine(context.Background(), home)
	if !strings.HasPrefix(line, `{"error":`) || strings.ContainsAny(line, "\n") {
		t.Fatalf("error line = %q, want single-line error object", line)
	}
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &decoded); err != nil || decoded.Error == "" {
		t.Fatalf("decode = %q, err = %v", line, err)
	}

	// 成功 → 结果单行 JSON
	server2 := checkFakeAPI(t, "v0.5.0-m3c1", 27848296, true)
	pointCheckAtFake(t, "v0.5.0-m3b5", server2)
	writeTokenFile(t, home, "ghs_probe_ok_token")
	line = CheckUpdateJSONLine(context.Background(), home)
	var res checkUpdateJSON
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("decode success line: %v (%q)", err, line)
	}
	if !res.UpdateAvailable || res.AssetSize != 27848296 || res.Latest != "v0.5.0-m3c1" {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(line, secret) {
		t.Fatal("output must never contain token")
	}
}
