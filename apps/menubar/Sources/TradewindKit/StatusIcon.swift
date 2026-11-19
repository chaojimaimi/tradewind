import AppKit
import SwiftUI

/// 菜单栏图标三态（demo renderStatus 事实，M2-plan §5）：
/// - active：运行且有接入路径（系统代理或 TUN 至少一开）→ 品牌色 + 代理绿点
/// - idle：  运行但无接入 → 品牌色、无绿点
/// - stopped：内核停止 → muted 灰 + 整体淡化（速率文本由调用方隐藏）
enum StatusIconState: Equatable {
    case active
    case idle
    case stopped
}

/// 图标状态推导与图像构造（纯逻辑可单测；SF Symbols "wind" 信风形）。
/// WHY v0.4.6（2026-11-19 用户拍板）四态可视化改造：旧版三态同形异色，idle 与 stopped
/// 同为 muted 灰不可辨，色彩单编码辨识度不足——用户反馈"看不出是否在运行/代理是否开"。
/// 改为"主形跟运行态走（运行=品牌色、停止=muted 灰）、绿点跟代理走（仅 active 显示）"，
/// 停止态再叠加 0.5 alpha 淡化，运行/接入/停止三维正交可辨。
enum StatusIcon {
    /// demo 口径：停止 → stopped；运行且（系统代理 或 TUN）→ active；否则 idle。
    static func state(running: Bool, intake: Bool) -> StatusIconState {
        if !running {
            return .stopped
        }
        return intake ? .active : .idle
    }

    /// 信风形符号名。WHY 合成画布非 template：绿点须以独立色彩烘焙（template 画布会被
    /// contentTintColor 单色染色，"绿点跟代理走"失效），主形品牌色同样烘焙进画布，
    /// 深浅色由 Theme 分主题着色达成。
    static func symbolName(_ state: StatusIconState) -> String {
        "wind"
    }

    /// 着色（appearance 感知）v0.4.6：运行态（active/idle）=品牌色，仅 stopped=muted 灰
    /// ——主形编码"是否在运行"，"代理是否开"交给绿点编码。
    static func tint(_ state: StatusIconState, scheme: ColorScheme) -> NSColor {
        switch state {
        case .active, .idle:
            return NSColor(Theme.brand(scheme))
        case .stopped:
            return NSColor(Theme.muted(scheme))
        }
    }

    /// 代理绿点仅"运行且有接入"显示（"绿点跟代理走"，v0.4.6）。
    static func showsProxyDot(_ state: StatusIconState) -> Bool {
        state == .active
    }

    /// 停止态整体淡化（App 层设 button.alphaValue）：与运行态拉开第二视觉维度。
    static func alpha(_ state: StatusIconState) -> CGFloat {
        state == .stopped ? 0.5 : 1
    }

    /// 17pt 主形 + 右下 6pt 代理绿点的合成状态栏图像（demo .tw-icon 17px 规范）。
    /// 纯函数：scheme 参数化、不读全局状态，双状态双主题可单测。
    static func image(_ state: StatusIconState, scheme: ColorScheme) -> NSImage? {
        let config = NSImage.SymbolConfiguration(pointSize: 17, weight: .regular)
        guard let glyph = NSImage(systemSymbolName: symbolName(state), accessibilityDescription: "Tradewind")?
            .withSymbolConfiguration(config) else { return nil }
        // 画布=主形实测尺寸（21×18 @17pt），不做额外扩带（code-reviewer P2：+6 扩带在
        // 非翻转坐标里位于顶部且恒空，还令 24pt 高图被菜单栏等比缩小低于 17px 规范）；
        // 主形左下对齐（非翻转坐标原点=左下），绿点落主形自身右下空白角（像素级实测无碰撞）
        let canvasSize = NSSize(width: max(glyph.size.width, 21), height: glyph.size.height)
        return NSImage(size: canvasSize, flipped: false) { _ in
            glyph.draw(in: NSRect(origin: .zero, size: glyph.size))
            // 模板符号无自带色（draw 出为黑剪影）：sourceAtop 只染已有像素，烘焙主形色
            tint(state, scheme: scheme).setFill()
            NSRect(origin: .zero, size: canvasSize).fill(using: .sourceAtop)
            guard showsProxyDot(state) else { return true }
            drawProxyDot(in: canvasSize)
            return true
        }
    }

    /// 右下 6pt 绿点：Theme.ok 填充 + 1pt primary 20% 描边环（深浅底色下与主形柔和解耦）。
    /// WHY v0.4.7（2026-11-19 用户反馈角标是"绿色小方点"）：NSRect.fill() 填充的是矩形，
    /// 仅描边环用 oval——实心点与圆环形状不一致；改 ovalIn 填充，点环同为圆形。
    private static func drawProxyDot(in canvasSize: NSSize) {
        let dot = NSRect(x: canvasSize.width - 7, y: 1, width: 6, height: 6)
        NSColor(Theme.ok).setFill()
        NSBezierPath(ovalIn: dot).fill()
        let ring = NSBezierPath(ovalIn: dot.insetBy(dx: -0.5, dy: -0.5))
        ring.lineWidth = 1
        NSColor(Color.primary.opacity(0.2)).setStroke()
        ring.stroke()
    }
}

/// 菜单栏速率文本（demo `↓x ↑y` 形态；运行态显示，停止态隐藏）。
enum RateText {
    /// 人类可读速率：>=1e9 GB、>=1e6 MB、>=1e3 KB、否则 B（demo fmtB 口径）。
    static func bytes(_ value: Double) -> String {
        switch value {
        case 1e9...: return String(format: "%.1f GB", value / 1e9)
        case 1e6...: return String(format: "%.1f MB", value / 1e6)
        case 1e3...: return String(format: "%.0f KB", value / 1e3)
        default: return String(format: "%.0f B", value)
        }
    }

    /// status item 完整文本："↓42.0 MB/s ↑0.8 MB/s"；nil 表示停止态（隐藏文本）。
    static func statusItem(down: Double?, up: Double?) -> String? {
        guard let down, let up else { return nil }
        return "↓\(bytes(down))/s ↑\(bytes(up))/s"
    }
}
