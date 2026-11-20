import AppKit
import Foundation
import Testing
@testable import TradewindKit

/// 关于页品牌资产 + 版本号展示（M3-plan §12 c1）。
@Suite struct AboutBrandTests {
    /// 品牌位图资源可寻址且为 256×256（icns icon_128x128@2x 派生，与 Dock 图标同构；
    /// AboutPane 以 Bundle.module.url + NSImage(contentsOf:) 显式加载——m3b2 返修：
    /// SwiftUI Image(name, bundle:) 不解析散装 PNG）
    @Test func aboutIconAssetLoadsAt256Pixels() throws {
        // WHY 不带 subdirectory：SPM 将资源平铺进 bundle 的 Contents/Resources/ 根
        let url = try #require(Bundle.module.url(forResource: "tradewind-about-icon", withExtension: "png"))
        let rep = try #require(NSImageRep(contentsOf: url))
        #expect(rep.pixelsWide == 256)
        #expect(rep.pixelsHigh == 256)
    }

    /// ShellVersion.display 剥单个前导 "v"（F1a "vv0.5.0-m3b1" 双写根因）：
    /// 版本值剥、dev/纯哈希原样（评审 P3：仅 v 后随数字才剥）
    @Test func shellVersionDisplayStripsSingleLeadingVOnly() {
        #expect(ShellVersion.display("v0.5.0-m3b1") == "0.5.0-m3b1")
        #expect(ShellVersion.display("dev") == "dev")
        #expect(ShellVersion.display("") == "")
        #expect(ShellVersion.display("v1.2.3") == "1.2.3")
    }
}
