import AppKit
import SwiftUI

/// 菜单栏图标三态（demo renderStatus 事实，M2-plan §5）：
/// - active：运行且有接入路径（系统代理或 TUN 至少一开）→ 品牌色
/// - idle：  运行但无接入 → 灰（demo：`icon off = !running || !sysProxy`）
/// - stopped：内核停止 → 灰（速率文本由调用方隐藏）
enum StatusIconState: Equatable {
    case active
    case idle
    case stopped
}

/// 图标状态推导与图像构造（纯逻辑可单测；SF Symbols "wind" 信风形，三态同形异色）。
enum StatusIcon {
    /// demo 口径：停止 → stopped；运行且（系统代理 或 TUN）→ active；否则 idle。
    static func state(running: Bool, intake: Bool) -> StatusIconState {
        if !running {
            return .stopped
        }
        return intake ? .active : .idle
    }

    /// 信风形符号名。WHY 非 template image：demo 品牌色分主题双值（§5.5 UI 真相源），
    /// template 只能渲染系统标签色无法表达品牌色；深浅色由 Theme 分主题着色达成。
    static func symbolName(_ state: StatusIconState) -> String {
        "wind"
    }

    /// 着色（appearance 感知）：active=品牌色，idle/stopped=muted 灰。
    static func tint(_ state: StatusIconState, scheme: ColorScheme) -> NSColor {
        switch state {
        case .active:
            return NSColor(Theme.brand(scheme))
        case .idle, .stopped:
            return NSColor(Theme.muted(scheme))
        }
    }

    /// 构造 17pt 状态栏图像（demo .tw-icon 17px 规范）。
    static func image(_ state: StatusIconState) -> NSImage? {
        let config = NSImage.SymbolConfiguration(pointSize: 17, weight: .regular)
        return NSImage(systemSymbolName: symbolName(state), accessibilityDescription: "Tradewind")?
            .withSymbolConfiguration(config)
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
