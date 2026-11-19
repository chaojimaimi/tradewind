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
    /// 周期刷新（M2-plan §8：壳每次操作前探测；v0.4.6 起随 popover 开合变频）
    private var refreshTimer: Timer?

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        popover.behavior = .transient // 系统行为：点击面板外自动收起
        popover.contentViewController = NSHostingController(rootView: ShellRootView(model: model))
        popover.delegate = self
        setupStatusItem()
        observeModel()
        rescheduleRefresh() // 初值 30s：启动时 popover 未展开
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
        // WHY v0.4.7 乐观值（方案 B）也驱动图标：pending 在动作发起即翻转，
        // 不订阅它们图标要等 CLI+refresh 完成才变（"随点击立即翻转"落空）；
        // 权威值落地后的收敛由上面的 $report 订阅承担
        model.$pendingSysProxy
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.refreshIcon() }
            .store(in: &cancellables)
        model.$pendingRunning
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.refreshIcon() }
            .store(in: &cancellables)
    }

    // WHY v0.4.5（2026-11-19 第四轮反馈）：菜单栏速率文本降级为默认关闭——运行态横向
    // 速率文本占宽且随数值宽度抖动（"↓42.0 MB/s ↑0.8 MB/s"），用户反馈干扰菜单栏；
    // 速率信息 popover 速率卡已有，title 恒空只留图标（三态着色不变）。
    // RateText.statusItem 机制保留勿删：批②设置窗口将其转为 opt-in 设置项，届时在
    // 此处接设置开关恢复 title 刷新（曾用的 traffic.$down 订阅已随 updateRateText 移除）。

    /// WHY v0.4.6（2026-11-19 拍板 A+C 的 C）随 popover 开合变频：async 化后主线程已不阻塞，
    /// 降频进一步省 CPU 与子进程探测次数——展开态 5s 供面板/图标保持新鲜，收起态每 30s
    /// 一次 status 探测足够维持图标新鲜（速率本就由 WS 推送，不依赖该 timer）。
    private func rescheduleRefresh() {
        refreshTimer?.invalidate()
        let interval: TimeInterval = popover.isShown ? 5 : 30
        refreshTimer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.model.refresh() }
        }
    }

    // MARK: 图标四态（v0.4.6）

    /// 图标刷新唯一写入点：合成图像（含代理绿点）+ 淡化 + 三态可辨 tooltip。
    /// WHY v0.4.6：色与形之外补 tooltip 文案兜底，悬停即可知运行/接入/停止。
    /// 合成图为非 template（主形色已离屏渐变烘焙，v0.4.13 双色），不设 contentTintColor
    /// ——对非 template 图是 no-op 死赋值（code-reviewer P3）。
    private func refreshIcon() {
        guard let button = statusItem?.button else { return }
        let scheme = Theme.currentScheme()
        let state = model.iconState // effective 系推导（v0.4.7：乐观值优先，点击即翻转）
        button.image = StatusIcon.image(state, scheme: scheme)
        button.alphaValue = StatusIcon.alpha(state)
        button.toolTip = statusTooltip(state)
    }

    /// tooltip 三态文案：active=代理开 / idle=代理关 / stopped，悬停可辨（v0.4.6）。
    private func statusTooltip(_ state: StatusIconState) -> String {
        switch state {
        case .active: return "Tradewind · 运行中（系统代理已开启）"
        case .idle: return "Tradewind · 运行中（系统代理已关闭）"
        case .stopped: return "Tradewind · 已停止"
        }
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

    /// 展开态 5s 高频刷新挂此而非 willShow（code-reviewer P1 实测：willShow 时
    /// popover.isShown 仍为 false，rescheduleRefresh 会误挂 30s——展开高频从未生效）
    func popoverDidShow(_ notification: Notification) {
        rescheduleRefresh()
    }

    func popoverDidClose(_ notification: Notification) {
        if let monitor = outsideClickMonitor {
            NSEvent.removeMonitor(monitor)
            outsideClickMonitor = nil
        }
        rescheduleRefresh() // 收起态 30s 低频
    }
}
