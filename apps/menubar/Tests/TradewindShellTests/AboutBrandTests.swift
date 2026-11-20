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

    /// ShellVersion.display 全形契约（批③ v1.2 P2-3 改写）：返回含 "v" 展示形态——
    /// 裸版本号补 v、已带 v 原样、dev 原样、纯哈希原样、空串原样
    @Test func shellVersionDisplayReturnsFullFormWithV() {
        #expect(ShellVersion.display("v0.5.0-m3b1") == "v0.5.0-m3b1")
        #expect(ShellVersion.display("dev") == "dev")
        #expect(ShellVersion.display("") == "")
        #expect(ShellVersion.display("0.5.0") == "v0.5.0")
    }
}
