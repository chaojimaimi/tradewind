package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// List 编排 `tradewind list`：订阅总表（uid/name/type/updated/流量/到期）。
func List(home paths.Home) error {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	rows := listRows(idx)
	if len(rows) == 0 {
		fmt.Println("no profiles; run `tradewind add` first")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tUID\tNAME\tTYPE\tUPDATED\tTRAFFIC\tEXPIRE")
	for _, row := range rows {
		fmt.Fprintln(tw, row)
	}
	return tw.Flush()
}

// listRows 生成订阅表的行（纯函数，便于单测断言列内容）。
func listRows(idx store.Index) []string {
	var rows []string
	for _, item := range idx.Items {
		cur := " "
		if item.UID == idx.Current {
			cur = "*"
		}
		rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			cur, item.UID, item.Name, item.Type, formatUpdated(item.Updated),
			trafficSummary(item), expireSummary(item)))
	}
	return rows
}

// formatUpdated 人类可读的更新时间。
func formatUpdated(ts int64) string {
	if ts <= 0 {
		return "never"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// trafficSummary 已用/总量行；订阅无 extra 信息时返回 "-"。
func trafficSummary(item store.ProfileItem) string {
	if item.Extra.Total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%s / %s",
		humanBytes(item.Extra.Upload+item.Extra.Download), humanBytes(item.Extra.Total))
}

// expireSummary 到期日期；0 表示未知/永久。
func expireSummary(item store.ProfileItem) string {
	if item.Extra.Expire <= 0 {
		return "-"
	}
	return time.Unix(item.Extra.Expire, 0).Format("2006-01-02")
}

// Use 编排 `tradewind use <uid|name>`：切换当前订阅。
// 顺序（M2-plan §4）：mutation.lock → 旧订阅快照回写 → 切 current → 重合成 → 热重载。
// 快照先于切换：此刻内核仍运行旧配置，回写归属旧订阅（复用触发三，M1-plan §4.3）。
func Use(ctx context.Context, home paths.Home, target string) error {
	if target == "" {
		return errors.New("app: usage: tradewind use <uid|name>")
	}
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()

	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	item, err := resolveProfile(idx, target)
	if err != nil {
		return err
	}
	if item.UID == idx.Current {
		fmt.Printf("%q is already current\n", item.Name)
		return nil
	}
	if err := snapshotBeforeSwitch(ctx, home, &idx); err != nil {
		return err
	}
	idx.Current = item.UID
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Printf("switched current -> %q (uid %s)\n", item.Name, item.UID)
	return ReloadIfRunning(ctx, home)
}

// resolveProfile 按目标解析订阅：uid 精确匹配优先，其次 name 精确匹配；
// 同名多条报错（防歧义误切），未命中报错并列出可用条目提示。
func resolveProfile(idx store.Index, target string) (*store.ProfileItem, error) {
	if item, ok := idx.ByUID(target); ok {
		return item, nil
	}
	var hits []*store.ProfileItem
	for i := range idx.Items {
		if idx.Items[i].Name == target {
			hits = append(hits, &idx.Items[i])
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("app: unknown profile %q (run `tradewind list` to see profiles)", target)
	default:
		uids := make([]string, 0, len(hits))
		for _, h := range hits {
			uids = append(uids, h.UID)
		}
		return nil, fmt.Errorf("app: profile name %q is ambiguous (uids: %v); use uid", target, uids)
	}
}
