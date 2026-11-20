package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// List 编排 `tradewind list`：订阅总表（uid/name/type/updated/流量/到期）。
// --json 输出机器可读单行 JSON 数组（壳订阅管理页数据源，M3-plan §11.2-5）。
func List(home paths.Home, asJSON bool) error {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	if asJSON {
		line, err := MarshalListJSON(listReport(idx))
		if err != nil {
			return err
		}
		fmt.Println(line)
		return nil
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

// ProfileJSON 是 `list --json` 的单条输出（字段对照 ProfileItem，壳侧解码模型）。
type ProfileJSON struct {
	// Current 是否当前订阅（人读表的 * 标记的机器可读形态）
	Current bool   `json:"current"`
	UID     string `json:"uid"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	// URL 订阅源地址（remote 型；file 型为空。批② c3 订阅卡 host 展示——
	// demo 元信息行即 URL host；omitempty 对 file 型输出无 url 键，壳侧容缺）
	URL string `json:"url,omitempty"`
	// Updated 最近导入/更新的 unix 秒
	Updated int64           `json:"updated"`
	Extra   store.ExtraInfo `json:"extra"`
	// AllowAutoUpdate 指针语义与 ProfileItem 一致：nil=未设置（默认允许），
	// 非 nil false=显式关闭（subs interval off）。omitempty 对指针只吞 nil 不吞
	// &false，关态显式输出 "allow_auto_update":false（审查 P3：机制是指针兜底，
	// 勿据旧注释改 tag）
	AllowAutoUpdate *bool `json:"allow_auto_update,omitempty"`
	UpdateInterval  int   `json:"update_interval,omitempty"`
}

// listReport 把索引转为 JSON 行集（纯函数，便于单测断言字段）。
func listReport(idx store.Index) []ProfileJSON {
	var out []ProfileJSON
	for _, item := range idx.Items {
		out = append(out, ProfileJSON{
			Current:         item.UID == idx.Current,
			UID:             item.UID,
			Name:            item.Name,
			Type:            item.Type,
			URL:             item.URL,
			Updated:         item.Updated,
			Extra:           item.Extra,
			AllowAutoUpdate: item.AllowAutoUpdate,
			UpdateInterval:  item.UpdateInterval,
		})
	}
	return out
}

// MarshalListJSON 序列化（缩进关闭，壳按 stdout 单行 JSON 解析）；空表输出 []。
func MarshalListJSON(items []ProfileJSON) (string, error) {
	if items == nil {
		items = []ProfileJSON{}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("app: marshal list json: %w", err)
	}
	return string(data), nil
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
// 顺序（M2-plan §4；批④ §2.2 前插语义预检）：mutation.lock → 目标订阅合成候选
// mihomo -t 预检（失败=报错返回，index/runtime/snapshot 全不动——收口批③登记的
// -t 绕过窗口）→ 旧订阅快照回写 → 切 current → 重合成 → 热重载。
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
	// 语义预检（批④ §2.2）：目标订阅字节 + 该 uid 增强层合成候选过 mihomo -t；
	// 失败在任何落盘之前中止（precheck 内部只读，不触碰 index/runtime/snapshot）
	profileBytes, err := os.ReadFile(home.ProfileFile(item.File))
	if err != nil {
		return fmt.Errorf("app: read profile %s: %w", item.File, err)
	}
	if err := precheckCandidateBytes(home, item.UID, profileBytes); err != nil {
		return err
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

// SubsDelete 编排 `tradewind subs delete <uid|name> [--force]`（M3-plan §11.2-5）：
// profiles.yaml 移除条目 + 删除 profiles/<uid>.yaml 原文件；current 保护（--force 才放行）。
// 删 current 语义：Current 置空 → 重合成纯模板裸内核 + ReloadIfRunning（未运行则跳过）。
func SubsDelete(ctx context.Context, home paths.Home, target string, force bool) error {
	if target == "" {
		return errors.New("app: usage: tradewind subs delete <uid|name> [--force]")
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
	isCurrent := item.UID == idx.Current
	if isCurrent && !force {
		return fmt.Errorf("app: %q is the current subscription; pass --force to delete it "+
			"(Tradewind falls back to the bare core)", item.Name)
	}
	// 摘要字段先拷贝：下面的原地过滤会移动元素，item 指针随底层重写而失效
	uid, name, file := item.UID, item.Name, item.File
	// 先索引后文件：索引是唯一真相源，文件已删而索引残留才会误引用（反向残留无害）
	remaining := idx.Items[:0]
	for _, it := range idx.Items {
		if it.UID != uid {
			remaining = append(remaining, it)
		}
	}
	idx.Items = remaining
	if isCurrent {
		idx.Current = ""
	}
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	if err := os.Remove(home.ProfileFile(file)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("app: remove profile file %s: %w", file, err)
	}
	fmt.Printf("deleted %q (uid %s)\n", name, uid)
	if isCurrent {
		return bareCoreFallback(ctx, home)
	}
	return nil
}

// bareCoreFallback 删 current 后的裸内核落位：Current 已置空，重合成纯模板 +
// 运行中热重载（未运行仅落盘），并提示用户后续动作（M3-plan §11.2-5）。
// WHY 显式合成保留 + PreSynthesized 变体（批④ P3-7）：本函数的显式 Synthesize 是
// 内核未运行分支唯一的 runtime.yaml 落盘点（F14），删掉会让未运行时停留旧内容。
func bareCoreFallback(ctx context.Context, home paths.Home) error {
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if err := Synthesize(home, secret); err != nil {
		return err
	}
	if err := ReloadIfRunningPreSynthesized(ctx, home); err != nil {
		return err
	}
	fmt.Println("current subscription deleted; Tradewind is now a bare core — import or `tradewind use` a subscription")
	return nil
}

// subsIntervalHours 是 `subs interval` 合法小时档位（demo 通用页 select 同款），秒值 = 小时×3600。
var subsIntervalHours = map[string]int{"1": 1, "12": 12, "24": 24}

// SubsInterval 编排 `tradewind subs interval <uid|name> <off|1|12|24>`（评审 P1-2 语义）：
// off → 写 AllowAutoUpdate=false（真实关闭开关——UpdateInterval=0 会被
// NormalizeUpdateInterval 归一为 86400，0≠关闭，勿混淆）；1/12/24 → 写秒值且允许自动更新。
// 仅改 profiles.yaml（调度器自然生效），不触发内核 reload；持 mutation.lock 与调度器互斥。
func SubsInterval(_ context.Context, home paths.Home, target, mode string) error {
	if target == "" {
		return errors.New("app: usage: tradewind subs interval <uid|name> <off|1|12|24>")
	}
	allow := true
	interval := 0
	switch mode {
	case "off":
		allow = false
	default:
		hours, ok := subsIntervalHours[mode]
		if !ok {
			return fmt.Errorf("app: subs interval: invalid mode %q (valid: off|1|12|24)", mode)
		}
		interval = hours * 3600
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
	item.AllowAutoUpdate = &allow
	item.UpdateInterval = interval
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	if !allow {
		fmt.Printf("auto update off for %q (uid %s)\n", item.Name, item.UID)
		return nil
	}
	fmt.Printf("update interval for %q (uid %s) -> every %dh\n", item.Name, item.UID, interval/3600)
	return nil
}

// SubsRename 编排 `tradewind subs rename <uid|name> <new_name>`（M3-plan §14 R1）：
// 纯索引写——name 全仓消费仅展示/日志与 resolveProfile 解析目标，不进合成配置
// （synth 只吃订阅文件字节按白名单取键），故不 ReloadIfRunning（热重载是伪需求）。
// 同 uid 同名幂等 no-op；与其他条目重名显式拒绝（resolveProfile 同名多条报歧义，
// rename 不得制造重名）。持 mutation.lock 与其他索引写互斥（SubsDelete 同先例）。
func SubsRename(_ context.Context, home paths.Home, target, newName string) error {
	if target == "" {
		return errors.New("app: usage: tradewind subs rename <uid|name> <new_name>")
	}
	name := strings.TrimSpace(newName)
	if name == "" {
		return errors.New("app: usage: tradewind subs rename <uid|name> <new_name>")
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
	if item.Name == name {
		fmt.Printf("renamed %q -> %q (uid %s)\n", item.Name, name, item.UID)
		return nil
	}
	for i := range idx.Items {
		if idx.Items[i].UID != item.UID && idx.Items[i].Name == name {
			return fmt.Errorf("app: name %q is already in use", name)
		}
	}
	oldName := item.Name
	item.Name = name
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Printf("renamed %q -> %q (uid %s)\n", oldName, name, item.UID)
	return nil
}
