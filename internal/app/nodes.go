package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// GlobalGroup 是 mihomo 的规则外聚合组：M1 列表/快照/默认组解析一律排除（规则模式聚焦）；
// 显式 `select GLOBAL <node>` 仍可用（不做人为限制，M1-plan §4.3）。
const globalGroup = "GLOBAL"

// delayConcurrency 是组内延迟测试的有界并发上限（M1-plan §9 步骤 3：≤8）。
const delayConcurrency = 8

// snapshotTimeout 是停机/切换订阅快照回写的窗口上限——内核不可达按时跳过，不拖慢停机。
const snapshotTimeout = 5 * time.Second

// coreClient 构造 unix socket 客户端（只读 secret——运行中的内核必有 secret 文件）。
// 路径按当前会话形态分流（批⑥b §4.3：TUN 形态拨服务的代理入口）。
func coreClient(home paths.Home) *core.Client {
	return core.NewClient(coreSockPath(home), store.ReadSecret(home))
}

// requireCore 统一前置探测：未运行给行动指引而非底层 dial 报错。
func requireCore(home paths.Home) error {
	if !CoreReachable(home) {
		return errors.New("app: core is not reachable; run `tradewind start` first")
	}
	return nil
}

// Nodes 编排 `tradewind nodes`：无参列策略组（含当前选择），带参列组内节点+最近延迟。
func Nodes(ctx context.Context, home paths.Home, group string) error {
	if err := requireCore(home); err != nil {
		return err
	}
	proxies, err := coreClient(home).GetProxies(ctx)
	if err != nil {
		return err
	}
	if group == "" {
		return printGroups(proxies)
	}
	entry, ok := proxies[group]
	if !ok {
		return fmt.Errorf("app: group %q not found (run `tradewind nodes` to list)", group)
	}
	return printGroupNodes(entry, proxies)
}

// Select 编排 `tradewind select`：PUT 内核成功后才回写 profiles（触发一；内核失败不落盘）。
func Select(ctx context.Context, home paths.Home, group, node string) error {
	if group == "" || node == "" {
		return errors.New("app: usage: tradewind select <group> <node>")
	}
	if err := requireCore(home); err != nil {
		return err
	}
	client := coreClient(home)
	if err := client.SelectProxy(ctx, group, node); err != nil {
		return err // 内核失败：不落盘（M1-plan §4.3）
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	item, ok := idx.CurrentItem()
	if !ok {
		// 裸内核（无订阅）无处记录；内核侧 store-selected 仍是唯一真相（单向同步）
		fmt.Println("selected (no current profile to record)")
		return nil
	}
	// 补读取 pinned（批⑥b §4.8）：自动组只有"被手动固定过"的选择才跨形态重放
	pinned := pinnedFromKernel(ctx, client, group, item.Selected)
	item.Selected = upsertSelection(item.Selected, group, node, pinned)
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Printf("selected %q for %q (recorded)\n", node, group)
	return nil
}

// Unpin 编排 `tradewind unpin`：清除 URLTest/Fallback 组的手动固定节点，恢复
// 自动测速选择（v0.4.16 用户拍板）。WHY 回写 profiles 的 pinned（批⑥b §4.8 修订，
// 取代旧"固定态是运行时状态、不回写"注释）：pinned 是跨形态重放的判定键——
// 固定 → unpin → start 被 kill -9（没有优雅快照）的序列里，不清记录的下一次
// 跨形态重放会把已经解除的固定重新钉上。内核侧的固定态本身（GET /proxies 的
// fixed 字段）仍是运行时状态，不在此列。
func Unpin(ctx context.Context, home paths.Home, group string) error {
	if group == "" {
		return errors.New("app: usage: tradewind unpin <group>")
	}
	if err := requireCore(home); err != nil {
		return err
	}
	if err := coreClient(home).UnpinProxy(ctx, group); err != nil {
		return err
	}
	if err := clearSelectionPinned(home, group); err != nil {
		return err
	}
	fmt.Printf("unpinned %q (auto selection resumed)\n", group)
	return nil
}

// Delay 编排 `tradewind delay`：默认组解析 + 组内有界并发测试，按组内顺序输出。
func Delay(ctx context.Context, home paths.Home, group string) error {
	if err := requireCore(home); err != nil {
		return err
	}
	proxies, err := coreClient(home).GetProxies(ctx)
	if err != nil {
		return err
	}
	name, err := resolveDelayGroup(home, proxies, group)
	if err != nil {
		return err
	}
	entry := proxies[name]
	fmt.Printf("testing %q (%d members)...\n", name, len(entry.All))
	results := testDelays(ctx, home, entry, proxies)
	for _, member := range entry.All {
		fmt.Printf("%s\t%s\n", member, results[member])
	}
	return nil
}

// printGroups 输出策略组表（字典序，输出确定性是 smoke 断言的前提）。
func printGroups(proxies map[string]core.Proxy) error {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tTYPE\tNOW")
	for _, name := range sortedGroupNames(proxies) {
		g := proxies[name]
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, g.Type, g.Now)
	}
	return tw.Flush()
}

// printGroupNodes 输出组内成员表：* 标当前选择，LAST DELAY 取内核缓存的历史采样。
func printGroupNodes(entry core.Proxy, proxies map[string]core.Proxy) error {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tLAST DELAY")
	for _, member := range entry.All {
		marker, delay := " ", "-"
		if member == entry.Now {
			marker = "*"
		}
		if m, ok := proxies[member]; ok {
			if d, has := m.LastDelay(); has {
				delay = fmt.Sprintf("%d ms", d)
			}
		}
		fmt.Fprintf(tw, "%s %s\t%s\n", marker, member, delay)
	}
	return tw.Flush()
}

// sortedGroupNames 返回策略组名的字典序列表（排除 GLOBAL）。
func sortedGroupNames(proxies map[string]core.Proxy) []string {
	var names []string
	for name, p := range proxies {
		if name == globalGroup || !p.IsGroup() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// upsertSelection 替换同组记录或追加新记录（保持 selected 数组组唯一；pinned 随
// 本次补读结果一并落盘，批⑥b §4.8）。
func upsertSelection(sels []store.ProxySelection, group, node string, pinned bool) []store.ProxySelection {
	for i := range sels {
		if sels[i].Group == group {
			sels[i].Node = node
			sels[i].Pinned = pinned
			return sels
		}
	}
	return append(sels, store.ProxySelection{Group: group, Node: node, Pinned: pinned})
}

// pinnedFromKernel 是 Select 的补读判定（触发一，批⑥b §4.8）：PUT 成功后再 GET
// /proxies 取该组条目，按 selectionPinned 决定记录的 pinned。补读失败、或返回里
// 没有这个组 → pinned 沿用该组旧记录的值，并告警一行（stderr）——选择已经生效，
// select 本身不算失败。
func pinnedFromKernel(ctx context.Context, client *core.Client, group string, old []store.ProxySelection) bool {
	proxies, err := client.GetProxies(ctx)
	if err != nil {
		fmt.Fprintf(replayWarnW, "warning: 节点选择固定标记读取失败（%v），%q 的 pinned 沿用旧记录\n", err, group)
		return oldPinned(old, group)
	}
	entry, ok := proxies[group]
	if !ok {
		fmt.Fprintf(replayWarnW, "warning: 节点选择固定标记读取失败（组不在 GET /proxies 返回里），%q 的 pinned 沿用旧记录\n", group)
		return oldPinned(old, group)
	}
	return selectionPinned(entry)
}

// oldPinned 返回旧记录里该组的 pinned（无记录 → false——首次选择无历史可沿）。
func oldPinned(sels []store.ProxySelection, group string) bool {
	for _, s := range sels {
		if s.Group == group {
			return s.Pinned
		}
	}
	return false
}

// clearSelectionPinned 把 current 订阅记录里该组的 pinned 清为 false 再保存。
// 无 current 订阅、或记录里没有这个组 → 无事可做；保存失败 → 带上下文的错误
// （不清掉的话：固定 → unpin → start 被 kill -9 → 下一次跨形态重放会把已解除
// 的固定重新钉上，批⑥b §4.8）。
func clearSelectionPinned(home paths.Home, group string) error {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return fmt.Errorf("app: unpin: load profiles: %w", err)
	}
	item, ok := idx.CurrentItem()
	if !ok {
		return nil
	}
	found := false
	for i := range item.Selected {
		if item.Selected[i].Group == group {
			item.Selected[i].Pinned = false
			found = true
		}
	}
	if !found {
		return nil
	}
	if err := store.SaveIndex(home, idx); err != nil {
		return fmt.Errorf("app: unpin: clear pinned record for %q: %w", group, err)
	}
	return nil
}

// resolveDelayGroup 解析延迟测试目标组：显式指定优先；无参时取 selected 首条记录的
// 组（"当前组"的落地），无记录则回退字典序第一个策略组（确定性）。
func resolveDelayGroup(home paths.Home, proxies map[string]core.Proxy, group string) (string, error) {
	if group != "" {
		if _, ok := proxies[group]; !ok {
			return "", fmt.Errorf("app: group %q not found", group)
		}
		return group, nil
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return "", err
	}
	if item, ok := idx.CurrentItem(); ok && len(item.Selected) > 0 {
		if g := item.Selected[0].Group; g != globalGroup {
			if _, ok := proxies[g]; ok {
				return g, nil
			}
		}
	}
	names := sortedGroupNames(proxies)
	if len(names) == 0 {
		return "", errors.New("app: no policy groups found")
	}
	return names[0], nil
}

// testDelays 有界并发测试成员延迟；组类型成员跳过（实测内核 delay 端点仅接受节点，
// 对组返回 503）。结果键为成员名，值为人读文本。
func testDelays(ctx context.Context, home paths.Home, entry core.Proxy, proxies map[string]core.Proxy) map[string]string {
	results := make(map[string]string, len(entry.All))
	var mu sync.Mutex
	sem := make(chan struct{}, delayConcurrency)
	var wg sync.WaitGroup
	for _, member := range entry.All {
		if m, ok := proxies[member]; ok && m.IsGroup() {
			results[member] = "(group)"
			continue
		}
		wg.Add(1)
		go func(member string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := coreClient(home).ProxyDelay(ctx, member, core.DefaultDelayTimeoutMS, core.DefaultDelayTestURL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// 内核语义：delay 测试失败即节点不可达（实测 503，message 恒定），折为简短文案
				results[member] = "unreachable"
				return
			}
			results[member] = fmt.Sprintf("%d ms", d)
		}(member)
	}
	wg.Wait()
	return results
}

// snapshotSelected 将内核当前选择回写 idx 中 item.Selected（触发二/三共用，M1-plan §4.3）。
// WHY 内核不可达跳过且不视为错误（M1-plan §8 停机竞态对策）：下次 select 自然修正。
func snapshotSelected(ctx context.Context, home paths.Home, idx *store.Index, item *store.ProfileItem) error {
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	proxies, err := coreClient(home).GetProxies(sctx)
	if err != nil {
		return nil
	}
	item.Selected = selectionsFrom(proxies)
	return store.SaveIndex(home, *idx)
}

// selectionsFrom 从全量代理快照提取组选择记录（除 GLOBAL 外所有 now 非空的组，
// 按组名排序；GLOBAL 存在且 now 非空时追加在末尾——全局模式下它决定全部流量
// 走向，放末尾不影响 resolveDelayGroup 取首条的既有语义）。
// pinned 只标"被手动固定过的自动组"（M16 的 fixed 字段）：把自动组的 now 原样
// 重放等于把它钉死在快照那一刻的节点上（批⑥b §4.8）。
func selectionsFrom(proxies map[string]core.Proxy) []store.ProxySelection {
	var sels []store.ProxySelection
	for _, name := range sortedGroupNames(proxies) {
		if now := proxies[name].Now; now != "" {
			sels = append(sels, store.ProxySelection{Group: name, Node: now, Pinned: selectionPinned(proxies[name])})
		}
	}
	if g, ok := proxies[globalGroup]; ok && g.Now != "" {
		sels = append(sels, store.ProxySelection{Group: globalGroup, Node: g.Now, Pinned: selectionPinned(g)})
	}
	return sels
}

// snapshotBeforeSwitch 触发三：add 切换 current 前对旧订阅快照回写（内核可达时，
// 热重载前——此时内核仍运行旧配置，快照归属旧订阅）。
func snapshotBeforeSwitch(ctx context.Context, home paths.Home, idx *store.Index) error {
	old, ok := idx.CurrentItem()
	if !ok || !CoreReachable(home) {
		return nil
	}
	return snapshotSelected(ctx, home, idx, old)
}

// snapshotBeforeStop 触发二：优雅停机在 mgr.Stop() 之前快照回写当前订阅。
func snapshotBeforeStop(home paths.Home) error {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	item, ok := idx.CurrentItem()
	if !ok {
		return nil // 无订阅无可快照
	}
	return snapshotSelected(context.Background(), home, &idx, item)
}
