// Tradewind 菜单栏壳装配（M2-plan §5/§3 批②）：NSStatusItem + NSPopover。
// LSUIElement 由 .app bundle 的 Info.plist 声明；运行期 accessory 兜底（swift run 直启场景）。
// 壳是观察者：退出壳不停内核（内核由 tradewind start 进程守护）。
import AppKit
import Combine
import SwiftUI

/// 应用装配入口（供 TradewindShell 可执行调用；跨模块需 public）。
public enum TradewindApp {
    public static func run() {
        let app = NSApplication.shared
        let delegate = MainActor.assumeIsolated { AppDelegate() }
        app.delegate = delegate
        app.run()
    }
}

/// WHY @MainActor：AppKit 回调与 PanelModel/TrafficStore 同处主 actor，编译器据此放行隔离访问。
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem?
    /// popover 展开期间的全局点击监听（收起兜底，见 popoverWillShow）
    private var outsideClickMonitor: Any?
    private let popover = NSPopover()
    /// 面板状态与 WS 速率（壳的数据中枢；菜单栏图标与 popover 速率卡同源于此）
    private lazy var model: PanelModel = .live()
    private var cancellables = Set<AnyCancellable>()
    /// 周期刷新（M2-plan §8：壳每次操作前探测；5s 供图标保持新鲜）
    private var refreshTimer: Timer?

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        popover.behavior = .transient // 系统行为：点击面板外自动收起
        popover.contentViewController = NSHostingController(rootView: ShellRootView(model: model))
        popover.delegate = self
        setupStatusItem()
        observeModel()
        startRefreshTimer()
        Task { await model.refresh() }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false // 仅状态栏驻留，无窗口
    }

    func applicationWillTerminate(_ notification: Notification) {
        model.traffic.persistNow() // 今日流量落盘（壳内累计的收尾）
    }

    // MARK: 状态栏装配

    private func setupStatusItem() {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        guard let button = item.button else {
            fatalError("app: status item button unavailable") // 环境损坏，静默继续无意义
        }
        button.target = self
        button.action = #selector(statusItemClicked(_:))
        // WHY 扩展 sendAction 掩码：状态项按钮默认仅左键触发 action——不扩展则右键
        // 事件永远到不了 statusItemClicked，Quit 菜单入口失效（M2 真机验收缺陷）
        button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        item.menu = nil // 事件全走 action：左键 popover / 右键菜单
        statusItem = item
        refreshIcon()
    }

    @objc private func statusItemClicked(_ sender: NSStatusBarButton) {
        guard let event = NSApp.currentEvent else { return }
        // 右键（或 control+左键）→ Quit 菜单（退壳不停内核，M2-plan §5 v1.1）
        if event.type == .rightMouseUp || (event.type == .leftMouseUp && event.modifierFlags.contains(.control)) {
            if popover.isShown { popover.performClose(sender) } // code-reviewer LOW：避免菜单下悬挂
            showQuitMenu(sender)
            return
        }
        if popover.isShown {
            popover.performClose(sender)
        } else {
            popover.show(relativeTo: sender.bounds, of: sender, preferredEdge: .minY)
            model.notifyPopoverShown()
        }
    }

    private func showQuitMenu(_ sender: NSStatusBarButton) {
        let menu = NSMenu()
        let quit = NSMenuItem(title: "退出壳（内核继续运行）", action: #selector(quitShell), keyEquivalent: "q")
        quit.target = self
        menu.addItem(quit)
        statusItem?.menu = menu
        // WHY 借道 item.menu：NSStatusItem 弹出系统菜单需要 menu 归属；弹出后立即还原 nil
        statusItem?.button?.performClick(sender)
        DispatchQueue.main.async { [weak self] in self?.statusItem?.menu = nil }
    }

    @objc private func quitShell() {
        NSApp.terminate(nil) // 壳退出即终止进程；内核由 start 进程守护，不受影响
    }

    // MARK: 状态观察（图标三态）

    private func observeModel() {
        model.$report
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.refreshIcon() }
            .store(in: &cancellables)
    }

    // WHY v0.4.5（2026-11-19 第四轮反馈）：菜单栏速率文本降级为默认关闭——运行态横向
    // 速率文本占宽且随数值宽度抖动（"↓42.0 MB/s ↑0.8 MB/s"），用户反馈干扰菜单栏；
    // 速率信息 popover 速率卡已有，title 恒空只留图标（三态着色不变）。
    // RateText.statusItem 机制保留勿删：批②设置窗口将其转为 opt-in 设置项，届时在
    // 此处接设置开关恢复 title 刷新（曾用的 traffic.$down 订阅已随 updateRateText 移除）。

    private func startRefreshTimer() {
        refreshTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.model.refresh() }
        }
    }

    // MARK: 图标三态

    private func refreshIcon() {
        guard let button = statusItem?.button else { return }
        let scheme = Theme.currentScheme()
        button.image = StatusIcon.image(model.iconState)
        button.contentTintColor = StatusIcon.tint(model.iconState, scheme: scheme)
    }
}

/// popover 生命周期：展示时刷新面板数据 + 全局收起兜底（与周期 timer 双保险）。
extension AppDelegate: NSPopoverDelegate {
    func popoverWillShow(_ notification: Notification) {
        model.notifyPopoverShown()
        // WHY 全局监听兜底：.transient 在 accessory（LSUIElement）应用下的自动收起不可靠——
        // 点击桌面/其他状态项不触发 resignKey，popover 悬挂不收（M2 真机验收缺陷）。
        // 显式监听进程外点击保证交互契约"点击外部即收起"；全局监听不接收本进程事件，
        // 状态项 toggle 与 popover 内部点击不受影响。
        outsideClickMonitor = NSEvent.addGlobalMonitorForEvents(
            matching: [.leftMouseDown, .rightMouseDown]
        ) { [weak self] _ in
            DispatchQueue.main.async { self?.popover.performClose(nil) }
        }
    }

    func popoverDidClose(_ notification: Notification) {
        if let monitor = outsideClickMonitor {
            NSEvent.removeMonitor(monitor)
            outsideClickMonitor = nil
        }
    }
}
