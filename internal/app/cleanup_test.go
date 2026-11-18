package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestCleanupLogsByRetentionDays(t *testing.T) {
	// 按天清理（M2-plan §2.4）：过期的 core.log.* 删除；新鲜轮转产物与活动 core.log 保留
	home := paths.Home(t.TempDir())
	if err := home.Ensure(); err != nil {
		t.Fatal(err)
	}
	logs := home.LogsDir()
	type logFile struct {
		name  string
		mtime time.Time
	}
	old := time.Now().AddDate(0, 0, -30)
	recent := time.Now().Add(-24 * time.Hour)
	files := []logFile{
		// 活动日志永不清理；core.log.1 到期删除；core.log.2 新鲜保留；非 core.log.* 前缀不管
		{"core.log", recent},
		{"core.log.1", old},
		{"core.log.2", recent},
		{"other.txt", old},
	}
	for _, f := range files {
		p := filepath.Join(logs, f.name)
		if err := os.WriteFile(p, []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupLogs(home); err != nil {
		t.Fatalf("cleanupLogs: %v", err)
	}
	want := map[string]bool{
		"core.log": true, "core.log.1": false, "core.log.2": true, "other.txt": true,
	}
	for name, exist := range want {
		_, err := os.Stat(filepath.Join(logs, name))
		if exist && err != nil {
			t.Fatalf("%s should be kept: %v", name, err)
		}
		if !exist && err == nil {
			t.Fatalf("%s should be removed (expired)", name)
		}
	}
}

func TestCleanupLogsNoLogsDir(t *testing.T) {
	// logs/ 不存在：no-op 成功（start 首启场景）
	if err := cleanupLogs(paths.Home(t.TempDir())); err != nil {
		t.Fatalf("cleanupLogs(no dir): %v", err)
	}
}
