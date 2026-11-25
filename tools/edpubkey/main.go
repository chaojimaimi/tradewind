// Command edpubkey reads a Sparkle EdDSA private key FILE and prints ONLY the
// derived public key (base64 std encoding, one line, stdout).
//
// WHY：release.sh 的签名前自检（batch6b plan §4.7 验签自检）需要比对"待用私钥"
// 与壳内嵌公钥（apps/menubar/sparkle-eddsa-pub.txt，即 app 实际信任的键）——
// 错私钥签出的发布装得上、appcast 照发，却被每个客户端在 Sparkle 验签处拒绝。
// 本工具是脚本链里唯一允许读私钥文件的东西，且只输出公钥：私钥字节绝不进
// stdout/stderr/日志。macOS 自带 LibreSSL 解析不了这类 key 文件（batch6b P5），
// 推导用 Go 标准库从 seed 重建公钥。
//
// 文件格式与 Sparkle 自己的 sign_update --ed-key-file 对齐（Sparkle
// common_cli/Secret.swift：decodeSecretString → decodePrivateAndPublicKeys）：
// 文件是 base64 文本，首尾空白（含换行）先剪掉；解码出的 secret 有两种形态：
// 32 字节是新格式（generate_keys -x 导出的就是它），即私钥 seed；96 字节是旧
// 格式，64 字节 Ed25519 私钥（seed‖公钥）后接 32 字节公钥。
//
// 裸二进制 seed 不是合法的 key 文件（sign_update 自己也读不了），这里同样拒：
// 本工具放行而 sign_update 拒收，自检就失去了"提前拦截"的意义——sign_update
// 解码 key 失败时会把整个 key 字符串打进错误信息。
//
// 要紧的方向是"Sparkle 拒收的本工具不得放行"，已逐项对合（c7a code review 用
// Foundation 同款调用实测）：折行的 base64 显式拒绝——Go 的解码器忽略串中的
// CR/LF，Swift 的严格解码不认；非规范的填充位（Strict）拒绝。反方向本工具有意
// 更严，遇到即中止（报错点明文件，不会误签）：带 BOM 的文件、末尾多一个 "="、
// 超过 1024 字节、旧格式里两份公钥副本与 seed 推导不符（Sparkle 取末尾那份
// 且不核对）。
//
// 用法：go run ./tools/edpubkey --key-file <path>
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	seedSize         = ed25519.SeedSize                 // 32：新格式 secret
	privSize         = ed25519.PrivateKeySize           // 64：旧格式里的 Ed25519 私钥（seed‖公钥）
	legacySecretSize = privSize + ed25519.PublicKeySize // 96：旧格式 secret
	maxKeyFileBytes  = 1024                             // 旧格式的 base64 文本 128 字符，留足首尾空白余量
	readLimit        = maxKeyFileBytes + 1              // 多读 1 字节即可识别超长文件，不把未知大小的文件读进内存
)

// notBase64 是"不是一份 Sparkle 认得的 key 文件"的统一说辞——只描述形态，不带字节。
var errNotBase64 = errors.New("key file is not base64 text (Sparkle exports a base64 seed on one line, not raw bytes)")

// derivePublicKey 从私钥文件的原始字节推导 32 字节公钥（解析规则见文件头注释）。
// 红线：错误信息只描述文件形态问题（长度/编码/损坏），绝不携带任何 key 字节。
func derivePublicKey(data []byte) ([]byte, error) {
	if len(data) > maxKeyFileBytes {
		return nil, fmt.Errorf("key file larger than %d bytes", maxKeyFileBytes)
	}
	if !utf8.Valid(data) {
		return nil, errNotBase64
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, errors.New("key file is empty")
	}
	// Go 的 base64 解码器忽略串中任意位置的 CR/LF，Swift 的严格解码（Sparkle 用的）
	// 不认——折行的 key 文件 Sparkle 读不了，这里必须同样拒
	if strings.ContainsAny(text, "\r\n") {
		return nil, errNotBase64
	}
	// Strict：非规范的填充位同样拒（规范生成的 key 不受影响；方向仍是宁可中止）
	secret, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil {
		return nil, errNotBase64
	}
	switch len(secret) {
	case seedSize: // 新格式：secret 就是 seed
		return publicKeyFromSeed(secret), nil
	case legacySecretSize:
		// 旧格式：私钥的后半段与末尾的公钥都必须等于由 seed 推导出的公钥——不等即文件
		// 损坏，sign_update 签出来的东西没人验得过
		pub := publicKeyFromSeed(secret[:seedSize])
		if !bytes.Equal(pub, secret[seedSize:privSize]) || !bytes.Equal(pub, secret[privSize:]) {
			return nil, errors.New("stored public key does not match the seed (corrupt key file)")
		}
		return pub, nil
	default:
		return nil, fmt.Errorf("decoded key has %d bytes, want %d (seed) or %d (private key + public key)",
			len(secret), seedSize, legacySecretSize)
	}
}

func publicKeyFromSeed(seed []byte) ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: edpubkey --key-file <sparkle-ed25519-private-key-file>")
	os.Exit(2)
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--key-file" {
		usage()
	}
	path := os.Args[2]
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: cannot open key file\n", path)
		os.Exit(1)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, readLimit))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: cannot read key file\n", path)
		os.Exit(1)
	}
	pub, err := derivePublicKey(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
}
