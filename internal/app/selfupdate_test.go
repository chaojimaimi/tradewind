package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/release"
	"github.com/chaojimaimi/tradewind/internal/version"
)

const (
	fakeOldTag = "v9.9.8-old"
	fakeNewTag = "v9.9.9-test"
	// fakeTokenValue 是测试专用假 token（红线测试断言输出无此子串）。
	fakeTokenValue = "ghs_fake_selfupdate_token"
)

// fakeAssetName/sidecar 是测试资产命名（与 CLI 命名契约一致）；资产本体是含
// tradewind 脚本条目的真实 tar.gz（解包链全真，见 fakeTarballBytes）。
func fakeAssetName(tag string) string   { return AssetNameFor(tag) }
func fakeSidecarName(tag string) string { return SidecarNameFor(tag) }

func fakePayloadScript(tag string) string {
	return "#!/bin/sh\necho \"tradewind " + tag + "\"\n"
}

// fakeTarballBytes 组装含 ./tradewind 脚本条目的 tar.gz 字节（m3b5 形态 ./ 前缀）。
func fakeTarballBytes(t *testing.T, tag string) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	script := fakePayloadScript(tag)
	hdr := &tar.Header{Name: "./tradewind", Mode: 0o755, Size: int64(len(script))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

// selfUpdateFixture 汇集假编排环境：假 API（tarball+sidecar）、临时 plist、
// 记录式缝隙。请求记录供否定式断言（octet-stream 通道，绝不走 browser_download_url）。
type selfUpdateFixture struct {
	home        paths.Home
	launchDir   string
	oldBin      string
	plistBin    string
	server      *httptest.Server
	mu          sync.Mutex
	requests    []string
	accepts     []string
	log         []string
	stopped     bool
	kickstarted int
	spawned     []string
	waitOK      bool
	fixtureTag  string
	payloadOK   bool // true=payload 回读版本正确；false=错误版本
	sidecarOK   bool
	withSidecar bool
}

func (f *selfUpdateFixture) recordRequest(path, accept string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, path)
	f.accepts = append(f.accepts, accept)
}

func (f *selfUpdateFixture) recordLog(step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, step)
}

func (f *selfUpdateFixture) wasRequested(path string) bool {
	for _, p := range f.requests {
		if p == path {
			return true
		}
	}
	return false
}

// newSelfUpdateFixture 组装全部测试态（httptest 由 t.Cleanup 收尾）。
func newSelfUpdateFixture(t *testing.T) *selfUpdateFixture {
	t.Helper()
	f := &selfUpdateFixture{
		home:        paths.Home(t.TempDir()),
		fixtureTag:  fakeNewTag,
		payloadOK:   true,
		sidecarOK:   true,
		withSidecar: true,
		waitOK:      true,
	}
	f.launchDir = t.TempDir()
	// 旧二进制（两处不同路径：调用者 + plist ProgramArguments[0]）
	f.oldBin = filepath.Join(t.TempDir(), "tradewind-old")
	if err := os.WriteFile(f.oldBin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.plistBin = filepath.Join(t.TempDir(), "tradewind-agent")
	if err := os.WriteFile(f.plistBin, []byte("old-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPathFor(f.launchDir), []byte(plistWithProgram(f.plistBin)), 0o644); err != nil {
		t.Fatal(err)
	}
	// 假 API：列表 + octet-stream 资产端点（资产本体=真实 tar.gz，解包链全真）
	tarball := fakeTarballBytes(t, fakeNewTag)
	sum := sha256.Sum256(tarball)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.recordRequest(r.URL.Path, r.Header.Get("Accept"))
		switch {
		case r.URL.Path == "/repos/chaojimaimi/tradewind/releases":
			assets := fmt.Sprintf(`{"id":1,"name":%q,"size":%d}`, fakeAssetName(fakeNewTag), len(tarball))
			if f.withSidecar {
				assets += fmt.Sprintf(`,{"id":2,"name":%q,"size":75}`, fakeSidecarName(fakeNewTag))
			}
			body := fmt.Sprintf(`[{"tag_name":%q,"draft":false,"prerelease":true,"assets":[%s]}]`, f.fixtureTag, assets)
			_, _ = w.Write([]byte(body))
		case r.URL.Path == "/repos/chaojimaimi/tradewind/releases/assets/1":
			if r.Header.Get("Accept") != "application/octet-stream" {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			_, _ = w.Write(tarball)
		case r.URL.Path == "/repos/chaojimaimi/tradewind/releases/assets/2":
			sumHex := hex.EncodeToString(sum[:])
			if !f.sidecarOK {
				sumHex = strings.Repeat("0", 64)
			}
			_, _ = w.Write([]byte(sumHex + "  " + fakeAssetName(fakeNewTag) + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	// version 注入
	old := version.Version
	version.Version = fakeOldTag
	t.Cleanup(func() { version.Version = old })
	// token 文件（600）
	tokenPath := filepath.Join(f.home.String(), "release_token")
	if err := os.WriteFile(tokenPath, []byte(fakeTokenValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// env 组装记录式编排环境（stop/kickstart/spawn/wait 全部缝隙化；默认内核未运行）。
func (f *selfUpdateFixture) env() selfUpdateEnv {
	return selfUpdateEnv{
		newRepoClient: func(token string) *release.Client {
			c := release.NewClient(token, nil)
			c.APIBase = f.server.URL
			c.HTTP = f.server.Client()
			return c
		},
		executable: func() (string, error) { return f.oldBin, nil },
		launchDir:  func() (string, error) { return f.launchDir, nil },
		autostartOn: func() bool {
			_, err := os.Stat(plistPathFor(f.launchDir))
			return err == nil
		},
		reachable: func(paths.Home) bool { return false },
		stopCore: func(paths.Home) error {
			f.recordLog("stop")
			f.stopped = true
			return nil
		},
		waitReady: func(paths.Home, string, time.Duration) bool { return f.waitOK },
		kickstart: func() error {
			f.recordLog("kickstart")
			f.kickstarted++
			return nil
		},
		spawnStart: func(exe string) error {
			f.recordLog("spawn")
			f.spawned = append(f.spawned, exe)
			return nil
		},
		runBinaryVersion: func(binPath string, args ...string) (string, error) {
			want := fakeNewTag
			if !f.payloadOK {
				want = fakeOldTag
			}
			return fmt.Sprintf("tradewind %s\ncore not reachable\n", want), nil
		},
		progress: &strings.Builder{},
	}
}

// plistWithProgram 生成仅含 ProgramArguments 的最小 plist（与 launchdPlist 同构）。
func plistWithProgram(exe string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>io.github.chaojimaimi.tradewind.agent</string>
<key>ProgramArguments</key><array><string>` + exe + `</string><string>start</string></array>
</dict></plist>`
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

func TestSelfUpdateSwapsInodesStopsFirstAndKickstarts(t *testing.T) {
	f := newSelfUpdateFixture(t)
	env := f.env()
	env.reachable = func(paths.Home) bool { return true } // 内核在跑
	oldCaller := inodeOf(t, f.oldBin)
	oldAgent := inodeOf(t, f.plistBin)
	tag, err := SelfUpdate(context.Background(), f.home, env)
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if tag != fakeNewTag {
		t.Fatalf("tag = %q", tag)
	}
	// 编排序断言：stop 先于 swap，kickstart 收尾
	if got := strings.Join(f.log, ","); got != "stop,kickstart" {
		t.Fatalf("call log = %q, want stop,kickstart", got)
	}
	// inode 断言：两处目标均换新 inode（rm+cp 铁律），内容为 tarball 内 payload 字节
	if inodeOf(t, f.oldBin) == oldCaller {
		t.Fatal("caller path inode unchanged (cp in-place?)")
	}
	if inodeOf(t, f.plistBin) == oldAgent {
		t.Fatal("agent path inode unchanged")
	}
	for _, target := range []string{f.oldBin, f.plistBin} {
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != fakePayloadScript(fakeNewTag) {
			t.Fatalf("target %s content = %q", target, data)
		}
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("target %s perm = %v, err = %v", target, info.Mode().Perm(), err)
		}
	}
	// 否定式断言（三审 P3）：下载命中 assets/<id> octet-stream，绝无 browser_download_url 形态
	for i, path := range f.requests {
		if strings.Contains(path, "/releases/download/") || strings.Contains(path, "browser_download") {
			t.Fatalf("browser_download_url channel used: %q", path)
		}
		if strings.HasPrefix(path, "/repos/chaojimaimi/tradewind/releases/assets/") && f.accepts[i] != "application/octet-stream" {
			t.Fatalf("asset request %q accept = %q", path, f.accepts[i])
		}
	}
	if !f.wasRequested("/repos/chaojimaimi/tradewind/releases/assets/1") ||
		!f.wasRequested("/repos/chaojimaimi/tradewind/releases/assets/2") {
		t.Fatalf("asset endpoints not hit: %v", f.requests)
	}
}

func TestSelfUpdateAutostartOffDetachedStartBranch(t *testing.T) {
	// 手动 start 场景（F8）：无 LaunchAgent plist → 目标只有调用者路径 + detached spawn
	f := newSelfUpdateFixture(t)
	if err := os.Remove(plistPathFor(f.launchDir)); err != nil {
		t.Fatal(err)
	}
	env := f.env()
	env.reachable = func(paths.Home) bool { return true }
	if _, err := SelfUpdate(context.Background(), f.home, env); err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if got := strings.Join(f.log, ","); got != "stop,spawn" {
		t.Fatalf("call log = %q, want stop,spawn", got)
	}
	if len(f.spawned) != 1 || f.spawned[0] != f.oldBin {
		t.Fatalf("spawned = %v, want new caller path %s", f.spawned, f.oldBin)
	}
	if f.kickstarted != 0 {
		t.Fatal("kickstart must not run without agent")
	}
}

func TestSelfUpdateMainNotRunningOnlySwaps(t *testing.T) {
	f := newSelfUpdateFixture(t)
	env := f.env()
	tag, err := SelfUpdate(context.Background(), f.home, env)
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if tag != fakeNewTag {
		t.Fatalf("tag = %q", tag)
	}
	if len(f.log) != 0 {
		t.Fatalf("no stop/restart expected: %v", f.log)
	}
	data, err := os.ReadFile(f.oldBin)
	if err != nil || string(data) != fakePayloadScript(fakeNewTag) {
		t.Fatalf("caller path not replaced: %v", err)
	}
	progress := env.progress.(*strings.Builder)
	if !strings.Contains(progress.String(), "下次启动生效") {
		t.Fatalf("progress = %q, want next-start hint", progress.String())
	}
}

func TestSelfUpdateSidecarMismatchAbortsBeforeSwap(t *testing.T) {
	f := newSelfUpdateFixture(t)
	f.sidecarOK = false
	env := f.env()
	before := inodeOf(t, f.oldBin)
	if _, err := SelfUpdate(context.Background(), f.home, env); err == nil {
		t.Fatal("sidecar mismatch: want abort")
	}
	if inodeOf(t, f.oldBin) != before {
		t.Fatal("abort must not touch disk targets")
	}
}

func TestSelfUpdateMissingSidecarAborts(t *testing.T) {
	f := newSelfUpdateFixture(t)
	f.withSidecar = false
	env := f.env()
	_, err := SelfUpdate(context.Background(), f.home, env)
	if err == nil || !strings.Contains(err.Error(), "缺校验文件") {
		t.Fatalf("err = %v, want missing-sidecar abort", err)
	}
}

func TestSelfUpdatePayloadVersionMismatchAborts(t *testing.T) {
	// 三审 P3：version 输出多行，只比对首行版本行；不符中止
	f := newSelfUpdateFixture(t)
	f.payloadOK = false
	env := f.env()
	_, err := SelfUpdate(context.Background(), f.home, env)
	if err == nil || !strings.Contains(err.Error(), "中止不动盘") {
		t.Fatalf("err = %v, want payload version abort", err)
	}
}

func TestSelfUpdateAlreadyLatestShortCircuits(t *testing.T) {
	f := newSelfUpdateFixture(t)
	env := f.env()
	old := version.Version
	version.Version = fakeNewTag
	defer func() { version.Version = old }()
	tag, err := SelfUpdate(context.Background(), f.home, env)
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if tag != fakeNewTag {
		t.Fatalf("tag = %q", tag)
	}
	for _, path := range f.requests {
		if strings.Contains(path, "assets/") {
			t.Fatalf("no asset download expected: %q", path)
		}
	}
}

func TestSelfUpdateCorruptPlistAbortsBeforeDownloads(t *testing.T) {
	f := newSelfUpdateFixture(t)
	if err := os.WriteFile(plistPathFor(f.launchDir), []byte("not a plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := f.env()
	_, err := SelfUpdate(context.Background(), f.home, env)
	if err == nil || !strings.Contains(err.Error(), "plist") {
		t.Fatalf("err = %v, want plist parse abort", err)
	}
	for _, path := range f.requests {
		if strings.Contains(path, "/assets/") {
			t.Fatalf("plist failure must abort before downloads: %v", f.requests)
		}
	}
}

func TestSelfUpdateSameTargetsSwapOnce(t *testing.T) {
	// plist ProgramArguments[0] == 调用者路径 → 只换一次（无重复替换错误）
	f := newSelfUpdateFixture(t)
	if err := os.WriteFile(plistPathFor(f.launchDir), []byte(plistWithProgram(f.oldBin)), 0o644); err != nil {
		t.Fatal(err)
	}
	env := f.env()
	if _, err := SelfUpdate(context.Background(), f.home, env); err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if data, err := os.ReadFile(f.oldBin); err != nil || string(data) != fakePayloadScript(fakeNewTag) {
		t.Fatalf("shared target not replaced: %v", err)
	}
}

func TestSelfUpdateWaitReadyTimeoutErrors(t *testing.T) {
	f := newSelfUpdateFixture(t)
	env := f.env()
	env.reachable = func(paths.Home) bool { return true }
	env.waitReady = func(paths.Home, string, time.Duration) bool { return false }
	_, err := SelfUpdate(context.Background(), f.home, env)
	if err == nil || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("err = %v, want handshake timeout error", err)
	}
}

func TestSelfUpdateJSONLineErrorAndSuccess(t *testing.T) {
	// 失败：无 token → {"error":...} 单行（恒退出 0 契约；假编排绝不触碰真实环境）
	home := paths.Home(t.TempDir())
	env := selfUpdateEnv{progress: &strings.Builder{}}
	line := selfUpdateJSONLine(context.Background(), home, env)
	if !strings.HasPrefix(line, `{"error":`) || strings.ContainsAny(line, "\n") {
		t.Fatalf("error line = %q", line)
	}
	if strings.Contains(line, fakeTokenValue) {
		t.Fatal("error line must never echo token")
	}

	// 成功：假 API 全链 → {"ok":true,"updated_to":...} 单行；进度只在 stderr
	f := newSelfUpdateFixture(t)
	fake := f.env()
	line = selfUpdateJSONLine(context.Background(), f.home, fake)
	var decoded struct {
		OK        bool   `json:"ok"`
		UpdatedTo string `json:"updated_to"`
	}
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if !decoded.OK || decoded.UpdatedTo != fakeNewTag {
		t.Fatalf("decoded = %+v", decoded)
	}
	if strings.Contains(line, fakeTokenValue) {
		t.Fatal("output must never contain token")
	}
}

func TestExtractTarEntryRejectsMissingEntry(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	if err := os.WriteFile(good, fakeTarballBytes(t, fakeNewTag), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "payload")
	if err := extractTarEntry(good, "tradewind", dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != fakePayloadScript(fakeNewTag) {
		t.Fatalf("payload = %q, err = %v", data, err)
	}
	if err := extractTarEntry(good, "other", filepath.Join(dir, "x")); err == nil {
		t.Fatal("missing entry: want error")
	}
}
