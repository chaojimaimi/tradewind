package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// EnsureSecret 返回控制器 secret；不存在则生成 32 字节 hex 并以 600 权限落盘（首启生成语义）。
// WHY 随机 secret：DESIGN §2.7.1 实测 Clash Verge 使用默认 secret 的教训——必须禁用默认值。
func EnsureSecret(h paths.Home) (string, error) {
	data, err := os.ReadFile(h.SecretFile())
	if err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("store: read secret: %w", err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate secret: %w", err)
	}
	secret := hex.EncodeToString(buf)
	if err := SaveFileRaw(h.SecretFile(), []byte(secret+"\n")); err != nil {
		return "", err
	}
	return secret, nil
}
