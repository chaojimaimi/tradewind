package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/subscription"
)

// AddSource 是 add 子命令的输入：URL 与本地文件二选一。
type AddSource struct {
	URL      string
	FilePath string
	Name     string
}

// Add 导入订阅（含校验），写入 profiles/<uid>.yaml 并成为 current。
// URL 拉取失败或校验不过 → 不落盘、返回错误（M0-plan §4）。
func Add(ctx context.Context, home paths.Home, src AddSource) error {
	if (src.URL == "") == (src.FilePath == "") {
		return fmt.Errorf("app: provide either <url> or --file <path>, not both/neither")
	}
	body, url, name, err := loadSource(ctx, src)
	if err != nil {
		return err
	}
	if err := subscription.Validate(body); err != nil {
		return err // 校验不过不落盘
	}
	uid, err := store.NewUID()
	if err != nil {
		return err
	}
	item := store.ProfileItem{
		UID:     uid,
		Type:    store.ProfileTypeFile,
		Name:    name,
		File:    uid + ".yaml",
		Updated: time.Now().Unix(),
	}
	if url != "" {
		item.Type = store.ProfileTypeRemote
		item.URL = url
	}
	if err := store.SaveProfileFile(home, item.File, body); err != nil {
		return err
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	// 触发三（M1-plan §4.3）：切换 current 前对旧订阅快照回写（内核可达时）；
	// 此刻内核仍运行旧配置，快照归属旧订阅
	if err := snapshotBeforeSwitch(ctx, home, &idx); err != nil {
		return err
	}
	idx.Items = append(idx.Items, item)
	// 导入即切换 current（同一管线是 M1 多订阅管理的正确基础，M0-plan §4）
	idx.Current = uid
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Printf("added %q (uid %s, %s) as current\n", item.Name, uid, item.Type)
	return ReloadIfRunning(ctx, home)
}

// loadSource 拉取 URL 或读取本地文件，并推导默认名称。
func loadSource(ctx context.Context, src AddSource) (body []byte, url, name string, err error) {
	if src.FilePath != "" {
		body, err = os.ReadFile(src.FilePath)
		if err != nil {
			return nil, "", "", fmt.Errorf("app: read file: %w", err)
		}
		name = src.Name
		if name == "" {
			base := filepath.Base(src.FilePath)
			name = strings.TrimSuffix(base, filepath.Ext(base))
		}
		return body, "", name, nil
	}
	res, err := subscription.Fetch(ctx, nil, src.URL)
	if err != nil {
		return nil, "", "", err
	}
	name = src.Name
	if name == "" {
		name = hostOf(src.URL)
	}
	return res.Body, src.URL, name, nil
}

// hostOf 从 URL 提取 host 作为默认订阅名（不含查询串，避免泄露 token）。
func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}
