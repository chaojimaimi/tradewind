import AppKit
import SwiftUI

/// demo 设计 tokens 的 Swift 移植（docs/ui-demo.html 内置"设计规范"面板，对照口径 M2-plan §5.5：
/// 布局/语义/tokens 对齐，材质为 AppKit 原生近似）。
///
/// 色彩：brand 双主题（dark hsl(191,85%,58%) / light hsl(191,82%,38%)）——
/// HSL→RGB 已预计算，避免用 HSB 近似引入漂移；ok/warn/bad 两主题同值（demo 事实）。
enum Theme {
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

    /// 卡片底（mat-card）：原生 .quaternary 材质近似 demo 半透明白/黑
    static let cardBackground = Color(nsColor: .quaternaryLabelColor)

    // MARK: 几何 tokens（4pt 网格 + demo 圆角）

    static let radiusPopover: CGFloat = 12
    static let radiusCard: CGFloat = 10
    static let radiusControl: CGFloat = 6
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
