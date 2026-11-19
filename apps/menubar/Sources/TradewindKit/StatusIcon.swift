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

/// 图标状态推导与图像构造（纯逻辑可单测；v0.4.10 起主形源为品牌位图 tradewind-menubar）。
/// WHY v0.4.6（2026-11-19 用户拍板）四态可视化改造：旧版三态同形异色，idle 与 stopped
/// 同为 muted 灰不可辨，色彩单编码辨识度不足——用户反馈"看不出是否在运行/代理是否开"。
/// 改为"主形跟运行态走（运行=品牌色、停止=muted 灰）、绿点跟代理走（仅 active 显示）"，
/// 停止态再叠加 0.5 alpha 淡化，运行/接入/停止三维正交可辨。
/// WHY v0.4.10（2026-11-19 拍板 logo 资产接入）：主形从 SF Symbols "wind" 换为品牌
/// 位图（16/@1x + 32/@2x 两份 PNG）。v0.4.12 起合成形态为"圆角底色板 + 标形镂空"
/// 徽章（用户反馈裸标形丢失了品牌底板，见 image 处 WHY）；绿点合成机制不变。
enum StatusIcon {
    /// demo 口径：停止 → stopped；运行且（系统代理 或 TUN）→ active；否则 idle。
    static func state(running: Bool, intake: Bool) -> StatusIconState {
        if !running {
            return .stopped
        }
        return intake ? .active : .idle
    }

    /// 品牌位图源：Bundle.module 加载 16/@1x + 32/@2x 两份 PNG，addRepresentation
    /// 组成 16×16 point 图像（Retina 绘制自动取 32px 表示）。
    /// WHY 两份表示同挂 16pt 逻辑尺寸：菜单栏按 point 取图，高分屏不失真。
    static func brandImage() -> NSImage? {
        let sources = [("tradewind-menubar-16", 16), ("tradewind-menubar-32", 32)]
        let image = NSImage(size: NSSize(width: 16, height: 16))
        image.accessibilityDescription = "Tradewind"
        for (name, pixels) in sources {
            // WHY 不带 subdirectory：SPM 将资源平铺进 bundle 的 Contents/Resources/ 根
            guard let url = Bundle.module.url(forResource: name, withExtension: "png"),
                let rep = NSImageRep(contentsOf: url),
                rep.pixelsWide == pixels, rep.pixelsHigh == pixels else { return nil }
            rep.size = NSSize(width: 16, height: 16) // 同一逻辑尺寸，像素数区分 @1x/@2x
            image.addRepresentation(rep)
        }
        return image
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

    /// 16×16 point 品牌徽章 + 右下 6pt 代理绿点的合成状态栏图像。
    /// 纯函数：scheme 参数化、不读全局状态，双状态双主题可单测。
    /// WHY 底色板（v0.4.12，2026-11-19 用户反馈"圆弧状底色板丢失，只剩 T 形标"）：
    /// menubar 预置 PNG 是裸标形（mono SVG 只有 T 形双笔画，squircle 底板只存在于
    /// 应用图标合成）。菜单栏改为"圆角底色板 + T 形标镂空"的徽章形态——与 icns
    /// 同构（icns 是深板亮标双色，菜单栏单色约束下以镂空近似，T 透出菜单栏底色）。
    static func image(_ state: StatusIconState, scheme: ColorScheme) -> NSImage? {
        guard let glyph = brandImage() else { return nil }
        // 画布=主形逻辑尺寸（16×16 point，位图资产 16/32px 双密度），不做额外扩带
        // （code-reviewer P2 口径沿用：扩带令画布被菜单栏等比缩小）；非翻转坐标原点=左下
        let canvasSize = glyph.size
        return NSImage(size: canvasSize, flipped: false) { _ in
            // 底色板：全幅内缩 0.5pt 防菜单栏裁边；圆角半径取超椭圆圆角比 0.29×边长
            // （assets.py squircle n=5 的连续圆角近似，"圆弧状"观感）
            let inset: CGFloat = 0.5
            let plate = NSBezierPath(
                roundedRect: NSRect(origin: .zero, size: canvasSize).insetBy(dx: inset, dy: inset),
                xRadius: (canvasSize.width - inset * 2) * 0.29,
                yRadius: (canvasSize.height - inset * 2) * 0.29)
            tint(state, scheme: scheme).setFill()
            plate.fill()
            // T 形标镂空：destinationOut 以标形 alpha 反乘底板 alpha，横杠竖干区域透出
            // 菜单栏底色。双重叠加=硬镂空（dest×(1-a)²）：16px @1x 资产的横杠仅 ~1px 薄带、
            // 内部 alpha 固有地半透明（≈0.75，实测），单次镂空残留 0.25 会让 1x 密度观感
            // 洇成"没镂干净"；两 pass 后 0.06，与 @2x（内部 1.0，单双 pass 均 0）观感对齐
            for _ in 0..<2 {
                glyph.draw(in: NSRect(origin: .zero, size: canvasSize),
                           from: .zero, operation: .destinationOut, fraction: 1)
            }
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
