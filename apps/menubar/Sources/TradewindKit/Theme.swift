import AppKit
import SwiftUI

/// demo 设计 tokens 的 Swift 移植（docs/ui-demo.html 内置"设计规范"面板，对照口径 M2-plan §5.5：
/// 布局/语义/tokens 对齐，材质为 AppKit 原生近似）。
///
/// 色彩：brand 双主题（dark hsl(191,85%,58%) / light hsl(191,82%,38%)）——
/// HSL→RGB 已预计算，避免用 HSB 近似引入漂移；ok/warn/bad 两主题同值（demo 事实）。
/// 批①.5 §3.2：底色四 token 以 CGFloat RGBA 常量暴露（可单测），Color 由常量构造
/// ——SwiftUI Color 无分量自省 API，单测断言常量而非 Color；不透明值近似 demo
/// 半透明+blur 的合成结果（偏差 #1：色值对齐、质感让渡，无实时桌面 blur）。
enum Theme {
    /// RGBA 四元组（sRGB）：底色常量的承载形式，双主题各一（测试直读）
    typealias RGBA = (red: CGFloat, green: CGFloat, blue: CGFloat, alpha: CGFloat)

    // MARK: 色彩 tokens

    /// 信风青蓝品牌色（选中态/主按钮/图表下行/菜单栏图标运行态）
    static func brand(_ scheme: ColorScheme) -> Color {
        scheme == .light
            ? Color(.sRGB, red: 0.068_4, green: 0.577_5, blue: 0.691_6)
            : Color(.sRGB, red: 0.223_1, green: 0.806_2, blue: 0.936_9)
    }

    /// 成功绿（状态点/系统代理开启）
    static let ok = Color(.sRGB, red: 0x32 / 255.0, green: 0xD0 / 255.0, blue: 0x74 / 255.0)
    /// 警告黄（延迟中档/上行曲线）
    static let warn = Color(.sRGB, red: 0xFF / 255.0, green: 0xB3 / 255.0, blue: 0x40 / 255.0)
    /// 危险红（延迟高档/退出）
    static let bad = Color(.sRGB, red: 0xFF / 255.0, green: 0x60 / 255.0, blue: 0x58 / 255.0)

    /// 灰（txt-3）：dark rgba(235,240,245,.38) / light rgba(29,37,48,.4)——停止/无接入态图标
    static func muted(_ scheme: ColorScheme) -> Color {
        scheme == .light
            ? Color(.sRGB, red: 0x1D / 255.0, green: 0x25 / 255.0, blue: 0x30 / 255.0, opacity: 0.4)
            : Color(.sRGB, red: 0xEB / 255.0, green: 0xF0 / 255.0, blue: 0xF5 / 255.0, opacity: 0.38)
    }

    /// 次级文本（txt-2）
    static func secondary(_ scheme: ColorScheme) -> Color {
        scheme == .light
            ? Color(.sRGB, red: 0x1D / 255.0, green: 0x25 / 255.0, blue: 0x30 / 255.0, opacity: 0.62)
            : Color(.sRGB, red: 0xEB / 255.0, green: 0xF0 / 255.0, blue: 0xF5 / 255.0, opacity: 0.62)
    }

    // MARK: 底色 tokens（demo 三层色阶严格分离禁止混并：--mat-pop / --mat-card / --mat-card-2 + 选中钮）

    /// popover 底 ≈ --mat-pop 的 blur 合成结果（light rgba(252,253,254,.72)+blur(42px)
    /// saturate(1.8) 观感=冷白近实心；dark rgba(40,44,50,.66)），均取不透明色
    static let popBackgroundLightRGBA: RGBA = (0xF2 / 255.0, 0xF6 / 255.0, 0xF8 / 255.0, 1) // #F2F6F8
    static let popBackgroundDarkRGBA: RGBA = (0x26 / 255.0, 0x29 / 255.0, 0x2E / 255.0, 1)  // #26292E

    /// 卡片底（--mat-card）：light 白 α0.92 / dark 白 α0.055。
    /// light 取 .92 而非字面 .66 的推导：demo 卡片是 .66 白叠在"blur 深化的冷底"上
    /// （合成 ≈ RGB 251,252,253），换算为不透明冷白 #F2F6F8 上叠 .92 白合成 ≈ RGB
    /// 254,254,254——两者差约 3 个 RGB 点内，观感等价
    static let cardBackgroundLightRGBA: RGBA = (1, 1, 1, 0.92)
    static let cardBackgroundDarkRGBA: RGBA = (1, 1, 1, 0.055)

    /// 分段容器/foot 按钮底（--mat-card-2）：light 白 α0.92 / dark 白 α0.09（demo 字面值）
    static let controlBackgroundLightRGBA: RGBA = (1, 1, 1, 0.92)
    static let controlBackgroundDarkRGBA: RGBA = (1, 1, 1, 0.09)

    /// 分段选中钮（第三层）：light #FFFFFF 不透明+浮起阴影 / dark 白 α0.16
    /// （必须高于容器 .09，dark 选中态辨识度的关键）
    static let controlSelectedLightRGBA: RGBA = (1, 1, 1, 1)
    static let controlSelectedDarkRGBA: RGBA = (1, 1, 1, 0.16)

    /// popover 底色（demo --mat-pop 合成近似；NSPopover 自身圆角裁切内容，无需自绘圆角）
    static func popBackground(_ scheme: ColorScheme) -> Color {
        color(scheme == .light ? popBackgroundLightRGBA : popBackgroundDarkRGBA)
    }

    /// 卡片底色（demo --mat-card）
    static func cardBackground(_ scheme: ColorScheme) -> Color {
        color(scheme == .light ? cardBackgroundLightRGBA : cardBackgroundDarkRGBA)
    }

    /// 分段容器/foot 按钮底色（demo --mat-card-2）
    static func controlBackground(_ scheme: ColorScheme) -> Color {
        color(scheme == .light ? controlBackgroundLightRGBA : controlBackgroundDarkRGBA)
    }

    /// 分段选中钮底色（demo 第三层色阶）
    static func controlSelected(_ scheme: ColorScheme) -> Color {
        color(scheme == .light ? controlSelectedLightRGBA : controlSelectedDarkRGBA)
    }

    /// Color 统一由 RGBA 常量构造（sRGB）
    private static func color(_ rgba: RGBA) -> Color {
        Color(.sRGB, red: rgba.red, green: rgba.green, blue: rgba.blue, opacity: rgba.alpha)
    }

    // MARK: 几何 tokens（4pt 网格 + demo 圆角）

    static let radiusPopover: CGFloat = 12
    static let radiusCard: CGFloat = 10
    static let radiusControl: CGFloat = 6
    /// 分段容器圆角（demo .seg border-radius 8，ui-demo.html:102；6 是按钮/foot 圆角 --r-ctl）
    static let radiusContainer: CGFloat = 8
    static let popoverWidth: CGFloat = 348

    /// 4pt 网格间距（demo 规范：4/8/12/16/20），按下标取用
    static let spacing: [CGFloat] = [4, 8, 12, 16, 20]

    // MARK: 字号 tokens（demo --fs-*）

    static let fontSizeTitle: CGFloat = 15
    static let fontSizeBody: CGFloat = 13
    static let fontSizeSub: CGFloat = 11

    // MARK: 深浅色感知

    /// 当前系统外观 → ColorScheme（菜单栏图标着色用；SwiftUI 内优先 @Environment）
    static func currentScheme() -> ColorScheme {
        let match = NSApp.effectiveAppearance.bestMatch(from: [.aqua, .darkAqua])
        return match == .darkAqua ? .dark : .light
    }
}
