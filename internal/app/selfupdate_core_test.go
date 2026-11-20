package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/release"
)

const (
	upstreamTag    = "v1.19.99"
	upstreamOldTag = "v1.19.34"
)

// coreFixture 是 --core 假编排：上游假 API（gzip 资产 + digest）、bin/mihomo 假脚本、
// 记录式缝隙。绝不触碰真实内核与 launchd。
type coreFixture struct {
	f          *selfUpdateFixture // 复用记录式缝隙（stop/kickstart/spawn/plist/home）
	coreBin    string             // home 内 bin/mihomo 绝对路径
	coreScript string             // 新内核假脚本（-v 输出含 upstreamTag）
	digestOK   bool
	withDigest bool
	gzTag      string // 资产内假脚本的版本（候选回读用）
}

func newCoreFixture(t *testing.T) *coreFixture {
	t.Helper()
	f := &coreFixture{f: newSelfUpdateFixture(t), digestOK: true, withDigest: true, gzTag: upstreamTag}
	// bin/mihomo 假内核 + 旧 sidecar（真实 home 内）
	binDir := filepath.Join(f.f.home.String(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.coreBin = filepath.Join(binDir, "mihomo")
	f.coreScript = fakeCoreScript(upstreamTag)
	if err := os.WriteFile(f.coreBin, []byte(fakeCoreScript(upstreamOldTag)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.f.home.Sidecar(), []byte("aaaa  "+f.coreBin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 上游假 API（gzip 资产 + digest；gzTag 动态读取——测试体内可改写候选版本）
	f.f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz := fakeCoreGzBytes(t, f.gzTag)
		sum := sha256Hex(gz)
		f.f.recordRequest(r.URL.Path, r.Header.Get("Accept"))
		switch {
		case r.URL.Path == "/repos/MetaCubeX/mihomo/releases":
			digest := ""
			if f.withDigest {
				digest = `,"digest":"sha256:` + sum + `"`
				if !f.digestOK {
					digest = `,"digest":"sha256:` + strings.Repeat("0", 64) + `"`
				}
			}
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"tag_name":%q,"draft":false,"prerelease":false,"assets":[{"id":9,"name":"mihomo-darwin-arm64-%s.gz","size":%d%s}]}]`,
				upstreamTag, upstreamTag, len(gz), digest)))
		case r.URL.Path == "/repos/MetaCubeX/mihomo/releases/assets/9":
			_, _ = w.Write(gz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.f.server.Close)
	return f
}

func fakeCoreScript(tag string) string {
	return "#!/bin/sh\necho \"Mihomo Meta " + tag + " darwin arm64 with go1.26\"\n"
}

// fakeCoreVersionLine 是假内核执行 -v 的首行输出（F7 实测形态）。
func fakeCoreVersionLine(tag string) string {
	return "Mihomo Meta " + tag + " darwin arm64 with go1.26"
}

// fakeCoreGzBytes gzip 打包假内核脚本（上游资产形态=单文件 gzip）。
func fakeCoreGzBytes(t *testing.T, tag string) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	if _, err := gz.Write([]byte(fakeCoreScript(tag))); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// env 组装 --core 记录式编排环境（运行探针/二进制回读按文件内容区分新旧）。
func (c *coreFixture) env() selfUpdateEnv {
	env := c.f.env()
	env.newUpstreamClient = func() *release.Client { return c.upstreamClient() }
	env.coreVersionProbe = func(paths.Home) (string, bool) {
		data, err := os.ReadFile(c.coreBin)
		if err != nil {
			return "", false
		}
		if strings.Contains(string(data), upstreamTag) {
			return fakeCoreVersionLine(upstreamTag), true
		}
		return fakeCoreVersionLine(upstreamOldTag), true
	}
	// -v 回读按文件内容返回对应版本的 Mihomo 首行；payload "version" 探针沿用主体形态
	env.runBinaryVersion = func(binPath string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "-v" {
			data, err := os.ReadFile(binPath)
			if err != nil {
				return "", err
			}
			if strings.Contains(string(data), upstreamTag) {
				return fakeCoreVersionLine(upstreamTag), nil
			}
			return fakeCoreVersionLine(upstreamOldTag), nil
		}
		return c.f.env().runBinaryVersion(binPath, args...)
	}
	return env
}

// upstreamClient 构造指向假 API 的上游客户端（公开仓免鉴权）。
func (c *coreFixture) upstreamClient() *release.Client {
	client := release.NewClient("", nil)
	client.APIBase = c.f.server.URL
	client.HTTP = c.f.server.Client()
	return client
}

func TestSelfUpdateCoreSwapsSidecarAndRestarts(t *testing.T) {
	c := newCoreFixture(t)
	env := c.env()
	env.reachable = func(paths.Home) bool { return true }
	env.autostartOn = func() bool { return true }
	tag, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err != nil {
		t.Fatalf("SelfUpdateCore: %v", err)
	}
	if tag != upstreamTag {
		t.Fatalf("tag = %q", tag)
	}
	if got := strings.Join(c.f.log, ","); got != "stop,kickstart" {
		t.Fatalf("call log = %q, want stop,kickstart", got)
	}
	// 新二进制落位 + sidecar 按 F6 格式重写且可被 VerifyBinary 消费
	data, err := os.ReadFile(c.coreBin)
	if err != nil || string(data) != c.coreScript {
		t.Fatalf("core bin = %q, err = %v", data, err)
	}
	if err := core.VerifyBinary(c.f.home.Bin(), c.f.home.Sidecar()); err != nil {
		t.Fatalf("VerifyBinary with rewritten sidecar: %v", err)
	}
	sidecar, err := os.ReadFile(c.f.home.Sidecar())
	if err != nil || !strings.HasPrefix(string(sidecar), sha256Hex([]byte(c.coreScript))+"  "+c.f.home.Bin()) {
		t.Fatalf("sidecar = %q, want <hex>  <abs path> format", sidecar)
	}
}

func TestSelfUpdateCoreNotRunningOnlySwaps(t *testing.T) {
	c := newCoreFixture(t)
	env := c.env()
	env.reachable = func(paths.Home) bool { return false }
	tag, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err != nil {
		t.Fatalf("SelfUpdateCore: %v", err)
	}
	if tag != upstreamTag {
		t.Fatalf("tag = %q", tag)
	}
	if len(c.f.log) != 0 {
		t.Fatalf("no stop/restart expected: %v", c.f.log)
	}
	if err := core.VerifyBinary(c.f.home.Bin(), c.f.home.Sidecar()); err != nil {
		t.Fatalf("VerifyBinary: %v", err)
	}
}

func TestSelfUpdateCoreDigestMismatchAborts(t *testing.T) {
	c := newCoreFixture(t)
	c.digestOK = false
	env := c.env()
	before, err := os.ReadFile(c.coreBin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = SelfUpdateCore(context.Background(), c.f.home, env)
	if err == nil || !strings.Contains(err.Error(), "digest 不符") {
		t.Fatalf("err = %v, want digest mismatch abort", err)
	}
	after, err := os.ReadFile(c.coreBin)
	if err != nil || string(after) != string(before) {
		t.Fatal("abort must not touch bin/mihomo")
	}
}

func TestSelfUpdateCoreMissingDigestDegradesToTLS(t *testing.T) {
	c := newCoreFixture(t)
	c.withDigest = false
	env := c.env()
	tag, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err != nil || tag != upstreamTag {
		t.Fatalf("tag = %q, err = %v; want digest-missing degrade path", tag, err)
	}
}

func TestSelfUpdateCoreCandidateVersionMismatchAborts(t *testing.T) {
	// 候选 -v 回读不含目标 tag → 中止不动盘
	c := newCoreFixture(t)
	c.gzTag = upstreamOldTag // 资产内容是旧版本
	env := c.env()
	_, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err == nil || !strings.Contains(err.Error(), "中止不动盘") {
		t.Fatalf("err = %v, want candidate readout abort", err)
	}
}

func TestSelfUpdateCoreAlreadyLatestViaProbe(t *testing.T) {
	c := newCoreFixture(t)
	// 运行中且已是最新：bin 内容即新版（探针据此报 tag）
	if err := os.WriteFile(c.coreBin, []byte(fakeCoreScript(upstreamTag)), 0o755); err != nil {
		t.Fatal(err)
	}
	env := c.env()
	env.reachable = func(paths.Home) bool { return true }
	tag, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err != nil || tag != upstreamTag {
		t.Fatalf("tag = %q, err = %v", tag, err)
	}
	for _, path := range c.f.requests {
		if strings.Contains(path, "assets/") {
			t.Fatalf("no download expected: %q", path)
		}
	}
}

func TestSelfUpdateCoreAlreadyLatestViaLocalReadout(t *testing.T) {
	// 未运行场景（评审 P3）：bin/mihomo -v 本地回读含 tag → 已是最新
	c := newCoreFixture(t)
	// 未运行场景（评审 P3）：bin/mihomo -v 本地回读含 tag → 已是最新
	if err := os.WriteFile(c.coreBin, []byte(fakeCoreScript(upstreamTag)), 0o755); err != nil {
		t.Fatal(err)
	}
	env := c.env()
	env.reachable = func(paths.Home) bool { return false }
	tag, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err != nil || tag != upstreamTag {
		t.Fatalf("tag = %q, err = %v", tag, err)
	}
	for _, path := range c.f.requests {
		if strings.Contains(path, "assets/") {
			t.Fatalf("no download expected: %q", path)
		}
	}
}

func TestSelfUpdateCoreHandshakeFailureRollsBack(t *testing.T) {
	// 握手失败 → 自动回滚：.prev/.prev.sha256 恢复 → 再拉回 → 报错含回读版本
	c := newCoreFixture(t)
	env := c.env()
	env.reachable = func(paths.Home) bool { return true }
	env.waitReady = func(paths.Home, string, time.Duration) bool { return false }
	_, err := SelfUpdateCore(context.Background(), c.f.home, env)
	if err == nil || !strings.Contains(err.Error(), "已回滚到 "+upstreamOldTag) {
		t.Fatalf("err = %v, want rollback to readout version", err)
	}
	// bin/sidecar 恢复为旧内容；.prev/.prev.sha256 消失（rename 回位）
	data, err := os.ReadFile(c.coreBin)
	if err != nil || string(data) != fakeCoreScript(upstreamOldTag) {
		t.Fatalf("core bin not restored: %q, err = %v", data, err)
	}
	if _, err := os.Stat(c.coreBin + ".prev"); !os.IsNotExist(err) {
		t.Fatalf(".prev must be consumed by rollback: %v", err)
	}
	// 拉回两次（首次 + 回滚后）
	if c.f.kickstarted != 2 {
		t.Fatalf("kickstart count = %d, want 2 (initial + rollback)", c.f.kickstarted)
	}
	if err := core.VerifyBinary(c.f.home.Bin(), c.f.home.Sidecar()); err == nil {
		t.Fatal("restored sidecar content is the old placeholder; mismatch expected against fake binary")
	}
}

func TestSelfUpdateCoreExtractCoreVersion(t *testing.T) {
	// F7 实测形态：首行 "Mihomo Meta v1.19.34 darwin arm64 with go1.26.11 …"
	got := extractCoreVersion("Mihomo Meta v1.19.34 darwin arm64 with go1.26.11\nAnother line")
	if got != "v1.19.34" {
		t.Fatalf("extractCoreVersion = %q", got)
	}
	if got := extractCoreVersion("no version here"); got != "no version here" {
		t.Fatalf("fallback = %q", got)
	}
}
