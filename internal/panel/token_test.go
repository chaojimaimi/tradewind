package panel

import (
	"os"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestEnsurePanelTokenGeneratesAndPersists(t *testing.T) {
	home := paths.Home(t.TempDir())
	tok, err := EnsurePanelToken(home)
	if err != nil {
		t.Fatalf("EnsurePanelToken: %v", err)
	}
	if len(tok) != TokenHexLen {
		t.Fatalf("token length = %d, want %d", len(tok), TokenHexLen)
	}
	if !IsTokenShape(tok) {
		t.Fatalf("token %q not token-shaped", tok)
	}
	// 文件 600 权限（凭据最小暴露）
	info, err := os.Stat(home.PanelTokenFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file perm = %o, want 600", perm)
	}
	// 第二次读取同一 token（不重新生成）
	again, err := EnsurePanelToken(home)
	if err != nil || again != tok {
		t.Fatalf("second read = %q err %v, want same %q", again, err, tok)
	}
	// ReadPanelToken 只读同值
	if ReadPanelToken(home) != tok {
		t.Fatal("ReadPanelToken mismatch")
	}
}

func TestReadPanelTokenMissing(t *testing.T) {
	home := paths.Home(t.TempDir())
	if got := ReadPanelToken(home); got != "" {
		t.Fatalf("ReadPanelToken on missing file = %q, want empty", got)
	}
}

func TestVerifyTokenConstantTimeSemantics(t *testing.T) {
	// 语义断言：相等才通过；不等（含前缀匹配、长度不等）一律拒绝
	if !VerifyToken("abcdef", "abcdef") {
		t.Fatal("equal tokens must verify")
	}
	if VerifyToken("abcdef", "abcdefg") {
		t.Fatal("prefix token must not verify")
	}
	if VerifyToken("", "abcdef") {
		t.Fatal("empty token must not verify")
	}
	if VerifyToken("wrong-wrong-wrong", "abcdef") {
		t.Fatal("wrong token must not verify")
	}
}

func TestIsTokenShape(t *testing.T) {
	good := strings.Repeat("ab", 32) // 64 hex chars
	if !IsTokenShape(good) {
		t.Fatal("64-hex string must pass shape check")
	}
	bad := []string{
		"",                                  // 空
		strings.Repeat("a", 63),             // 少一位
		strings.Repeat("a", 65),             // 多一位
		strings.Repeat("g", 64),             // 非 hex
		"proxies",                           // SPA 深链
		"PROXIES" + strings.Repeat("A", 56), // 大写（token 生成恒为小写）
	}
	for _, s := range bad {
		if IsTokenShape(s) {
			t.Fatalf("IsTokenShape(%q) should be false", s)
		}
	}
}
