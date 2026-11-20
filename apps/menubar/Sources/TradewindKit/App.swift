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
    /// 设置窗口懒创建单例（M3 批② c2 §11.2-2）：关闭=orderOut 保活，复开零重建。
    /// WHY 自持独立桥：PanelModel.bridge 私有，设置页读写链路自成一体；
    /// c3 订阅页/通用页与 c4 连接页共用同一条桥（同一 CLI 路径解析，无状态）。
    private lazy var settingsBridge = try? CoreBridge.live()
    /// 连接页数据中枢（c4 §11.2-3）：独立 WS /connections，与 TrafficStore 的
    /// /traffic 并行无冲突；网关地址经同一条桥的 panelURL 解析。
    private lazy var connectionsStore = ConnectionsStore(bridge: settingsBridge)
    /// WHY 显式 Optional 懒创建而非 lazy var（§12 c2 评审 P3 定案）：保懒创建语义
    /// 且 applyAppearance 的 KVO 路径可安全触碰——Optional 链在窗口未创建时直接
    /// 短路，不会因切主题误触发整窗构建。
    private var settingsWindow: SettingsWindowController?

    /// 按原 lazy 闭包同参构造设置窗口（openSettings 唯一触发点）。
    private func makeSettingsWindowIfNeeded() {
        guard settingsWindow == nil else { return }
        settingsWindow = SettingsWindowController(
            model: SettingsModel(bridge: settingsBridge),
            subsModel: SubsModel(bridge: settingsBridge),
            traffic: model.traffic,
            connectionsStore: connectionsStore)
    }
    private var cancellables = Set<AnyCancellable>()
    /// 周期刷新（M2-plan §8：壳每次操作前探测；v0.4.6 起随 popover 开合变频）
    private var refreshTimer: Timer?
    /// 全局启停快捷键 ⌥⇧T（M3 批② c5，M3-plan §11.2-7）；nil=未注册
    /// （hotkey_enabled 关或注册失败），热开关经 prefChangedNotification 动态装卸
    private var hotKey: GlobalHotKey?

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        popover.behavior = .transient // 系统行为：点击面板外自动收起
        // 设置…回调闭包注入（M3 批② c2）：popover 收起在 openSettings 内先行执行
        popover.contentViewController = NSHostingController(
            rootView: ShellRootView(model: model, onOpenSettings: { [weak self] in
                self?.openSettings()
            }))
        popover.delegate = self
        setupStatusItem()
        observeModel()
        observeShellPrefs()
        applyAppearance() // 启动即应用主题偏好（§12 c2；窗口未建时仅 popover 生效）
        installMainMenu() // §12 c3 最小主菜单（.regular 期编辑快捷键依赖）
        rescheduleRefresh() // 初值 30s：启动时 popover 未展开
        Task { await model.refresh() }
        if ShellPrefs.hotkeyEnabled() { registerHotKey() } // 缺省 true（§11.2-7）
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false // 仅状态栏驻留，无窗口
    }

    /// Dock 图标点击 reopen（§12 c3）：设置窗口在 .regular 期收进 Dock 后，点击 Dock
    /// 图标统一走 openSettings（deminiaturize 已内聚于 showSettings）；返回 false 声明
    /// 已处理。.accessory 下无 Dock 图标 reopen 不可达；边缘路径触发时退化为开窗，无害。
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        openSettings()
        return false
    }

    func applicationWillTerminate(_ notification: Notification) {
        model.traffic.persistNow() // 今日流量落盘（壳内累计的收尾）
    }

    deinit {
        // 壳偏好通知解除（observeShellPrefs 注册对；进程退出才 deinit，惯例防御）
        NotificationCenter.default.removeObserver(self)
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
        // 速率 title 字体（M3 批② c5）：monospacedDigit 11px——等宽数字防
        // "↓42.0 MB/s" 逐秒宽度抖动（opt-in title 与 image 共存，图像左、文本右）
        button.font = .monospacedDigitSystemFont(ofSize: 11, weight: .regular)
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

    // MARK: 设置窗口（M3 批② c2）

    /// 打开设置窗口（M3-plan §11.2-2）：**先收 popover** —— outsideClickMonitor 是
    /// 全局监视器不接收本进程事件（popoverDidClose 注释），不收起则 popover 悬挂在
    /// 设置窗口之上；再 key 化窗口并激活应用（accessory 应用惯例）。
    private func openSettings() {
        if popover.isShown {
            popover.performClose(nil)
        }
        makeSettingsWindowIfNeeded()
        settingsWindow?.showSettings()
    }

    // MARK: 最小主菜单（M3-plan §12 c3，评审 P2-2）

    /// LSUIElement 应用开窗经典缺陷：运行期切 .regular 后若无主菜单，⌘C/⌘V/⌘X/⌘A/
    /// ⌘Z/⌘M/⌘W 全部失效（键位事件经主菜单匹配后走 responder chain）。启动装配一次；
    /// .accessory 下主菜单不显示，无副作用。结构最小化：应用（退出壳）+ Edit（标准
    /// 编辑 selector）+ Window（最小化/关闭）。菜单项不设 target——动作由 responder
    /// chain 派发到 key window / first responder（Edit 落输入框、Window 落设置窗口）。
    private func installMainMenu() {
        let mainMenu = NSMenu()
        // 应用菜单：首项空 item 惯例（系统以应用名注入首项标题）；⌘Q 退壳不停内核
        let appMenu = NSMenu()
        appMenu.addItem(NSMenuItem())
        appMenu.addItem(NSMenuItem(title: "退出壳（内核继续运行）",
                                   action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q"))
        let appItem = NSMenuItem()
        appItem.submenu = appMenu
        mainMenu.addItem(appItem)
        // Edit 菜单：标准 responder-chain selector。WHY NSSelectorFromString 而非
        // #selector：undo/redo/cut/copy/paste 无 Swift 暴露的 @objc 声明
        // （NSResponder 无同名成员，#selector 编译失败），Selector 字符串构造又触发
        // "use #selector" 警告——运行时 selector API 是两条约束下的正规途径。
        // 修复 .regular 期全部输入框编辑快捷键
        let editMenu = NSMenu(title: "编辑")
        editMenu.addItem(Self.menuItem("撤销", NSSelectorFromString("undo:"), "z"))
        editMenu.addItem(Self.menuItem("重做", NSSelectorFromString("redo:"), "z",
                                       modifiers: [.command, .shift]))
        editMenu.addItem(NSMenuItem.separator())
        editMenu.addItem(Self.menuItem("剪切", NSSelectorFromString("cut:"), "x"))
        editMenu.addItem(Self.menuItem("拷贝", NSSelectorFromString("copy:"), "c"))
        editMenu.addItem(Self.menuItem("粘贴", NSSelectorFromString("paste:"), "v"))
        editMenu.addItem(Self.menuItem("全选", NSSelectorFromString("selectAll:"), "a"))
        let editItem = NSMenuItem()
        editItem.submenu = editMenu
        mainMenu.addItem(editItem)
        // Window 菜单：设置窗口最小化/关闭快捷键（performMiniaturize:/performClose:
        // 经 responder chain 到 key window）
        let windowMenu = NSMenu(title: "窗口")
        windowMenu.addItem(Self.menuItem("最小化", NSSelectorFromString("performMiniaturize:"), "m"))
        windowMenu.addItem(Self.menuItem("关闭", NSSelectorFromString("performClose:"), "w"))
        let windowItem = NSMenuItem()
        windowItem.submenu = windowMenu
        mainMenu.addItem(windowItem)
        NSApp.mainMenu = mainMenu
    }

    /// 标准 selector 菜单项（默认 ⌘ 键位；无 target 走 responder chain）
    private static func menuItem(_ title: String, _ action: Selector, _ key: String,
                                 modifiers: NSEvent.ModifierFlags = .command) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: action, keyEquivalent: key)
        item.keyEquivalentModifierMask = modifiers
        return item
    }

    // MARK: 界面主题（M3-plan §12 c2，评审 P1-1 AppKit appearance 机制）

    /// 主题偏好应用到应用根 + popover + 设置窗口。WHY NSApp 根（m3b2 返修）：
    /// popover.appearance 单点赋值真机未见效（截图三 popover 仍浅色，macOS 26 对
    /// NSPopover 外观覆盖的传播不可靠），NSApp.appearance 是外观继承链的根
    /// （App→window/popover→view），popover 内容、sheet、窗口全部经继承跟随；
    /// window 显式赋值保留（机制测试 T2 锁定：铬+宿主 SwiftUI 均即时跟随）。
    /// nil=跟随系统（清除覆盖）。WHY Optional 链：窗口未创建时直接短路，不因切
    /// 主题误触发懒创建。菜单栏图标为烘焙品牌色不随主题（v0.4.13 既定）；状态项
    /// 速率 title 文字色恒随系统外观（预期行为，目验注明）。
    private func applyAppearance() {
        let resolved = ShellPrefs.ShellAppearance.appearance().nsAppearance
        NSApp.appearance = resolved
        popover.appearance = resolved
        settingsWindow?.window?.appearance = resolved
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
        // 菜单栏速率 opt-in（M3 批② c5，§11.2-8）：TrafficStore 的 /traffic WS 推送驱动。
        // WHY 启动即订阅：traffic 是 let（publisher 自 PanelModel.init 存在），数据在
        // model.refresh() 的 attach 之后才流入——pref 关时 refreshRateTitle 只做清空，
        // 开销为零；订阅无需等 attach。
        model.traffic.$down
            .combineLatest(model.traffic.$up)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _, _ in self?.refreshRateTitle() }
            .store(in: &cancellables)
    }

    // MARK: 菜单栏速率 opt-in（v0.4.5 默认关闭 → 批② c5 转设置开关，§11.2-8）

    /// WHY v0.4.5（2026-11-19 第四轮反馈）默认关闭：运行态横向速率文本占宽且随数值
    /// 宽度抖动，用户反馈干扰菜单栏。批② c5 起 opt-in（tradewind.shell.menubar_rate，
    /// 通用页开关 @AppStorage 即时生效）：开关开且运行态时 title = RateText.statusItem
    /// （停止态传 nil → nil 文本 → 清空，RateText 既有契约）；关时清空只留图标。
    /// title 唯一写入点：WS 速率推送 + 图标刷新（运行态翻转即隐藏/显现）+ 壳偏好通知。
    private func refreshRateTitle() {
        guard let button = statusItem?.button else { return }
        guard ShellPrefs.menubarRate() else {
            if !button.title.isEmpty { button.title = "" } // 关：热清空（不等下条 WS 推送）
            return
        }
        // WHY effectiveRunning：与图标同口径（v0.4.7 乐观值优先），toggle 期间 title 同步翻转
        let running = model.effectiveRunning
        let text = RateText.statusItem(down: running ? model.traffic.down : nil,
                                       up: running ? model.traffic.up : nil) ?? ""
        if button.title != text { button.title = text }
    }

    // MARK: 全局启停快捷键（M3 批② c5，§11.2-7）

    /// 注册 ⌥⇧T。动作经 PanelModel.toggleFromHotkey：现场 status 探测定向 + 与
    /// popover toggleMaster 共享 busy 防抖；注册失败经 onError toast 显式上报
    /// （不静默，§11.6 冲突风险对策；OSStatus 无用户语义不回显错误码）。
    private func registerHotKey() {
        guard hotKey == nil else { return } // 已注册：热开关重复开启不重注册
        let hotKey = GlobalHotKey(handler: { [weak self] in
            guard let self else { return }
            Task { await self.model.toggleFromHotkey() }
        }, onError: { [weak self] _ in
            self?.model.toast = "快捷键 ⌥⇧T 注册失败：可能与其他应用冲突"
        })
        self.hotKey = hotKey
    }

    /// 热卸载（hotkey_enabled 关闭时；热开关语义——不重启壳即时生效）
    private func unregisterHotKey() {
        hotKey?.unregister()
        hotKey = nil
    }

    /// 壳偏好热开关观察（GeneralPane 写 @AppStorage 后 post prefChangedNotification，
    /// userInfo 带变更键名）。WHY 通知而非 UserDefaults KVO（m3b2 返修实锤）：
    /// "tradewind.shell.*" 键含点，KVO 解析为嵌套 keyPath 致 observeValue 永不触发
    /// （该机制事实曾由取证测试证实，测试本体后移除、证据见 commit 133330d）。
    @objc private func shellPrefChanged(_ note: Notification) {
        let changed = (note.userInfo?.keys.compactMap { $0 as? String }) ?? []
        for key in changed {
            switch key {
            case ShellPrefs.hotkeyEnabledKey:
                ShellPrefs.hotkeyEnabled() ? registerHotKey() : unregisterHotKey()
            case ShellPrefs.menubarRateKey:
                refreshRateTitle()
            case ShellPrefs.appearanceKey:
                applyAppearance()
            default:
                break
            }
        }
    }

    /// 壳偏好通知注册（与 GeneralPane 写入点配对；选择器式观察随对象释放自动拆除，
    /// deinit 防御性 removeObserver 兜底）
    private func observeShellPrefs() {
        NotificationCenter.default.addObserver(self,
                                               selector: #selector(shellPrefChanged(_:)),
                                               name: ShellPrefs.prefChangedNotification,
                                               object: nil)
    }

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
        // 速率 title 随运行态翻转即时隐藏/显现（停止态清空，opt-in 开时）
        refreshRateTitle()
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
