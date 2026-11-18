package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyBinarySidecarMatch(t *testing.T) {
	dir := t.TempDir()
	bin := writeTempFile(t, dir, "mihomo", "#!/bin/sh\nexit 0\n")
	sidecar := writeSidecar(t, dir, bin)
	if err := VerifyBinary(bin, sidecar); err != nil {
		t.Fatalf("VerifyBinary: %v", err)
	}
}

func TestVerifyBinarySidecarMissingFallsBack(t *testing.T) {
	// sidecar 缺失 → 回退内置常量；桩二进制哈希不等于常量 → 必须拒绝启动
	dir := t.TempDir()
	bin := writeTempFile(t, dir, "mihomo", "#!/bin/sh\nexit 0\n")
	err := VerifyBinary(bin, filepath.Join(dir, "missing.sha256"))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want mismatch", err)
	}
}

func TestVerifyBinaryMismatchRefuses(t *testing.T) {
	// 发布物损坏/被篡改场景：sidecar 与二进制不一致 → 拒绝启动
	dir := t.TempDir()
	bin := writeTempFile(t, dir, "mihomo", "tampered\n")
	sidecar := writeTempFile(t, dir, "mihomo.sha256",
		"0000000000000000000000000000000000000000000000000000000000000000  /path/to/mihomo\n")
	err := VerifyBinary(bin, sidecar)
	if err == nil {
		t.Fatal("mismatched binary: want error")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("err = %v, want refuse wording", err)
	}
}

func TestVerifyBinaryMissingBinary(t *testing.T) {
	dir := t.TempDir()
	err := VerifyBinary(filepath.Join(dir, "no-such-bin"), filepath.Join(dir, "s.sha256"))
	if err == nil || !os.IsNotExist(err) && !strings.Contains(err.Error(), "no-such-bin") {
		t.Fatalf("err = %v, want missing binary error", err)
	}
}

func TestFallbackSHA256MatchesShippedBinary(t *testing.T) {
	// 代码内置常量必须与预置内核一致；内核换版时此测试第一时间报警
	home, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	bin := filepath.Join(home, "tradewind", "bin", "mihomo")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("shipped mihomo not found: %v", err)
	}
	out, err := exec.Command("shasum", "-a", "256", bin).Output()
	if err != nil {
		t.Skipf("shasum failed: %v", err)
	}
	actual := strings.Fields(string(out))[0]
	if actual != FallbackSHA256 {
		t.Fatalf("shipped mihomo sha256 = %s, code fallback = %s (update FallbackSHA256 or sidecar)",
			actual, FallbackSHA256)
	}
}
