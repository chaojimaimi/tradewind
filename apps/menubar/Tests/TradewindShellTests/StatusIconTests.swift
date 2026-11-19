import SwiftUI
import Testing
@testable import TradewindKit

/// 图标三态映射与 v0.4.6 四态可视化（M2-plan §6 + 2026-11-19 拍板）。
@Suite struct StatusIconTests {
    @Test func stateMappingFollowsDemoSemantics() {
        // demo 口径：off = !running || !sysProxy → 停止/无接入区分状态枚举（编码交给色/点/透明度）
        #expect(StatusIcon.state(running: false, intake: false) == .stopped)
        #expect(StatusIcon.state(running: false, intake: true) == .stopped)
        #expect(StatusIcon.state(running: true, intake: true) == .active)
        #expect(StatusIcon.state(running: true, intake: false) == .idle)
    }

    /// v0.4.6 新语义：主形跟运行态走——active/idle 均品牌色，仅 stopped=muted 灰
    @Test func tintFollowsRunStateBrandForRunningMutedOnlyWhenStopped() {
        for scheme in [ColorScheme.light, .dark] {
            let brand = StatusIcon.tint(.active, scheme: scheme)
            #expect(StatusIcon.tint(.idle, scheme: scheme) == brand) // 运行无接入仍是品牌色
            #expect(brand != StatusIcon.tint(.stopped, scheme: scheme)) // 停止=灰可辨
            #expect(StatusIcon.tint(.stopped, scheme: scheme) == NSColor(Theme.muted(scheme)))
        }
    }

    /// v0.4.6：绿点跟代理走（仅 active）+ 停止态 0.5 淡化
    @Test func proxyDotAndAlphaEncodeIntakeAndLiveness() {
        #expect(StatusIcon.showsProxyDot(.active))
        #expect(!StatusIcon.showsProxyDot(.idle))
        #expect(!StatusIcon.showsProxyDot(.stopped))
        #expect(StatusIcon.alpha(.active) == 1)
        #expect(StatusIcon.alpha(.idle) == 1)
        #expect(StatusIcon.alpha(.stopped) == 0.5)
    }

    /// 合成图像：三态双主题均可构造、画布即主形实测尺寸（无扩带，code-reviewer P2）、
    /// 非模板（绿点色彩独立于染色）
    @Test func composedImageRendersAllStatesAndSchemes() throws {
        for scheme in [ColorScheme.light, .dark] {
            for state in [StatusIconState.active, .idle, .stopped] {
                #expect(StatusIcon.image(state, scheme: scheme) != nil)
            }
        }
        let active = try #require(StatusIcon.image(.active, scheme: .light))
        let idle = try #require(StatusIcon.image(.idle, scheme: .light))
        #expect(active.size == idle.size) // 画布统一：点区有无不影响尺寸（菜单栏不跳动）
        // 画布=主形实测尺寸（17pt "wind" 实测 21×18）：无 +6 扩带——扩带在非翻转坐标
        // 位于顶部且恒空，还会令画布被菜单栏等比缩小致主形低于 17px 规范
        #expect(active.size.width >= 21)
        #expect(active.size.height == 18)
        #expect(!active.isTemplate) // template 画布会被 contentTintColor 单色染色
    }

    @Test func symbolImageLoadsAndIsNamedWind() {
        // SF Symbols "wind" 必须在本机可解析（macOS 12+）；缺失说明运行环境过旧
        #expect(StatusIcon.symbolName(.active) == "wind")
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
