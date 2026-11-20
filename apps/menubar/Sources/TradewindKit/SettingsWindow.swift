import AppKit
import SwiftUI

/// 设置窗口页签（M3-plan §11.3 装配；demo side-item 语义：icon+文字+选中态 brand 底）。
/// c2 中间态只装配已交付页（§11.5：未交付页侧栏隐藏）——订阅管理/连接与流量随
/// c3/c4 加入本枚举并接线 pane 视图。
enum SettingsPane: String, CaseIterable, Identifiable, Equatable {
    case general
    case enhance
    case about

    var id: String { rawValue }

    var title: String {
        switch self {
        case .general: return "通用"
        case .enhance: return "增强链"
        case .about: return "关于"
        }
    }

    /// SF Symbol 近似 demo 内联 SVG（订阅=house/连接=wave 形，c3/c4 启用时再补）
    var icon: String {
        switch self {
        case .general: return "gearshape"
        case .enhance: return "link"
        case .about: return "info.circle"
        }
    }

    /// 增强链为 P2 占位页（demo 侧栏同款徽标）
    var badge: String? {
        self == .enhance ? "P2" : nil
    }
}

/// 设置窗口装配（M3-plan §11.2-2）：命令式 NSWindow 懒创建（App 层持单例），
/// 780×540 对齐 demo .window，styleMask 含 closable/titled/miniaturizable
/// 不含 resizable/fullScreen；关闭 = orderOut 保活（复开快）。
/// 打开动作 = makeKeyAndOrderFront + NSApp.activate(ignoringOtherApps:)（accessory
/// 应用惯例）；收 popover 由 App 层先于本动作执行（§11.2-2，全局监视器不收本进程点击）。
@MainActor
final class SettingsWindowController: NSWindowController, NSWindowDelegate {
    /// 窗口内容尺寸（demo .window 780×540）
    static let windowSize = NSSize(width: 780, height: 540)

    private let model: SettingsModel

    init(model: SettingsModel) {
        self.model = model
        let window = NSWindow(
            contentRect: NSRect(origin: .zero, size: Self.windowSize),
            styleMask: [.titled, .closable, .miniaturizable],
            backing: .buffered,
            defer: false
        )
        window.title = "Tradewind"
        // WHY isReleasedWhenClosed=false：程序化创建的 NSWindow 默认红点即销毁窗口，
        // 与"关闭=orderOut 保活（复开快）"契约冲突
        window.isReleasedWhenClosed = false
        // WHY NSHostingView 而非 NSHostingController：后者按 fittingSize 级联改写窗口
        // 尺寸，与 780×540 固定非 resizable 窗口冲突（fixed 尺寸应只由 styleMask+初始
        // contentRect 决定；SwiftUI 根视图同帧值双保险）
        window.contentView = NSHostingView(rootView: ShellSettingsView(model: model))
        super.init(window: window)
        window.delegate = self
    }

    required dynamic init?(coder: NSCoder) {
        fatalError("init(coder:) unsupported") // 仅程序化装配，无 nib 反序列化路径
    }

    /// 打开设置窗口并刷新初始值。WHY 不在此收 popover：popover 归 App 层所有，
    /// 由其 openSettings 先行收起（见 App.swift）。
    func showSettings() {
        guard let window else { return }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        Task { await model.load() }
    }

    // MARK: 关闭 = orderOut 保活（§11.2-2）

    /// 拦截真正 close：仅 orderOut 隐藏，窗口与控制器保持存活（复开零重建）。
    func windowShouldClose(_ sender: NSWindow) -> Bool {
        sender.orderOut(nil)
        return false
    }
}

/// 窗口内容（SwiftUI）：HStack[侧栏 198pt, 分隔线, 内容区]（demo .win-body）。
/// 侧栏 198pt 对齐 demo .sidebar；选中态 brand 底白字（demo side-item.on）。
struct ShellSettingsView: View {
    @ObservedObject var model: SettingsModel
    @State private var selection: SettingsPane = .general
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Divider().overlay(Theme.muted(scheme).opacity(0.2))
            // 内容区（demo .pane overflow-y:auto）：滚动在pane内层，此处只给满高
            ZStack(alignment: .bottom) {
                paneContent
                toastLayer
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(width: SettingsWindowController.windowSize.width,
               height: SettingsWindowController.windowSize.height)
        .background(Theme.popBackground(scheme))
    }

    private var paneContent: some View {
        ScrollView(showsIndicators: true) {
            switch selection {
            case .general: GeneralPane(model: model)
            case .enhance: EnhancePane(profileName: model.currentProfileName)
            case .about: AboutPane(model: model)
            }
        }
    }

    // MARK: 侧栏（demo .sidebar：198px/padding 8/间距 2）

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 2) {
            ForEach(SettingsPane.allCases) { pane in
                sideItem(pane)
            }
            Spacer(minLength: 0)
        }
        .padding(8)
        .frame(width: 198)
        .background(Theme.settingsSideBackground(scheme))
    }

    private func sideItem(_ pane: SettingsPane) -> some View {
        let isSelected = pane == selection
        return Button {
            selection = pane
        } label: {
            HStack(spacing: 9) {
                Image(systemName: pane.icon)
                    .font(.system(size: 13))
                Text(pane.title)
                    .font(.system(size: 13))
                if let badge = pane.badge {
                    Spacer(minLength: 8)
                    BadgeView(text: badge)
                }
            }
            .padding(.vertical, 7)
            .padding(.horizontal, 10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(
                RoundedRectangle(cornerRadius: 7)
                    .fill(isSelected ? AnyShapeStyle(Theme.brand(scheme)) : AnyShapeStyle(.clear))
            )
            .foregroundStyle(isSelected ? Color.white : Theme.secondary(scheme))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(pane.title)
        .accessibilityAddTraits(isSelected ? [.isSelected] : [])
    }

    // MARK: toast（写操作结果提示；PopoverView.ToastView 复用，不参与命中测试）

    @ViewBuilder
    private var toastLayer: some View {
        if let toast = model.toast {
            ToastView(text: toast)
        }
    }
}
