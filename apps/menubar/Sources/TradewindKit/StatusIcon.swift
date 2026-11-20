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
/// 位图（16/@1x + 32/@2x 两份 PNG）。
/// WHY v0.4.13（2026-11-19 用户复测否决 v0.4.12 单色镂空近似）："T 形标与底色板分别
/// 有不同的颜色"是设计稿的硬性要求——改为设计资产原色的双色烘焙：深墨绿对角渐变
/// 底板 + 亮青渐变标形（取色与方向均来自资产包 assets.py 的 bg/wing 渐变定义），
/// 与 icns/Dock 图标完全同构。菜单栏项非模板图（自绘烘焙），双色成立。
enum StatusIcon {
    /// 画布边长（point）：20（v0.5.0，2026-11-20 用户复测拍板 20pt；v0.4.15 曾 18pt）。
    /// 菜单栏内容区 ~24pt 上探安全；16/32px 资产插值 1.25x，渐变图形观感可接受
    /// （=v0.4.15 审查 P3-4 先例）；绿点贴角为既有形态，出界幅度 20pt ≈0.3-0.7pt@2x
    /// 接受（目验裁量项）。画布/底板内缩/圆角比不变式随边长比例自缩放。
    static let canvasSide: CGFloat = 20

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

    // MARK: 设计资产双色（v0.4.13，色值/方向取自资产包 assets.py defs，严格遵循方案）

    /// 底色板渐变：bg 线性渐变 (110,60)→(940,970)（1024 画布 top-left 坐标），
    /// 换算 AppKit y-up 角度 ≈ -47.6°；三停点 #0D3A4E / #08222E / #040F18。
    private static let plateGradient = NSGradient(
        colors: [srgb("#0D3A4E"), srgb("#08222E"), srgb("#040F18")],
        atLocations: [0, 0.5, 1], colorSpace: .sRGB)!
    /// 标形渐变：wing 线性渐变 (200,470)→(820,330)，y-up 角度 ≈ +12.7°；
    /// 三停点 #2FB6DC / #4FDCE4(.55) / #8EF7DE。
    private static let markGradient = NSGradient(
        colors: [srgb("#2FB6DC"), srgb("#4FDCE4"), srgb("#8EF7DE")],
        atLocations: [0, 0.55, 1], colorSpace: .sRGB)!
    /// 停止态双色灰（语义=muted：结构保留、色彩退灰；整体 0.5 淡化仍由 App 层叠加）。
    private static let stoppedPlateGradient = NSGradient(
        colors: [srgb("#2A2A2C"), srgb("#161618")],
        atLocations: [0, 1], colorSpace: .sRGB)!
    private static let stoppedMarkGradient = NSGradient(
        colors: [srgb("#8E8E93"), srgb("#B8B8BC")],
        atLocations: [0, 1], colorSpace: .sRGB)!
    private static let plateAngle: CGFloat = -47.6
    private static let markAngle: CGFloat = 12.7

    /// #RRGGBB → sRGB（十六进制字面量集中解析，色值与资产定义一字不差）。
    private static func srgb(_ hex: String) -> NSColor {
        var v: UInt64 = 0
        Scanner(string: String(hex.dropFirst())).scanHexInt64(&v)
        return NSColor(srgbRed: CGFloat((v >> 16) & 0xFF) / 255,
                       green: CGFloat((v >> 8) & 0xFF) / 255,
                       blue: CGFloat(v & 0xFF) / 255, alpha: 1)
    }

    /// 标形的渐变填充版：独立离屏画布先铺渐变再 destinationIn 咬合标形 alpha。
    /// WHY 离屏：NSImage(size:flipped:) 直接绘制进当前上下文（无独立缓冲），
    /// destinationIn 若在主画布执行会咬掉已画的底板而非标形。
    private static func gradientFilledMark(_ glyph: NSImage, size: NSSize, gradient: NSGradient) -> NSImage {
        let masked = NSImage(size: size)
        masked.lockFocus()
        gradient.draw(in: NSRect(origin: .zero, size: size), angle: markAngle)
        glyph.draw(in: NSRect(origin: .zero, size: size), from: .zero,
                   operation: .destinationIn, fraction: 1)
        masked.unlockFocus()
        return masked
    }

    /// 代理绿点仅"运行且有接入"显示（"绿点跟代理走"，v0.4.6）。
    static func showsProxyDot(_ state: StatusIconState) -> Bool {
        state == .active
    }

    /// 停止态整体淡化（App 层设 button.alphaValue）：与运行态拉开第二视觉维度。
    static func alpha(_ state: StatusIconState) -> CGFloat {
        state == .stopped ? 0.5 : 1
    }

    /// canvasSide² point 品牌徽章 + 右下 6.5pt 代理绿点的合成状态栏图像。
    /// WHY v0.5.0（2026-11-20 用户复测拍板 20pt，v0.4.15 曾 18pt）：菜单栏内容区
    /// ~24pt 上探安全；16/32px 资产插值 1.25x，渐变图形观感可接受（=v0.4.15 审查
    /// P3-4 先例）。纯函数：不读全局状态，双状态可单测。WHY scheme 参数保留但未用：
    /// 设计资产为固定品牌色（不随系统外观切换，Dock 图标同样恒定）；参数留给未来
    /// 深浅色变体。
    static func image(_ state: StatusIconState, scheme: ColorScheme) -> NSImage? {
        guard let glyph = brandImage() else { return nil }
        // 画布=canvasSide² point（大于资产逻辑尺寸 16pt，glyph 插值铺满）；非翻转坐标原点=左下
        let canvasSize = NSSize(width: canvasSide, height: canvasSide)
        let running = state != .stopped
        let plate = running ? plateGradient : stoppedPlateGradient
        let mark = running ? markGradient : stoppedMarkGradient
        return NSImage(size: canvasSize, flipped: false) { _ in
            // 底色板：全幅内缩 0.5pt 防菜单栏裁边；圆角半径取超椭圆圆角比 0.29×边长
            // （assets.py squircle n=5 的连续圆角近似，"圆弧状"观感）
            let inset: CGFloat = 0.5
            let platePath = NSBezierPath(
                roundedRect: NSRect(origin: .zero, size: canvasSize).insetBy(dx: inset, dy: inset),
                xRadius: (canvasSize.width - inset * 2) * 0.29,
                yRadius: (canvasSize.height - inset * 2) * 0.29)
            plate.draw(in: platePath, angle: plateAngle)
            // 标形：wing 渐变咬合位图 alpha 后叠入（双色；运行/停止换渐变组）
            gradientFilledMark(glyph, size: canvasSize, gradient: mark)
                .draw(in: NSRect(origin: .zero, size: canvasSize))
            guard showsProxyDot(state) else { return true }
            drawProxyDot(in: canvasSize)
            return true
        }
    }

    /// 右下 6.5pt 绿点（20pt 画布下 6→6.5 比例同步，§12 c4）：Theme.ok 填充 +
    /// 1pt primary 20% 描边环（深浅底色下与主形柔和解耦；描边环 inset -0.5 不变）。
    /// WHY v0.4.7（2026-11-19 用户反馈角标是"绿色小方点"）：NSRect.fill() 填充的是矩形，
    /// 仅描边环用 oval——实心点与圆环形状不一致；改 ovalIn 填充，点环同为圆形。
    private static func drawProxyDot(in canvasSize: NSSize) {
        let dot = NSRect(x: canvasSize.width - 7.5, y: 1, width: 6.5, height: 6.5)
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

/// 菜单栏速率 title 判定（批④ P3-1 提纯：原 refreshRateTitle 内联 statusItem 副作用
/// 不可单测——本纯函数只做判定，App 调用点只剩赋值）。
enum RateTitle {
    /// enabled=壳偏好开关（menubar_rate）；running=有效运行态（乐观值口径）。
    /// 返回 nil = 清空 title（开关关/停止态，"热清空不等下条 WS 推送"语义不变）。
    static func forModel(enabled: Bool, running: Bool, down: Double?, up: Double?) -> String? {
        guard enabled, running else { return nil }
        return RateText.statusItem(down: down, up: up)
    }
}

/// 快捷键注册失败 toast 持久化口径（批④ P3-4：失败 toast 不自清——直至下次注册
/// 尝试改写/清除；纯函数可测，App.registerHotKey 只做赋值）。
enum HotKeyToast {
    static let failureMessage = "快捷键 ⌥⇧T 注册失败：可能与其他应用冲突"

    /// 注册尝试结束后的 toast 终态：
    ///   - 失败 → 失败文案（覆盖任何在显消息——失败态必须可见）；
    ///   - 成功 → 既有失败 toast 清除，其他消息（CLI 缺失等）不动。
    static func resolve(afterSuccess: Bool, current: String?) -> String? {
        if afterSuccess {
            return current == failureMessage ? nil : current
        }
        return failureMessage
    }
}
