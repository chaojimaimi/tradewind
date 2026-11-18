package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/subscription"
)

// 调度器周期（M2-plan §2.2/§2.4）：30 分钟巡检订阅到期；日志清理每日一次复用同一循环。
const (
	schedulerInterval = 30 * time.Minute
	cleanupInterval   = 24 * time.Hour
)

// startScheduler 在 start 运行循环内拉起单 goroutine 调度器（ctx 取消即退出）。
func startScheduler(ctx context.Context, home paths.Home) {
	go schedulerLoop(ctx, home)
}

// schedulerLoop 巡检循环：每 tick 跑一轮自动更新；距上次日志清理满一天再清理。
// 所有失败仅记日志——调度器是后台尽力而为面，绝不因自身错误拖垮 start。
func schedulerLoop(ctx context.Context, home paths.Home) {
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	lastCleanup := time.Now() // start 启动时已执行过一次（Run 内）
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := autoUpdateRound(ctx, home); err != nil {
			fmt.Printf("scheduler: auto update round: %v\n", err)
		}
		if time.Since(lastCleanup) >= cleanupInterval {
			lastCleanup = time.Now()
			if err := cleanupLogs(home); err != nil {
				fmt.Printf("scheduler: log cleanup: %v\n", err)
			}
		}
	}
}

// autoUpdateRound 单轮自动更新：持 mutation.lock 遍历到期 remote 订阅。
// 成功拉取即刷新 Updated（无论内容是否变更，v1.2 语义）；内容 sha256 变更才热重载。
// 锁竞争/单条失败不 panic：锁超时本轮放弃，单条失败记日志继续其余条目。
func autoUpdateRound(ctx context.Context, home paths.Home) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	now := time.Now()
	processed, changed := 0, 0
	for i := range idx.Items {
		item := &idx.Items[i]
		if !dueForUpdate(*item, now) {
			continue
		}
		processed++
		didChange, err := autoUpdateItem(ctx, home, settings, item)
		if err != nil {
			fmt.Printf("scheduler: update %s (%s): %v\n", item.UID, item.Name, err)
			continue
		}
		if didChange {
			changed++
			fmt.Printf("scheduler: %s (%s) content changed\n", item.UID, item.Name)
		}
	}
	if processed == 0 {
		return nil
	}
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Printf("scheduler: %d profile(s) fetched, %d changed\n", processed, changed)
	if changed == 0 {
		return nil
	}
	return ReloadIfRunning(ctx, home)
}

// autoUpdateItem 拉取单个到期订阅：内容 sha256 变更检测。
// 返回内容是否变更。成功拉取（无论变更与否）都刷新 Updated——否则
// now-updated ≥ interval 持续为真，端点轰击从另一扇门回来（M2-plan §2.2 v1.2）。
func autoUpdateItem(ctx context.Context, home paths.Home, settings store.Settings, item *store.ProfileItem) (bool, error) {
	before := fileSHA256(home.ProfileFile(item.File))
	res, err := fetchForUpdate(ctx, home, settings, item.URL)
	if err != nil {
		return false, err
	}
	if err := subscription.Validate(res.Body); err != nil {
		return false, err
	}
	if err := store.SaveProfileFile(home, item.File, res.Body); err != nil {
		return false, err
	}
	item.Updated = time.Now().Unix()
	if res.HasExtra {
		item.Extra = store.ExtraInfo{
			Upload: res.Extra.Upload, Download: res.Extra.Download,
			Total: res.Extra.Total, Expire: res.Extra.Expire,
		}
	}
	return before != fmt.Sprintf("%x", sha256.Sum256(res.Body)), nil
}

// fileSHA256 文件内容摘要；文件缺失/读失败返回占位串（调用方视作"内容未知"→ 有变更）。
func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable)"
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
