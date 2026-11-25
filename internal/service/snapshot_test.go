package service

// 快照机器的单元级测试（批⑥b §4.4）：L1 种子（seedSnapshot）、L5 回写（restoreRuntime）、
// 锁序（restoreRuntime 只取 snapMu）、日志消毒（sanitizeForLog）与内核拒绝文案。
// 服务级编排用例（reload/start 各规则）在 reload_test.go。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chaojimaimi/tradewind/internal/core"
)

// minimalProduct 是最小合法配置的归一化产物（多个用例共用）。
func minimalProduct(t *testing.T) []byte {
	t.Helper()
	res, rej := NormalizeRuntime([]byte("mode: rule\n"))
	if rej != nil {
		t.Fatalf("normalize fixture rejected at %s: %s", rej.Key, rej.Reason)
	}
	return res.Bytes
}

// seedFixture 组装 L1 种子用例的最小夹具（data + service-data，属主 = 当前用户）。
func seedFixture(t *testing.T, dir string) ConfigFile {
	t.Helper()
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ConfigFile{
		DataDir:     dataDir,
		DataUID:     os.Getuid(),
		CoreHome:    coreHome,
		UserRuntime: filepath.Join(dataDir, "runtime.yaml"),
	}
}

// TestSeedSnapshot L1 种子的五形态（批⑥b §4.4）：已归一化的 R 原样采纳（R 不动）；
// 缺失 / 符号链接 / 被当前归一化器拒绝 → 快照 nil；合法但非归一化形态 → R 被收敛
// 成产物、快照 == 产物（幂等固定点的另一半：服务升级后收紧的策略同样作用于种子）。
func TestSeedSnapshot(t *testing.T) {
	t.Run("adopts normalized R byte-identically", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		product := minimalProduct(t)
		if err := os.WriteFile(RuntimeIn(conf.CoreHome), product, 0o600); err != nil {
			t.Fatal(err)
		}
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		srv.seedSnapshot(conf)
		if got := srv.snapshotCopy(); !bytes.Equal(got, product) {
			t.Fatalf("snapshot = %q, want the seeded product", got)
		}
		disk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
		if err != nil || !bytes.Equal(disk, product) {
			t.Fatalf("R = %q err %v, want byte-identical (no rewrite)", disk, err)
		}
	})
	t.Run("missing R leaves nil snapshot", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		srv.seedSnapshot(conf)
		if got := srv.snapshotCopy(); got != nil {
			t.Fatalf("snapshot = %q, want nil", got)
		}
	})
	t.Run("symlinked R leaves nil snapshot", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		target := filepath.Join(dir, "elsewhere.yaml")
		if err := os.WriteFile(target, minimalProduct(t), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, RuntimeIn(conf.CoreHome)); err != nil {
			t.Fatal(err)
		}
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		srv.seedSnapshot(conf)
		if got := srv.snapshotCopy(); got != nil {
			t.Fatalf("snapshot = %q, want nil (safeopen refuses the symlink)", got)
		}
	})
	t.Run("rejected R leaves nil snapshot and R untouched", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		bad := []byte("external-ui: /tmp/x\n")
		if err := os.WriteFile(RuntimeIn(conf.CoreHome), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		srv.seedSnapshot(conf)
		if got := srv.snapshotCopy(); got != nil {
			t.Fatalf("snapshot = %q, want nil (current normalizer rejects the seed)", got)
		}
		disk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
		if err != nil || !bytes.Equal(disk, bad) {
			t.Fatalf("R = %q err %v, want untouched (seed never rewrites on rejection)", disk, err)
		}
	})
	t.Run("non-normalized R converges to the product", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		// 合法但缺钉死键：过当前归一化器能接受，产物 ≠ 磁盘字节 → 覆盖落盘
		if err := os.WriteFile(RuntimeIn(conf.CoreHome), []byte("mode: rule\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		srv.seedSnapshot(conf)
		product := minimalProduct(t)
		if got := srv.snapshotCopy(); !bytes.Equal(got, product) {
			t.Fatalf("snapshot = %q, want the product", got)
		}
		disk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
		if err != nil || !bytes.Equal(disk, product) {
			t.Fatalf("R = %q err %v, want rewritten to the product", disk, err)
		}
	})
}

// TestRestoreRuntime L5（PreSpawn 钩子的文件部分）四形态：快照 nil → 拒绝且绝不写
// 文件（R 缺失仍缺失——内核对缺失配置会自建默认配置以 root 跑起来，M17）；R 被篡
// 改/缺失 → 按快照回写；R 与快照一致 → 一个字节都不动（mtime 不变）。
func TestRestoreRuntime(t *testing.T) {
	t.Run("nil snapshot refuses and writes nothing", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		// R 缺失：拒绝后仍缺失（没有空文件）
		if err := srv.restoreRuntime(conf); err == nil {
			t.Fatal("restoreRuntime without a snapshot must refuse")
		}
		if _, err := os.Stat(RuntimeIn(conf.CoreHome)); !os.IsNotExist(err) {
			t.Fatalf("R must stay missing (err = %v)", err)
		}
		// R 已存在：拒绝且字节不动
		keep := []byte("keep\n")
		if err := os.WriteFile(RuntimeIn(conf.CoreHome), keep, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := srv.restoreRuntime(conf); err == nil {
			t.Fatal("restoreRuntime without a snapshot must refuse")
		}
		disk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
		if err != nil || !bytes.Equal(disk, keep) {
			t.Fatalf("R = %q err %v, want untouched", disk, err)
		}
	})
	t.Run("tampered R is rewritten from the snapshot", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		product := minimalProduct(t)
		srv.setSnapshot(product)
		if err := os.WriteFile(RuntimeIn(conf.CoreHome), []byte("garbage\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := srv.restoreRuntime(conf); err != nil {
			t.Fatalf("restoreRuntime: %v", err)
		}
		disk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
		if err != nil || !bytes.Equal(disk, product) {
			t.Fatalf("R = %q err %v, want the snapshot bytes", disk, err)
		}
	})
	t.Run("missing R is recreated from the snapshot", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		product := minimalProduct(t)
		srv.setSnapshot(product)
		if err := srv.restoreRuntime(conf); err != nil {
			t.Fatalf("restoreRuntime: %v", err)
		}
		disk, err := os.ReadFile(RuntimeIn(conf.CoreHome))
		if err != nil || !bytes.Equal(disk, product) {
			t.Fatalf("R = %q err %v, want recreated from the snapshot", disk, err)
		}
	})
	t.Run("matching R is left untouched", func(t *testing.T) {
		dir := shortTempDir(t)
		conf := seedFixture(t, dir)
		srv := NewServer(ServerConfig{})
		srv.conf = conf
		product := minimalProduct(t)
		srv.setSnapshot(product)
		if err := os.WriteFile(RuntimeIn(conf.CoreHome), product, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(RuntimeIn(conf.CoreHome))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond) // 若发生重写，mtime 必然跳过这个间隔
		if err := srv.restoreRuntime(conf); err != nil {
			t.Fatalf("restoreRuntime: %v", err)
		}
		after, err := os.Stat(RuntimeIn(conf.CoreHome))
		if err != nil {
			t.Fatal(err)
		}
		if !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("R was rewritten although it already matched the snapshot (mtime moved)")
		}
	})
}

// TestPreSpawnHookLockOrder 锁序（批⑥b §4.4）：restoreRuntime 只取 snapMu——start
// 路径调用钩子时调用方已持 s.mu，钩子内部若再取 s.mu 即自死锁。持 s.mu 直接驱动
// 钩子 + 并发循环 setSnapshot（snapMu 竞争面），钩子必须在超时内返回。
func TestPreSpawnHookLockOrder(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	srv := NewServer(cfg)
	srv.conf = conf
	srv.rootCtx = context.Background()
	allowTrust(srv)
	product := minimalProduct(t)
	srv.setSnapshot(product)
	if err := os.WriteFile(RuntimeIn(conf.CoreHome), product, 0o600); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // 并发踩 snapMu：暴露"钩子持锁做 I/O"一类的次序倒置
		for {
			select {
			case <-stop:
				return
			default:
				srv.setSnapshot(product)
			}
		}
	}()
	hookDone := make(chan error, 1)
	go func() { hookDone <- srv.preSpawnHook()() }()
	select {
	case err := <-hookDone:
		if err != nil {
			t.Fatalf("preSpawnHook returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preSpawnHook deadlocked while s.mu is held (restoreRuntime must only take snapMu)")
	}
}

// TestSanitizeForLog 日志消毒：换行/回车折空格；双向覆盖符、零宽字符、行分隔符、
// ESC、NUL、非法 UTF-8 字节一律 '?'；可打印 CJK 与空格原样；512 字节封顶且不劈开
// 字符。
func TestSanitizeForLog(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"newline and CR fold to spaces", "a\nb\rc", "a b c"},
		{"bidi override replaced", "x\u202ez", "x?z"},
		{"zero-width space replaced", "x\u200bz", "x?z"},
		{"line separator replaced", "x\u2028z", "x?z"},
		{"escape replaced", "x\x1bz", "x?z"},
		{"NUL replaced", "x\x00z", "x?z"},
		{"invalid utf8 byte replaced", "x\xffy", "x?y"},
		{"printable CJK and space survive", "中文 ok", "中文 ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeForLog(tc.in); got != tc.want {
				t.Fatalf("sanitizeForLog(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// 512 字节封顶：整段在字符边界截断（"中" 3 字节 × 400 = 1200 字节输入）
	got := sanitizeForLog(strings.Repeat("中", 400))
	if want := strings.Repeat("中", 170); got != want || len(got) != 510 {
		t.Fatalf("truncation = %d bytes, want 510 (170 chars, rune-boundary safe)", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated log text must stay valid UTF-8")
	}
}

// TestRejectionAndKernelRejectText 文案装配：归一化拒绝带键名与原因（消毒后）；内
// 核拒绝提取 {"message":...}，非 JSON 回退原文，同样消毒。
func TestRejectionAndKernelRejectText(t *testing.T) {
	rej := &Rejection{Key: "external-ui", Reason: "external-ui is not allowed in TUN mode"}
	if got, want := rejectionText(rej), "service: 配置被拒绝：external-ui（external-ui is not allowed in TUN mode）"; got != want {
		t.Fatalf("rejectionText = %q, want %q", got, want)
	}
	// 拒绝文案自身过消毒（键路径里的换行不进单行协议）
	multiline := &Rejection{Key: "a\nb", Reason: "r"}
	if got := rejectionText(multiline); got != "service: 配置被拒绝：a b（r）" {
		t.Fatalf("rejectionText(multiline) = %q, want newlines folded", got)
	}

	se := &core.StatusError{Code: 400, Body: `{"message":"fake: bad \"config\"\nline2"}`}
	if got, want := kernelRejectMessage(se), `fake: bad "config" line2`; got != want {
		t.Fatalf("kernelRejectMessage = %q, want %q (message extracted, newline folded)", got, want)
	}
	plain := &core.StatusError{Code: 500, Body: "plain text"}
	if got := kernelRejectMessage(plain); got != "plain text" {
		t.Fatalf("kernelRejectMessage(non-json) = %q, want the raw body", got)
	}
	if got := kernelRejectMessage(&core.StatusError{Code: 400, Body: "x\x00y"}); got != "x?y" {
		t.Fatalf("kernelRejectMessage(control chars) = %q, want sanitized", got)
	}
}
