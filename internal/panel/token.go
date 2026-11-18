// Package panel 实现面板网关：token 凭据、loopback HTTP 网关（token 入口 +
// 会话 cookie + 静态 SPA + API 反代），M1-plan §5.2。
//
// 凭据模型：`GET /ui/<token>/` 为唯一凭据入口，校验通过后下发会话 cookie；
// 后续请求全部凭 cookie。secret 永不出网关进程（反代侧无条件覆写 Authorization）。
package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// TokenHexLen 是 panel token 的 hex 长度（32 字节随机数）。
const TokenHexLen = 64

// EnsurePanelToken 返回面板凭据 token；不存在则生成并以 600 权限落盘（首启生成语义）。
func EnsurePanelToken(h paths.Home) (string, error) {
	data, err := os.ReadFile(h.PanelTokenFile())
	if err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("panel: read token: %w", err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("panel: generate token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := store.SaveFileRaw(h.PanelTokenFile(), []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

// ReadPanelToken 只读获取 token（绝不生成）；文件缺失返回空串。
func ReadPanelToken(h paths.Home) string {
	data, err := os.ReadFile(h.PanelTokenFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// VerifyToken 以 constant-time 比较校验候选 token（防本机进程时序侧信道，M1-plan §4.2）。
func VerifyToken(input, want string) bool {
	return subtle.ConstantTimeCompare([]byte(input), []byte(want)) == 1
}

// IsTokenShape 判断候选是否形如 panel token（64 位小写 hex）。
// WHY 形状门：/ui/ 下单段路径既可能是 token 入口、也可能是 SPA 深链（如 /ui/proxies），
// 二者无法凭存在性区分，只能凭凭据的确定形状区分。
func IsTokenShape(s string) bool {
	if len(s) != TokenHexLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
