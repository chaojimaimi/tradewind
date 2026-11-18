package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/subscription"
)

// Update 手动更新订阅（target: UID | current | all）。
// 仅对 remote 型生效；file 型明确报错跳过（本地文件无更新语义，M0-plan §4）。
// 成功后内核在跑 → 重新合成 + 热重载（ReloadIfRunning）；未运行 → 仅落盘。
// mutation.lock 覆盖 fetch→SaveIndex→Synthesize→PUT 全程，与调度器互斥（M2-plan §2.2）。
func Update(ctx context.Context, home paths.Home, target string) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
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
	settings, err := store.LoadSettings(home)
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
		if err := updateOne(ctx, home, settings, item); err != nil {
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

// DefaultUpdateInterval 是订阅自动更新间隔的默认值（秒，=24h）。
// UpdateInterval 0/缺失归一到该值——防"0 恒真导致每 30 分钟全量拉取"打爆机场端点（M2-plan §2.2）。
const DefaultUpdateInterval = 86400

// NormalizeUpdateInterval interval 归一化：非正值 → 默认 86400。
func NormalizeUpdateInterval(v int) int {
	if v <= 0 {
		return DefaultUpdateInterval
	}
	return v
}

// dueForUpdate 判断条目是否到期可自动拉取：
// remote 型 + allow_auto_update 生效（指针 nil 视为 true）+ now-updated ≥ interval。
func dueForUpdate(item store.ProfileItem, now time.Time) bool {
	if item.Type != store.ProfileTypeRemote {
		return false
	}
	if item.AllowAutoUpdate != nil && !*item.AllowAutoUpdate {
		return false
	}
	return now.Unix()-item.Updated >= int64(NormalizeUpdateInterval(item.UpdateInterval))
}

// fetchForUpdate 订阅拉取的两层回退（M2-plan §2.2）：
// update_via_proxy 开启且内核可达 → 先经自身代理（127.0.0.1:mixed）拉取；
// 失败回退直连重试一次（内核在跑但隧道坏的实战场景）；两层失败才返回错误。
// 未运行/未开启 → 直连。
func fetchForUpdate(ctx context.Context, home paths.Home, settings store.Settings, rawURL string) (subscription.Result, error) {
	if settings.UpdateViaProxyEnabled() && CoreReachable(home) {
		client, err := proxyClient(settings.MixedPort)
		if err == nil {
			res, err := subscription.Fetch(ctx, client, rawURL)
			if err == nil {
				return res, nil
			}
			fmt.Printf("update: fetch via self proxy failed (%v); retrying direct\n", err)
		}
	}
	return subscription.Fetch(ctx, nil, rawURL)
}

// proxyClient 构造经自身 mixed 端口的 HTTP 客户端（超时继承 fetchTimeout）。
func proxyClient(mixedPort int) (*http.Client, error) {
	proxyURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", mixedPort))
	if err != nil {
		return nil, fmt.Errorf("app: parse proxy url: %w", err)
	}
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}, nil
}

// fetchTimeout 与 subscription 包内单次拉取超时一致（此处显式声明避免导出内部常量）。
const fetchTimeout = 30 * time.Second

// updateOne 拉取 + 校验 + 覆写订阅原文件与 extra（拉取失败则该项保持原样）。
// 成功拉取即刷新 Updated（无论内容是否变更）——调度器 due 判据依赖该语义（v1.2）。
func updateOne(ctx context.Context, home paths.Home, settings store.Settings, item *store.ProfileItem) error {
	res, err := fetchForUpdate(ctx, home, settings, item.URL)
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
