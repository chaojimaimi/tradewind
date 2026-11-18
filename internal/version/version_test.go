package version

import (
	"strings"
	"testing"
)

// TestDevFallback 锁定未注入时的回退值：go test 环境无 ldflags，Version 必须是 dev。
// ldflags 注入路径由 scripts/build.sh + smoke 的 `tradewind version` 断言覆盖（构建产物无法单测）。
func TestDevFallback(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version = %q, want dev fallback in test builds", Version)
	}
	if strings.Contains(Version, " ") {
		t.Fatalf("Version %q must not contain whitespace (ldflags -X value contract)", Version)
	}
}
