import CoreGraphics
import Testing
@testable import TradewindKit

/// ConnectionTableLayout：连接表槽位口径与横滚判定（M3-plan §14.2 c4，评审 P1-1
/// 槽位=文本宽+16）。纯函数直调（NodeListLayoutTests 同构先例）。
@Suite struct ConnectionTableLayoutTests {
    /// 槽位/缺省常量锚定（demo 列宽 72/118/104/132 + 16 padding 口径）
    @Test func slotConstantsMatchDemoColumnMetrics() {
        #expect(ConnectionTableLayout.timeSlot == 88) // 72 + 16
        #expect(ConnectionTableLayout.trafficSlot == 148) // 132 + 16
        #expect(ConnectionTableLayout.defaults.host == 150)
        #expect(ConnectionTableLayout.defaults.rule == 118)
        #expect(ConnectionTableLayout.defaults.node == 104)
    }

    /// 填充分支：host 偏好槽 < 可视富余 → host 槽填满富余，
    /// tableWidth == available（不横滚）
    @Test func hostSlotFillsSurplusWithoutHorizontalScroll() {
        // others = time 88 + rule(118+16) + node(104+16) + traffic 148 = 490；
        // 宽可视 700 → 富余 210 > 偏好槽 116（hostPref=100）
        let others: CGFloat = 490
        let available: CGFloat = 700
        let slot = ConnectionTableLayout.hostSlot(hostPref: 100, othersTotal: others, available: available)
        #expect(slot == available - others)
        let width = ConnectionTableLayout.tableWidth(othersTotal: others, hostSlot: slot, available: available)
        #expect(width == available) // 填满可视宽，无横滚
    }

    /// 溢出分支：host 偏好槽 > 可视富余 → 取偏好槽，tableWidth > available（横滚）
    @Test func hostSlotOverflowsIntoHorizontalScroll() {
        let others: CGFloat = 490
        let available: CGFloat = 538
        let slot = ConnectionTableLayout.hostSlot(hostPref: 200, othersTotal: others, available: available)
        #expect(slot == 216) // 偏好槽 = 200 + 16（> 富余 48）
        let width = ConnectionTableLayout.tableWidth(othersTotal: others, hostSlot: slot, available: available)
        #expect(width == others + slot)
        #expect(width > available)
    }

    /// 文本宽 clamp：越上界/越下界收敛到边界，区间内原值不动（range 锁真实
    /// hostRange 常量——下界 94 见 ConnectionTableLayout.hostRange 注，评审 P3-2）
    @Test func clampedBoundsAndInRange() {
        let range = ConnectionTableLayout.hostRange
        #expect(range.lowerBound == 94)
        #expect(ConnectionTableLayout.clamped(50, to: range) == 94)
        #expect(ConnectionTableLayout.clamped(999, to: range) == 600)
        #expect(ConnectionTableLayout.clamped(237, to: range) == 237)
    }

    /// 全列拖至最小总槽恰等于可视宽（评审 P3-2）：88+96+96+148+110=538——
    /// "填满不横滚"分支在固定 780 窗内可达（host 下界 94 的存在依据）
    @Test func minimumSlotsReachExactFitAtStandardViewport() {
        let total = ConnectionTableLayout.timeSlot
            + (ConnectionTableLayout.hostRange.lowerBound + 16)
            + (ConnectionTableLayout.ruleRange.lowerBound + 16)
            + (ConnectionTableLayout.nodeRange.lowerBound + 16)
            + ConnectionTableLayout.trafficSlot
        #expect(total == 538)
        let hostSlot = ConnectionTableLayout.hostSlot(
            hostPref: ConnectionTableLayout.hostRange.lowerBound,
            othersTotal: total - ConnectionTableLayout.hostRange.lowerBound - 16,
            available: 538)
        #expect(hostSlot == 110)
        #expect(ConnectionTableLayout.tableWidth(
            othersTotal: total - 110, hostSlot: hostSlot, available: 538) == 538)
    }

    /// 缺省总槽算术（§14.4 目验 3）：88+166+134+120+148=656 > 538——缺省即横滚
    /// （窗口 780 - sidebar 198 - padding 44 = 可视 538，评审 P3 措辞对齐）
    @Test func defaultSlotsSumExceedsAvailableViewport() {
        let defaults = ConnectionTableLayout.defaults
        let others = ConnectionTableLayout.timeSlot
            + defaults.rule + 16 + defaults.node + 16 + ConnectionTableLayout.trafficSlot
        #expect(others == 490)
        let hostSlot = ConnectionTableLayout.hostSlot(hostPref: defaults.host,
                                                      othersTotal: others, available: 538)
        #expect(hostSlot == 166) // 150 + 16（> 富余 48，缺省即溢出横滚）
        #expect(others + hostSlot == 656)
        #expect(others + hostSlot > 538)
    }
}
