package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// validModes 是出站模式的合法枚举（与 store.Settings.Validate 一致，错误信息用）。
var validModes = []string{"rule", "global", "direct"}

// Mode 编排 `tradewind mode`：无参显示当前模式；带参切换。
// 切换顺序即崩溃安全语义（M2-plan §2.1 v1.1）：settings 回写 → 重合成 → 运行中 PATCH——
// 先落盘再动运行态，任意中途崩溃后重启，settings/runtime.yaml 与实际运行态一致，模式不回退。
func Mode(ctx context.Context, home paths.Home, mode string) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	if mode == "" {
		fmt.Println(settings.Mode)
		return nil
	}
	return switchMode(ctx, home, settings, mode)
}

// switchMode 执行模式切换三步（回写/重合成/PATCH）。
func switchMode(ctx context.Context, home paths.Home, settings store.Settings, mode string) error {
	if !validMode(mode) {
		return fmt.Errorf("app: invalid mode %q (valid: %s)", mode, strings.Join(validModes, "|"))
	}
	settings.Mode = mode
	if err := store.SaveSettings(home, settings); err != nil {
		return err
	}
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if err := Synthesize(home, secret); err != nil {
		return err
	}
	if !CoreReachable(home) {
		fmt.Printf("mode -> %s (core not running; takes effect on next start)\n", mode)
		return nil
	}
	if err := coreClient(home).PatchConfigsMode(ctx, mode); err != nil {
		return err
	}
	fmt.Printf("mode -> %s (hot-applied)\n", mode)
	return nil
}

// validMode 校验模式枚举。WHY 不复用 Settings.Validate：这里的错误面向 CLI 用户，
// 需要列出合法取值而非内部字段名。
func validMode(mode string) bool {
	for _, m := range validModes {
		if m == mode {
			return true
		}
	}
	return false
}

// currentMode 返回 settings 中的当前模式（供 status --json 复用）。
func currentMode(home paths.Home) (string, error) {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return "", err
	}
	if !validMode(settings.Mode) {
		return "", errors.New("app: settings mode is invalid")
	}
	return settings.Mode, nil
}
