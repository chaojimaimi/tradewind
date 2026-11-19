import Testing
@testable import TradewindKit

/// Theme 底色四 token：RGBA 常量断言（批①.5 §3.2——SwiftUI Color 无分量自省 API，
/// 只断言常量；三层色阶 pop/card/control/controlSelected 严格分离禁止混并）。
@Suite struct ThemeColorTokenTests {
    @Test func popBackgroundIsOpaqueColdTone() {
        // ≈ --mat-pop blur 合成结果：light 冷白 #F2F6F8 / dark 冷黑 #26292E，均不透明
        #expect(Theme.popBackgroundLightRGBA == (0xF2 / 255.0, 0xF6 / 255.0, 0xF8 / 255.0, 1))
        #expect(Theme.popBackgroundDarkRGBA == (0x26 / 255.0, 0x29 / 255.0, 0x2E / 255.0, 1))
    }

    @Test func cardBackgroundFollowsDemoMatCard() {
        // --mat-card：light 白 α0.92（.66 白叠 blur 冷底的合成等价，推导见 Theme 注释）
        // / dark 白 α0.055
        #expect(Theme.cardBackgroundLightRGBA == (1, 1, 1, 0.92))
        #expect(Theme.cardBackgroundDarkRGBA == (1, 1, 1, 0.055))
    }

    @Test func controlBackgroundAndSelectedKeepLayerSeparation() {
        // --mat-card-2：light 白 α0.92 / dark 白 α0.09；选中钮第三层 light 不透明白 /
        // dark 白 α0.16——必须高于容器 .09（dark 选中态辨识度关键）
        #expect(Theme.controlBackgroundLightRGBA == (1, 1, 1, 0.92))
        #expect(Theme.controlBackgroundDarkRGBA == (1, 1, 1, 0.09))
        #expect(Theme.controlSelectedLightRGBA == (1, 1, 1, 1))
        #expect(Theme.controlSelectedDarkRGBA == (1, 1, 1, 0.16))
        #expect(Theme.controlSelectedDarkRGBA.alpha > Theme.controlBackgroundDarkRGBA.alpha)
    }

    @Test func containerRadiusMatchesDemoSegCorner() {
        // demo .seg 容器圆角 8（ui-demo.html:102）；6 是按钮/foot 圆角 --r-ctl
        #expect(Theme.radiusContainer == 8)
    }
}
