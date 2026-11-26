// replay.go：节点选择跨形态接力（批⑥b §4.8，F18/M16 的直接后果）。
//
// 两种会话形态的内核各持一份互不相通的 cache.db（I1 禁止 root 读写用户目录），
// 节点选择只靠内核缓存恢复——不接力的话每次 tun on / tun off 策略组都会回到
// 默认节点。对策全在用户域：切换形态后的下一次启动，把 profiles.yaml 里记录的
// 组选择重放给新内核（只用代理白名单内的 GET /proxies 与 PUT /proxies/{组}）。
//
// 只在跨形态时重放（WHY）：同一形态内，内核自己那份 cache.db 才是最新真相——
// 面板做的选择只进 cache.db、不进 profiles.yaml，无条件重放会让陈旧记录覆盖它；
// 跨形态时新内核读的是另一份落后的 cache.db，快照才是更新的那个。
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// 会话形态名（state.json 顶层 last_form 的取值）。
const (
	formNameTun  = "tun"
	formNameUser = "user"
)

// 重放的时间预算：单次 PUT 2s（可以比 Fallback.Set 的同步测速短——它先置位、
// 后测速，客户端等到超时也不影响已经置上的位），总预算 10s。
// 包级变量而非常量：超时/耗尽路径的测试经缝隙缩短，不等真实时长。
var (
	replayPutTimeout = 2 * time.Second
	replayBudget     = 10 * time.Second
)

// replayWarnW 是重放/补读告警的输出面（stderr——stdout 有契约，不容告警污染）。
// 包级缝隙：测试注入 buffer 断言文案；不在 fd 层重定向（go test -json 下 testing
// 包会把 os.Stderr 指到 os.Stdout，fd 层截不到）。
var replayWarnW io.Writer = os.Stderr

// formName 把会话形态布尔折算成 state.json 顶层 last_form 的取值。
func formName(tun bool) string {
	if tun {
		return formNameTun
	}
	return formNameUser
}

// isAutoGroup 报告组类型是否为"手动选择会形成固定"的自动组（M16：这两类组的
// PUT 即固定、DELETE 解除、GET 带 fixed 字段；Selector 无固定语义）。
func isAutoGroup(groupType string) bool {
	return groupType == "URLTest" || groupType == "Fallback"
}

// selectionPinned 判定一条快照/补读结果是否带 pinned：仅自动组且内核 fixed 非空。
// Selector 组重放无条件 PUT，不需要标记；未固定的自动组 PUT 会把自动测速钉死
// 在快照那一刻的节点上，绝不能带标记。
func selectionPinned(entry core.Proxy) bool {
	return isAutoGroup(entry.Type) && entry.Fixed != ""
}

// replaySelected 把 selected 逐条重放到刚就绪的内核上。绝不返回错误、绝不使
// start 失败：单条失败继续下一条，读失败/预算耗尽只留至多一行 warning（stderr）。
// 判定只用记录里的 pinned 标记与组类型——内核此刻的 fixed 不逐条核对（组测速
// 解除固定后记录尚未同步的窗口是登记在案的边界 R30(4)）。
func replaySelected(ctx context.Context, client *core.Client, selected []store.ProxySelection) {
	bctx, cancel := context.WithTimeout(ctx, replayBudget)
	defer cancel()
	proxies, err := client.GetProxies(bctx)
	if err != nil {
		fmt.Fprintln(replayWarnW, "warning: 节点选择重放：读取组类型失败，本次跳过重放")
		return
	}
	// eligible 是按规则应当重放的条数（含预算耗尽后没轮到的），missed 是其中
	// 未落上的（单条失败 + 预算耗尽跳过）——汇总行 N/M 的分母分子。
	eligible, missed := 0, 0
	for _, sel := range selected {
		entry, ok := proxies[sel.Group]
		if !ok {
			continue // 组不存在（新配置裁掉了）：跳过
		}
		if entry.Type != "Selector" && !(isAutoGroup(entry.Type) && sel.Pinned) {
			continue // 未固定的自动组、无 pinned 的旧记录、Relay/LoadBalance/未知：零 PUT
		}
		eligible++
		if bctx.Err() != nil {
			missed++ // 总预算耗尽：停，剩余条目按未重放计
			continue
		}
		pctx, pcancel := context.WithTimeout(bctx, replayPutTimeout)
		if err := client.SelectProxy(pctx, sel.Group, sel.Node); err != nil {
			missed++
		}
		pcancel()
	}
	if missed > 0 {
		fmt.Fprintf(replayWarnW, "warning: 节点选择重放：%d/%d 条未重放（内核继续运行，这些组保持默认选择）\n", missed, eligible)
	}
}

// replayIfFormChanged 只在跨形态时调用 replaySelected：state.json 顶层 last_form
// 与本次启动的形态不同才动。调用方必须在 postHandshake 写 last_form 之前、并按
// 本次启动的形态显式给 socket 路径（此刻 state 还是上一会话的形态，coreClient
// 照 state 取会拨到另一形态的路径）。
func replayIfFormChanged(ctx context.Context, home paths.Home, client *core.Client, form string) {
	st, err := store.LoadState(home)
	if err != nil || st.LastForm == "" || st.LastForm == form {
		return // 读不出、首次启动、同一形态：都不动
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return
	}
	item, ok := idx.CurrentItem()
	if !ok || len(item.Selected) == 0 {
		return
	}
	replaySelected(ctx, client, item.Selected)
}
