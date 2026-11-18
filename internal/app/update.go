package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/subscription"
)

// Update 手动更新订阅（target: UID | current | all）。
// 仅对 remote 型生效；file 型明确报错跳过（本地文件无更新语义，M0-plan §4）。
// 成功后内核在跑 → 重新合成 + 热重载（ReloadIfRunning）；未运行 → 仅落盘。
func Update(ctx context.Context, home paths.Home, target string) error {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	if len(idx.Items) == 0 {
		return fmt.Errorf("app: no profiles yet; run `tradewind add` first")
	}
	uids, err := resolveTargets(idx, target)
	if err != nil {
		return err
	}
	var updated, skipped []string
	for _, uid := range uids {
		item, ok := idx.ByUID(uid)
		if !ok {
			return fmt.Errorf("app: profile %s not found", uid)
		}
		if item.Type != store.ProfileTypeRemote {
			skipped = append(skipped, fmt.Sprintf("%s (%s): file-type, nothing to update", uid, item.Name))
			continue
		}
		if err := updateOne(ctx, home, item); err != nil {
			return fmt.Errorf("app: update %s (%s): %w", uid, item.Name, err)
		}
		updated = append(updated, fmt.Sprintf("%s (%s)", uid, item.Name))
	}
	for _, s := range skipped {
		fmt.Println("skip:", s)
	}
	if len(updated) == 0 {
		return fmt.Errorf("app: nothing updated (%s)", strings.Join(skipped, "; "))
	}
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Println("updated:", strings.Join(updated, ", "))
	return ReloadIfRunning(ctx, home)
}

// updateOne 拉取 + 校验 + 覆写订阅原文件与 extra（拉取失败则该项保持原样）。
func updateOne(ctx context.Context, home paths.Home, item *store.ProfileItem) error {
	res, err := subscription.Fetch(ctx, nil, item.URL)
	if err != nil {
		return err
	}
	if err := subscription.Validate(res.Body); err != nil {
		return err
	}
	if err := store.SaveProfileFile(home, item.File, res.Body); err != nil {
		return err
	}
	item.Updated = time.Now().Unix()
	if res.HasExtra {
		item.Extra = store.ExtraInfo{
			Upload: res.Extra.Upload, Download: res.Extra.Download,
			Total: res.Extra.Total, Expire: res.Extra.Expire,
		}
	}
	return nil
}

// resolveTargets 把 target 关键字解析为 uid 列表。
func resolveTargets(idx store.Index, target string) ([]string, error) {
	switch target {
	case "", "current":
		item, ok := idx.CurrentItem()
		if !ok {
			return nil, fmt.Errorf("app: no current profile; run `tradewind add` first")
		}
		return []string{item.UID}, nil
	case "all":
		var uids []string
		for i := range idx.Items {
			uids = append(uids, idx.Items[i].UID)
		}
		return uids, nil
	default:
		if _, ok := idx.ByUID(target); !ok {
			return nil, fmt.Errorf("app: unknown profile %q (use `tradewind status` to list)", target)
		}
		return []string{target}, nil
	}
}
