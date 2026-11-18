// Package core 负责 mihomo 内核二进制校验、进程生命周期与 unix socket 控制器客户端。
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// FallbackSHA256 是随项目分发的 mihomo v1.19.34 darwin/arm64 期望摘要（M0-plan §10）。
// 正常路径读 bin/mihomo.sha256 sidecar；sidecar 缺失时才回退此常量。
const FallbackSHA256 = "a55a9073f9b7b1fa596a82c034d2dae3407ec34732102b6e42c5513ce3033f34"

// VerifyBinary 校验内核二进制 sha256 与期望值一致。
// 期望值来源：sidecar 文件首行前 64 字符（兼容 `shasum -a 256` 输出格式）；
// sidecar 缺失时回退代码内置常量；均不匹配 → 拒绝启动（M0-plan §5.1 步骤 3）。
func VerifyBinary(binPath, sidecarPath string) error {
	expected := FallbackSHA256
	data, err := os.ReadFile(sidecarPath)
	switch {
	case err == nil:
		line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
		if len(line) >= 64 {
			expected = strings.ToLower(line[:64])
		}
		// sidecar 内容异常（过短）时保留 fallback 而非直接失败
	case errors.Is(err, os.ErrNotExist):
		// 回退内置期望值
	default:
		return fmt.Errorf("core: read sidecar %s: %w", sidecarPath, err)
	}
	f, err := os.Open(binPath)
	if err != nil {
		return fmt.Errorf("core: open binary %s: %w", binPath, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("core: hash binary: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("core: sha256 mismatch for %s: got %s, want %s (refusing to start)",
			binPath, actual, expected)
	}
	return nil
}
