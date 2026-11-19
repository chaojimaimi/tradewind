import CoreGraphics
import Testing
@testable import TradewindKit

/// NodeListLayout：demo max-height 语义的确定性判定 + 行高基准守门（批①.5 §3.1）。
@Suite struct NodeListLayoutTests {
    @Test func thresholdBoundariesMatchDemoMaxHeight() {
        // 行高基准 30、滚动高 168 → floor(168/30)=5 行内全显，超出固定 168 内部滚动
        #expect(NodeListLayout.layout(forCount: 1) == .fitAll)
        #expect(NodeListLayout.layout(forCount: 5) == .fitAll)
        #expect(NodeListLayout.layout(forCount: 6) == .fixedScroll)
        #expect(NodeListLayout.layout(forCount: 43) == .fixedScroll) // Auto 实测 43 节点
    }

    @Test func constantsMatchNodeRowMetrics() {
        // NodeRow vertical padding 7×2 + 12.5pt 文本行 ≈ 16 → 30（M3-plan §3.1 推导）
        #expect(NodeListLayout.rowHeightBaseline == 30)
        #expect(NodeListLayout.scrollHeight == 168)
        #expect(NodeListLayout.fitThreshold == Int(NodeListLayout.scrollHeight / NodeListLayout.rowHeightBaseline))
        // 不变量自执行（code-reviewer P2）：fitAll 全显高度不得超出滚动高——
        // 未来调行高/间距基线时静默破坏阈值会在此报警，而非只靠真机终验兜底
        #expect(CGFloat(NodeListLayout.fitThreshold) * NodeListLayout.rowHeightBaseline
                    + CGFloat(NodeListLayout.fitThreshold - 1) * 2 <= NodeListLayout.scrollHeight)
    }
}
