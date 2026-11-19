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

    /// v0.4.13 双色语义：运行态=设计资产原色（亮青标形），停止态=退灰可辨
    @Test func markFollowsRunStateBrightBrandForRunningGrayForStopped() throws {
        let active = try #require(StatusIcon.image(.active, scheme: .light))
        let stopped = try #require(StatusIcon.image(.stopped, scheme: .light))
        // 标形厚处（位图 (8,9)，v0.4.12 测试同点位）：运行=亮青（亮且绿主导），
        // 停止=中灰（灰且亮度显著低于亮青）
        let runningMark = try pixelColor(of: active, x: 8, y: 9)
        #expect(runningMark.alphaComponent > 0.9)
        #expect(runningMark.brightnessComponent > 0.5)
        let stoppedMark = try pixelColor(of: stopped, x: 8, y: 9)
        #expect(stoppedMark.alphaComponent > 0.9)
        #expect(stoppedMark.brightnessComponent < 0.75)
        #expect(abs(stoppedMark.redComponent - stoppedMark.greenComponent) < 0.1)
        #expect(abs(stoppedMark.greenComponent - stoppedMark.blueComponent) < 0.1)
        // 停止态双色结构锁点（审查 P3-4）：灰板/灰标仍分层，防未来误用同渐变
        let stoppedPlate = try pixelColor(of: stopped, x: 2, y: 6)
        #expect(stoppedPlate.alphaComponent > 0.9)
        #expect(stoppedMark.brightnessComponent > stoppedPlate.brightnessComponent + 0.15)
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

    /// 合成图像：三态双主题均可构造、画布 16×16 point（v0.4.10 品牌位图主形）、
    /// 非模板（绿点色彩独立于染色）+ active 像素含代理绿点（idle 同位无绿）
    @Test func composedImageRendersAllStatesAndSchemes() throws {
        for scheme in [ColorScheme.light, .dark] {
            for state in [StatusIconState.active, .idle, .stopped] {
                #expect(StatusIcon.image(state, scheme: scheme) != nil)
            }
        }
        let active = try #require(StatusIcon.image(.active, scheme: .light))
        let idle = try #require(StatusIcon.image(.idle, scheme: .light))
        #expect(active.size == idle.size) // 画布统一：点区有无不影响尺寸（菜单栏不跳动）
        // 画布=品牌位图逻辑尺寸（16×16 point；@1x 16px + @2x 32px 双密度表示）
        #expect(active.size.width == 16)
        #expect(active.size.height == 16)
        #expect(!active.isTemplate) // template 画布会被 contentTintColor 单色染色
        // 像素验证：绿点落右下点区中心（点区 drawing 坐标 (9,1,6,6) 中心 (12,4)，
        // 位图 top-left 原点 y=16-4=12）；idle 同位无绿（主形仅品牌/灰烘焙）
        #expect(isProxyDotGreen(try pixelColor(of: active, x: 12, y: 12)))
        #expect(!isProxyDotGreen(try pixelColor(of: idle, x: 12, y: 12)))
    }

    /// v0.4.10 品牌资产：Bundle.module 两份 PNG 可加载（非 nil 且 16/32 像素尺寸），
    /// 双表示合成 16pt 品牌图像可用
    @Test func menubarBrandAssetsLoadAtBothScales() throws {
        for (name, pixels) in [("tradewind-menubar-16", 16), ("tradewind-menubar-32", 32)] {
            // WHY 不带 subdirectory：SPM 将资源平铺进 bundle 的 Contents/Resources/ 根
            let url = try #require(Bundle.module.url(forResource: name, withExtension: "png"))
            let rep = try #require(NSImageRep(contentsOf: url))
            #expect(rep.pixelsWide == pixels)
            #expect(rep.pixelsHigh == pixels)
        }
        #expect(StatusIcon.brandImage() != nil)
    }

    /// v0.4.13 双色徽章（用户复测否决单色镂空："T 与底色板分别有色"）：
    /// 深墨绿底板不透明、亮青标形不透明、四角圆弧外透明。位图 top-left 原点 y=16-绘制y。
    @Test func composedImageBakesTwoToneBadge() throws {
        let active = try #require(StatusIcon.image(.active, scheme: .light))
        // 板区（标形位图外，左缘中段 绘制 (2.5,9.5)→位图 (2,6)）：深墨绿=暗且不透明
        let platePixel = try pixelColor(of: active, x: 2, y: 6)
        #expect(platePixel.alphaComponent > 0.9)
        #expect(platePixel.brightnessComponent < 0.4)
        #expect(platePixel.blueComponent > platePixel.redComponent) // 深绿蓝调
        // 标形亮青（竖干厚处 绘制 (8.5,6.5)→位图 (8,9)）：亮、青绿主导
        let markPixel = try pixelColor(of: active, x: 8, y: 9)
        #expect(markPixel.alphaComponent > 0.9)
        #expect(markPixel.brightnessComponent > 0.5)
        #expect(markPixel.greenComponent > markPixel.redComponent)
        // 板与标明暗对比（双色结构锁点）：标显著亮于板
        #expect(markPixel.brightnessComponent > platePixel.brightnessComponent + 0.15)
        // 圆弧外角（绘制 (0.5,0.5)→位图 (0,15) 底行）：板内缩 0.5pt+圆角，角点透明
        let cornerPixel = try pixelColor(of: active, x: 0, y: 15)
        #expect(cornerPixel.alphaComponent < 0.1)
    }

    // MARK: 像素取样辅助

    /// 合成图绘入 16×16 RGBA 位图后读指定点颜色（换算 sRGB 分量比对）。
    /// WHY 像素级验证：绿点合成是绘制管线的输出语义，仅断言非 nil 不足以锁定。
    private func pixelColor(of image: NSImage, x: Int, y: Int) throws -> NSColor {
        let rep = try #require(NSBitmapImageRep(
            bitmapDataPlanes: nil, pixelsWide: Int(image.size.width),
            pixelsHigh: Int(image.size.height), bitsPerSample: 8, samplesPerPixel: 4,
            hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
            bytesPerRow: 0, bitsPerPixel: 0))
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        image.draw(in: NSRect(origin: .zero, size: image.size))
        NSGraphicsContext.restoreGraphicsState()
        let color = try #require(rep.colorAt(x: x, y: y))
        return try #require(color.usingColorSpace(.sRGB))
    }

    /// 绿点判定：Theme.ok sRGB(0.196,0.816,0.455) 绿主导且蓝显著低于绿——
    /// 与品牌色区分（brand 双主题均蓝高于绿），避免主形青蓝误判为绿点。
    private func isProxyDotGreen(_ color: NSColor) -> Bool {
        color.alphaComponent > 0.9
            && color.greenComponent > 0.6
            && color.blueComponent < color.greenComponent - 0.15
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
