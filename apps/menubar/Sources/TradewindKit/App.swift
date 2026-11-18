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
    private let popover = NSPopover()
    /// 面板状态与 WS 速率（壳的数据中枢；菜单栏图标/速率文本同源于此）
    private lazy var model: PanelModel = .live()
    private var cancellables = Set<AnyCancellable>()
    /// 周期刷新（M2-plan §8：壳每次操作前探测；5s 供图标/速率保持新鲜）
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
        item.menu = nil // 事件全走 action：左键 popover / 右键菜单
        statusItem = item
        refreshIcon()
    }

    @objc private func statusItemClicked(_ sender: NSStatusBarButton) {
        guard let event = NSApp.currentEvent else { return }
        // 右键（或 control+左键）→ Quit 菜单（退壳不停内核，M2-plan §5 v1.1）
        if event.type == .rightMouseUp || (event.type == .leftMouseUp && event.modifierFlags.contains(.control)) {
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

    // MARK: 状态观察（图标三态 + 运行态速率文本）

    private func observeModel() {
        model.$report
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.refreshIcon() }
            .store(in: &cancellables)
        model.traffic.$down
            .receive(on: DispatchQueue.main)
            .sink { [weak self] down in self?.updateRateText(down: down) }
            .store(in: &cancellables)
    }

    /// demo 口径：运行态显示速率文本，停止态隐藏。
    private func updateRateText(down: Double) {
        guard let button = statusItem?.button else { return }
        let running = model.running
        button.title = running ? (RateText.statusItem(down: down, up: model.traffic.up) ?? "") : ""
    }

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

/// popover 生命周期：展示时刷新面板数据（与周期 timer 双保险）。
extension AppDelegate: NSPopoverDelegate {
    func popoverWillShow(_ notification: Notification) {
        model.notifyPopoverShown()
    }
}
