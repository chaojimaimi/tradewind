import SwiftUI
import Testing
@testable import TradewindKit

/// 图标三态映射（M2-plan §6 壳层单测：图标三态映射）。
@Suite struct StatusIconTests {
    @Test func stateMappingFollowsDemoSemantics() {
        // demo 口径：off = !running || !sysProxy → 停止/无接入都是灰
        #expect(StatusIcon.state(running: false, intake: false) == .stopped)
        #expect(StatusIcon.state(running: false, intake: true) == .stopped)
        #expect(StatusIcon.state(running: true, intake: true) == .active)
        #expect(StatusIcon.state(running: true, intake: false) == .idle)
    }

    @Test func activeTintDiffersFromMutedPerScheme() {
        for scheme in [ColorScheme.light, .dark] {
            let active = StatusIcon.tint(.active, scheme: scheme)
            let muted = StatusIcon.tint(.idle, scheme: scheme)
            #expect(active != muted)
            #expect(StatusIcon.tint(.idle, scheme: scheme) == StatusIcon.tint(.stopped, scheme: scheme))
        }
    }

    @Test func symbolImageLoadsAndIsNamedWind() {
        // SF Symbols "wind" 必须在本机可解析（macOS 12+）；缺失说明运行环境过旧
        #expect(StatusIcon.symbolName(.active) == "wind")
        #expect(StatusIcon.image(.active) != nil)
    }
}

/// 速率文本（demo fmtB 口径 + status item 形态）。
@Suite struct RateTextTests {
    @Test func humanReadableBuckets() {
        #expect(RateText.bytes(0) == "0 B")
        #expect(RateText.bytes(512) == "512 B")
        #expect(RateText.bytes(1024) == "1 KB")
        #expect(RateText.bytes(42_000_000) == "42.0 MB")
        #expect(RateText.bytes(1_820_000_000) == "1.8 GB")
    }

    @Test func statusItemTextShowsWhileRunningHidesWhenStopped() {
        let text = RateText.statusItem(down: 6_000_000, up: 800_000)
        #expect(text == "↓6.0 MB/s ↑800 KB/s")
        #expect(RateText.statusItem(down: nil, up: nil) == nil)
    }
}
