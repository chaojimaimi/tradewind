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
func coreClient(home paths.Home) *core.Client {
	return core.NewClient(home.SockFile(), store.ReadSecret(home))
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
	if err := coreClient(home).SelectProxy(ctx, group, node); err != nil {
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
	item.Selected = upsertSelection(item.Selected, group, node)
	if err := store.SaveIndex(home, idx); err != nil {
		return err
	}
	fmt.Printf("selected %q for %q (recorded)\n", node, group)
	return nil
}

// Unpin 编排 `tradewind unpin`：清除 URLTest/Fallback 组的手动固定节点，恢复
// 自动测速选择（v0.4.16 用户拍板）。WHY 不回写 profiles：固定态是内核运行时状态
// （GET /proxies 的 fixed 字段），unpin 即清除，无"期望选择"需要持久化。
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

// upsertSelection 替换同组记录或追加新记录（保持 selected 数组组唯一）。
func upsertSelection(sels []store.ProxySelection, group, node string) []store.ProxySelection {
	for i := range sels {
		if sels[i].Group == group {
			sels[i].Node = node
			return sels
		}
	}
	return append(sels, store.ProxySelection{Group: group, Node: node})
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

// selectionsFrom 从全量代理快照提取组选择记录（排除 GLOBAL；now 非空才记录）。
func selectionsFrom(proxies map[string]core.Proxy) []store.ProxySelection {
	var sels []store.ProxySelection
	for _, name := range sortedGroupNames(proxies) {
		if now := proxies[name].Now; now != "" {
			sels = append(sels, store.ProxySelection{Group: name, Node: now})
		}
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
