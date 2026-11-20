import AppKit
import SwiftUI

/// 设置窗口页签（M3-plan §11.3 装配；demo side-item 语义：icon+文字+选中态 brand 底）。
/// 侧栏序对齐 demo（订阅管理/连接与流量/通用/增强链/关于），五页随批② c2-c4 全量交付。
enum SettingsPane: String, CaseIterable, Identifiable, Equatable {
    case subs
    case conns
    case general
    case enhance
    case about

    var id: String { rawValue }

    var title: String {
        switch self {
        case .subs: return "订阅管理"
        case .conns: return "连接与流量"
        case .general: return "通用"
        case .enhance: return "增强链"
        case .about: return "关于"
        }
    }

    /// SF Symbol 近似 demo 内联 SVG（连接=天线电波，近似 demo 弧+点形态）
    var icon: String {
        switch self {
        case .subs: return "house"
        case .conns: return "antenna.radiowaves.left.and.right"
        case .general: return "gearshape"
        case .enhance: return "link"
        case .about: return "info.circle"
        }
    }

    /// 侧栏徽标（批③ B2 转正后增强链不再是占位页——P2 徽标退役）
    var badge: String? {
        nil
    }
}

/// 设置窗口装配（M3-plan §11.2-2）：命令式 NSWindow 懒创建（App 层持单例），
/// 780×540 对齐 demo .window，styleMask 含 closable/titled/miniaturizable
/// 不含 resizable/fullScreen；关闭 = orderOut 保活（复开快）。
/// 打开动作 = makeKeyAndOrderFront + NSApp.activate(ignoringOtherApps:)（accessory
/// 应用惯例）；收 popover 由 App 层先于本动作执行（§11.2-2，全局监视器不收本进程点击）。
/// §12 c3：窗口可见期切 .regular（Dock 图标现身——黄钮最小化收 Dock + Cmd+Tab），
/// 隐藏期回 .accessory；最小主菜单装配归 App 层（评审 P2-2）。
@MainActor
final class SettingsWindowController: NSWindowController, NSWindowDelegate {
    /// 窗口内容尺寸（demo .window 780×540）
    static let windowSize = NSSize(width: 780, height: 540)

    /// 激活策略翻转唯一承载（§12 c3）：窗口可见期 .regular（Dock 图标现身），
    /// 隐藏期回 .accessory（Dock 图标消失、菜单栏壳如常）。LSUIElement 菜单栏
    /// 工具的标准做法；状态项/popover 在 .regular 下行为不变。
    static func targetActivationPolicy(windowVisible: Bool) -> NSApplication.ActivationPolicy {
        windowVisible ? .regular : .accessory
    }

    private let model: SettingsModel
    private var hasCenteredOnce = false
    private let subsModel: SubsModel
    /// 速率/今日流量源（壳唯一 TrafficStore 实例由 App 注入，面板 popover 与设置页同源）
    private let traffic: TrafficStore
    /// 连接页数据中枢（c4，独立 WS /connections）；生命周期挂 showSettings/orderOut
    private let connectionsStore: ConnectionsStore
    /// 代理守护 toast 模型（批④ §2.3）：窗口打开期轮询 HijackEvents，关闭即停
    private let guardModel: GuardModel
    /// tradewind:// 导入协调器（批④ §2.3）：pendingURL 驱动订阅页确认 alert
    private let importCoordinator: ImportCoordinator

    /// 生产由 App 层注入与 SettingsModel 同源的桥（§11.2-10 架构契约"壳只消费
    /// 机器可读输出"不变）+ 壳级 TrafficStore；单测显式装配（WHY 无默认值：
    /// @MainActor init 的默认参数在非隔离调用点求值，v5 模式下直接编译错误）。
    init(model: SettingsModel, subsModel: SubsModel, traffic: TrafficStore,
         connectionsStore: ConnectionsStore, guardModel: GuardModel,
         importCoordinator: ImportCoordinator) {
        self.model = model
        self.subsModel = subsModel
        self.traffic = traffic
        self.connectionsStore = connectionsStore
        self.guardModel = guardModel
        self.importCoordinator = importCoordinator
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
        window.contentView = NSHostingView(rootView: ShellSettingsView(
            model: model, subsModel: subsModel, traffic: traffic,
            connectionsStore: connectionsStore, guardModel: guardModel,
            importCoordinator: importCoordinator))
        super.init(window: window)
        window.delegate = self
    }

    required dynamic init?(coder: NSCoder) {
        fatalError("init(coder:) unsupported") // 仅程序化装配，无 nib 反序列化路径
    }

    /// 打开设置窗口并刷新初始值（§12 c3 六步序，统一入口——Dock 点击/菜单栏"设置…"
    /// 两路径共用）。WHY 不在此收 popover：popover 归 App 层所有，由其 openSettings
    /// 先行收起（见 App.swift）。
    func showSettings() {
        guard let window else { return }
        // ① 窗口可见期切 .regular：Dock 图标现身（黄钮最小化可收进 Dock）
        NSApp.setActivationPolicy(Self.targetActivationPolicy(windowVisible: true))
        // ② makeKeyAndOrderFront 不还原 miniaturized 窗——Dock 收起后从菜单栏复开、
        // Dock mini 窗红叉关闭后复开两路径同兜（评审 P1-2）
        if window.isMiniaturized { window.deminiaturize(nil) }
        // ③ 仅首开居中（审查 P3-4：orderOut 保活复开应保留用户拖动位置）
        if !hasCenteredOnce {
            window.center()
            hasCenteredOnce = true
        }
        // ④ key 化置前 + ⑤ 激活应用
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        // ⑥ 三路数据刷新
        Task { await model.load() }
        Task { await subsModel.load() } // c3 订阅页数据（list --json）
        Task { await connectionsStore.start() } // c4 连接页 WS（关闭窗口时 stop，§11.2-3）
        guardModel.startPolling() // 批④守护 toast：窗口打开期轮询，关闭即停（窗口层兜底）
    }

    // MARK: 关闭 = orderOut 保活（§11.2-2）

    /// 拦截真正 close：仅 orderOut 隐藏，窗口与控制器保持存活（复开零重建）。
    /// WHY 先 stop 连接 WS：orderOut 不触发 SwiftUI onDisappear，窗口层兜底断流
    /// （避免常驻连接开销，M3-plan §11.6 风险条目；start/stop 幂等，双挂无冲突）。
    /// §12 c3：隐藏即回 .accessory（Dock 图标消失）——orderOut miniaturized 窗时
    /// isMiniaturized 残留 true，showSettings 的 deminiaturize 兜底覆盖。
    func windowShouldClose(_ sender: NSWindow) -> Bool {
        connectionsStore.stop()
        guardModel.stopPolling()
        sender.orderOut(nil)
        NSApp.setActivationPolicy(Self.targetActivationPolicy(windowVisible: false))
        return false
    }
}

/// 窗口内容（SwiftUI）：HStack[侧栏 198pt, 分隔线, 内容区]（demo .win-body）。
/// 侧栏 198pt 对齐 demo .sidebar；选中态 brand 底白字（demo side-item.on）。
struct ShellSettingsView: View {
    @ObservedObject var model: SettingsModel
    @ObservedObject var subsModel: SubsModel
    /// WHY plain let：速率每秒更新只需驱动连接页自身（ConnectionsPane 内
    /// @ObservedObject 观察），壳层不订阅可避免整窗 1Hz 重渲染
    let traffic: TrafficStore
    let connectionsStore: ConnectionsStore
    /// 代理守护 toast 模型（批④ §2.3；App 注入与窗口层生命周期配对）
    let guardModel: GuardModel
    /// tradewind:// 导入协调器（批④ §2.3；App 注入，订阅页据此挂确认 alert）
    let importCoordinator: ImportCoordinator
    /// 增强链页模型（批③ B2 转正：@StateObject 本页装配，App 层装配面不变）
    @StateObject private var enhanceModel = EnhanceModel.live()
    @State private var selection: SettingsPane = .subs // demo 初态：订阅管理高亮（ui-demo.html side-item.on；审查 P3-6 对齐）
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
            case .subs: SubscriptionsPane(model: subsModel, importCoordinator: importCoordinator)
            case .conns: ConnectionsPane(store: connectionsStore, traffic: traffic)
            case .general: GeneralPane(model: model)
            case .enhance: EnhancePane(profileName: model.currentProfileName, model: enhanceModel)
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
        // 批③ B2：增强链页 toast 并入窗口层（enhanceModel 由本页装配）
        if let toast = model.toast ?? subsModel.toast ?? enhanceModel.toast ?? guardModel.toast {
            ToastView(text: toast)
        }
    }
}
