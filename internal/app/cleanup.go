package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// rotatedLogPrefix 是滚动日志轮转产物的前缀（core.log.1 等）。
// 活动中的 core.log 不在此列——按大小轮转由 core.Manager.rotateLog 负责（5MB，M2-plan §2.4）。
const rotatedLogPrefix = "core.log."

// cleanupLogs 按天清理滚动日志（M2-plan §2.4）：删除 mtime 早于 log_retention_days 的
// core.log.* 轮转产物。执行点：start 启动时 + 调度器每日 ticker。
// 单个文件删除失败不阻断其余（合并报错，由调用方记日志）。
func cleanupLogs(home paths.Home) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(home.LogsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // logs/ 尚未创建：无可清理
		}
		return fmt.Errorf("app: read logs dir: %w", err)
	}
	cutoff := time.Now().AddDate(0, 0, -settings.LogRetentionDays)
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), rotatedLogPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", e.Name(), err))
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(home.LogsDir(), e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Name(), err))
			continue
		}
		fmt.Printf("cleanup: removed expired log %s\n", e.Name())
	}
	return errors.Join(errs...)
}
