package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tempKey 生成一次性临时 keypair——测试红线：真 ~/.sparkle 私钥零接触。
func tempKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate temporary keypair: %v", err)
	}
	return priv, pub
}

// b64 是 Sparkle key 文件的编码方式（base64 文本）。
func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// legacySecret 拼旧格式 secret：64 字节 Ed25519 私钥（seed‖公钥）后接 32 字节公钥。
func legacySecret(priv ed25519.PrivateKey, pub ed25519.PublicKey) []byte {
	return append(append([]byte{}, priv...), pub...)
}

func TestDerivePublicKeyNewFormat(t *testing.T) {
	priv, pub := tempKey(t)
	for name, file := range map[string][]byte{
		"no trailing newline":    []byte(b64(priv.Seed())),
		"trailing newline":       []byte(b64(priv.Seed()) + "\n"),
		"surrounding whitespace": []byte("  \n\t" + b64(priv.Seed()) + "\r\n\n"),
	} {
		got, err := derivePublicKey(file)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if !bytes.Equal(got, pub) {
			t.Fatalf("%s: must yield the public key", name)
		}
	}
}

func TestDerivePublicKeyLegacyFormat(t *testing.T) {
	priv, pub := tempKey(t)
	got, err := derivePublicKey([]byte(b64(legacySecret(priv, pub)) + "\n"))
	if err != nil {
		t.Fatalf("96-byte legacy form: unexpected error: %v", err)
	}
	if !bytes.Equal(got, pub) {
		t.Fatal("legacy form must yield the public key")
	}
	// 两处公钥副本任一与 seed 推导不符 = 损坏
	for _, at := range []int{privSize - 1, legacySecretSize - 1} {
		secret := legacySecret(priv, pub)
		secret[at] ^= 0xFF
		if _, err := derivePublicKey([]byte(b64(secret))); err == nil {
			t.Fatalf("legacy form with byte %d corrupted must be rejected", at)
		}
	}
}

// TestDerivePublicKeyRejectsNonSparkleShapes：sign_update 读不了的东西这里也放行不得
// ——否则"签名前自检"拦不住随后在 sign_update 才暴露的问题。
// wrappedB64 把 base64 文本在第 20 个字符处用 sep 折成两行（首尾空白之外的中段换行）。
func wrappedB64(data []byte, sep string) []byte {
	enc := b64(data)
	return []byte(enc[:20] + sep + enc[20:])
}

func TestDerivePublicKeyRejectsNonSparkleShapes(t *testing.T) {
	priv, _ := tempKey(t) // 折行用例需要一份真实的 seed 文本形态
	mk := func(n int) []byte {
		raw := make([]byte, n)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		return []byte(b64(raw))
	}
	cases := map[string][]byte{
		"empty":              {},
		"whitespace only":    []byte(" \n\t\r\n"),
		"not base64":         []byte("this is not base64!!\n"),
		"not utf-8":          {0xff, 0xfe, 0xfd},
		"base64 of 0 bytes":  []byte(""),
		"base64 of 31 bytes": mk(31),
		"base64 of 33 bytes": mk(33),
		"base64 of 64 bytes": mk(privSize), // seed‖公钥 不是 Sparkle 认的形态
		"base64 of 97 bytes": mk(legacySecretSize + 1),
		// 裸二进制用确定的非 base64 字节模式（0x00、0x01 循环），不拿随机 seed 赌
		// "它碰巧不是合法 base64 文本"
		"raw 32-byte seed":        bytes.Repeat([]byte{0x00, 0x01}, seedSize/2),
		"raw 64-byte private key": bytes.Repeat([]byte{0x00, 0x01}, privSize/2),
		"raw 96-byte legacy":      bytes.Repeat([]byte{0x00, 0x01}, legacySecretSize/2),
		"unpadded base64":         []byte(strings.TrimRight(b64(make([]byte, seedSize)), "=")),
		// 折行的 base64：Go 解码器忽略串中换行、Sparkle 的严格解码不认——必须拒
		"wrapped with LF":   wrappedB64(priv.Seed(), "\n"),
		"wrapped with CRLF": wrappedB64(priv.Seed(), "\r\n"),
		// 末尾数据字符的填充位非零（规范编码恒为 0）：Strict 拒
		"non-canonical padding bits": []byte(strings.Replace(
			base64.StdEncoding.EncodeToString(make([]byte, seedSize)), "A=", "B=", 1)),
		"oversized": bytes.Repeat([]byte("A"), maxKeyFileBytes+1),
	}
	for name, file := range cases {
		if _, err := derivePublicKey(file); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

// 红线断言：任何错误文本都不得含 seed/私钥的任何编码形态（raw / hex / base64）。
func TestDerivePublicKeyErrorsNeverContainKeyBytes(t *testing.T) {
	priv, pub := tempKey(t)
	seed := priv.Seed()
	corruptTail := legacySecret(priv, pub)
	corruptTail[legacySecretSize-1] ^= 0x01
	corruptMid := legacySecret(priv, pub)
	corruptMid[privSize-1] ^= 0x01
	inputs := [][]byte{
		nil,
		[]byte(" \n"),
		[]byte("not base64 at all"),
		[]byte(b64(seed[:seedSize-1])),
		[]byte(b64(priv)), // 64 字节
		[]byte(b64(corruptTail)),
		[]byte(b64(corruptMid)),
		bytes.Repeat([]byte("A"), maxKeyFileBytes+1),
	}
	needles := []string{
		string(seed), hex.EncodeToString(seed), b64(seed),
		string(priv), hex.EncodeToString(priv), b64(priv),
	}
	for i, in := range inputs {
		_, err := derivePublicKey(in)
		if err == nil {
			t.Fatalf("input #%d: want error", i)
		}
		for _, needle := range needles {
			if needle != "" && strings.Contains(err.Error(), needle) {
				t.Fatal("error message leaks key material")
			}
		}
	}
}

// 端到端：go build 进临时目录，喂临时 key 文件——stdout 只有一行 base64 公钥；
// 坏用法 exit 2；损坏/缺失 key 文件 exit 1 且信息点名路径。
func TestCLI(t *testing.T) {
	priv, pub := tempKey(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "edpubkey")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	keyPath := filepath.Join(dir, "test.ed25519.priv")
	if err := os.WriteFile(keyPath, []byte(b64(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	cmd := exec.Command(bin, "--key-file", keyPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--key-file happy path: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stdout must be the only output, stderr not empty: %q", stderr.String())
	}
	if want := b64(pub) + "\n"; string(out) != want {
		t.Fatal("stdout must be exactly the base64 public key + newline")
	}
	// 坏用法 → usage 到 stderr，exit 2
	out, err = exec.Command(bin).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("bad usage must exit 2, got %v", err)
	}
	if !strings.Contains(string(out), "usage:") {
		t.Fatalf("bad usage must print usage to stderr, got: %s", out)
	}
	// 损坏 key 文件（旧格式末尾公钥被改）→ exit 1，信息点名路径；文件内容不出现在输出里
	corruptPath := filepath.Join(dir, "corrupt.ed25519.priv")
	secret := legacySecret(priv, pub)
	secret[len(secret)-1] ^= 0xFF
	if err := os.WriteFile(corruptPath, []byte(b64(secret)), 0o600); err != nil {
		t.Fatalf("write corrupt key file: %v", err)
	}
	out, err = exec.Command(bin, "--key-file", corruptPath).CombinedOutput()
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("corrupt key file must exit 1, got %v", err)
	}
	if !strings.Contains(string(out), corruptPath) {
		t.Fatalf("error must name the key file path, got: %s", out)
	}
	for _, needle := range []string{b64(priv.Seed()), hex.EncodeToString(priv.Seed()), string(priv.Seed())} {
		if strings.Contains(string(out), needle) {
			t.Fatal("CLI error output leaks key material")
		}
	}
	// 缺失文件 → exit 1，信息点名路径
	missing := filepath.Join(dir, "absent.ed25519.priv")
	out, err = exec.Command(bin, "--key-file", missing).CombinedOutput()
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), missing) {
		t.Fatalf("missing key file must exit 1 naming the path, got %v: %s", err, out)
	}
}
