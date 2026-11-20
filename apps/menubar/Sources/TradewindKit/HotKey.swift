import AppKit
import Carbon.HIToolbox
import Foundation

// MARK: - 壳偏好键（M3-plan §11.2-1 双轨制：壳偏好走 UserDefaults tradewind.shell.*，壳不写 YAML）

/// 壳偏好键集中声明：设置窗口 GeneralPane 的 @AppStorage 与 App 层通知/读取同源，
/// 防字面量漂移。值语义：hotkey_enabled 缺省 true（开）、menubar_rate 缺省 false
/// （opt-in，v0.4.5 用户反馈速率文本占宽抖动故默认关闭）、appearance 缺省 system
/// （跟随系统，§12 c2）、silent_launch 缺省 true（恒静默现状，§13 R1）。热变更经
/// prefChangedNotification 投递（键含点，KVO 不送达）。
enum ShellPrefs {
    static let hotkeyEnabledKey = "tradewind.shell.hotkey_enabled"
    static let menubarRateKey = "tradewind.shell.menubar_rate"
    static let appearanceKey = "tradewind.shell.appearance"
    static let silentLaunchKey = "tradewind.shell.silent_launch"
    /// 连接表 host/rule/node 三列文本宽偏好（§14 R3；缺省 150/118/104，
    /// ConnectionsPane @AppStorage 直接消费）。WHY 不走通知链：键生命周期限于
    /// 连接页、无 App 层观察者，§12 KVO 陷阱不适用（不走 prefChangedNotification）。
    static let connColHostKey = "tradewind.shell.conn_col_host"
    static let connColRuleKey = "tradewind.shell.conn_col_rule"
    static let connColNodeKey = "tradewind.shell.conn_col_node"

    /// 壳偏好变更通知（m3b2 返修：UserDefaults KVO 对含点 key 不送达——取证测试
    /// 曾实锤 "tradewind.shell.*" 被 KVO 解析成嵌套 keyPath 致 observeValue 永不
    /// 触发，取证本体后移除、证据见 commit 133330d）。GeneralPane 写入偏好后
    /// post 本通知（userInfo 带变更键名→true），App 层据此刷新对应行为。
    static let prefChangedNotification = Notification.Name("tradewind.shell.pref-changed")

    /// 快捷键开关：object 为 nil 即未写入 → true（与 bool(forKey:) 的 false 缺省区分）
    static func hotkeyEnabled(defaults: UserDefaults = .standard) -> Bool {
        defaults.object(forKey: hotkeyEnabledKey) == nil ? true : defaults.bool(forKey: hotkeyEnabledKey)
    }

    /// 静默启动：object 为 nil 即未写入 → true（缺省开=恒静默现状，§13 R1）
    static func silentLaunch(defaults: UserDefaults = .standard) -> Bool {
        defaults.object(forKey: silentLaunchKey) == nil ? true : defaults.bool(forKey: silentLaunchKey)
    }

    /// 菜单栏速率开关：opt-in，未写入即 false
    static func menubarRate(defaults: UserDefaults = .standard) -> Bool {
        defaults.bool(forKey: menubarRateKey)
    }

    /// 界面主题（评审 P1-1：AppKit appearance 机制而非 preferredColorScheme——
    /// NSHostingView/NSPopover 宿主下后者有效性随 macOS 版本不一且不改窗口/popover 铬）
    enum ShellAppearance: String, CaseIterable {
        case system, light, dark

        /// 跟随系统 → nil（NSAppearance 缺省即随系统）；解析失败/未写 → .system
        static func appearance(defaults: UserDefaults = .standard) -> ShellAppearance {
            ShellAppearance(rawValue: defaults.string(forKey: ShellPrefs.appearanceKey) ?? "") ?? .system
        }

        var nsAppearance: NSAppearance? {
            switch self {
            case .system: return nil
            case .light: return NSAppearance(named: .aqua)
            case .dark: return NSAppearance(named: .darkAqua)
            }
        }
    }
}

// MARK: - 全局启停快捷键（M3-plan §11.2-7，批② c5）

/// Carbon `RegisterEventHotKey` 封装：AppKit 链接即可用（HIToolbox 头随 macOS SDK），
/// 零第三方依赖基线不破。默认 ⌥⇧T（kVK_ANSI_T + optionKey|shiftKey，评审 P2：
/// Carbon 常量名不含 cmdKey）。注册失败经 onError 显式上报（壳 toast，不静默，§11.6）。
/// WHY registrar 注入缝：单测不真注册全局热键（真注册会污染测试进程的全局事件面），
/// 注入桩返回 noErr/错误码走同一判定路径；无真实句柄时 unregister/deinit 自然 no-op。
/// P3-2（批④ §4）：eventUPP 校验 EventHotKeyID——回调从事件参数提取 ID，匹配本实例
/// 注册的 signature+id 才 fire（application event target 上其他同类别键盘事件不再
/// 误触发启停）。
final class GlobalHotKey {
    /// 默认键位：T 键 + Option + Shift（⌥⇧T）
    static let defaultKeyCode = UInt32(kVK_ANSI_T)
    static let defaultModifiers = UInt32(optionKey | shiftKey)

    /// 注册状态（unregister/deinit 后置 false；测试据此断言）
    private(set) var isRegistered = false
    private let handler: () -> Void
    private let onError: (OSStatus) -> Void
    private var hotKeyRef: EventHotKeyRef?
    private var eventHandlerRef: EventHandlerRef?
    /// userData 载荷的实例持有（code-reviewer P1-1：passUnretained 不持引用，
    /// 局部变量随装配函数返回释放=热键触发 use-after-free；unowned owner 不成环）
    private var hotKeyContext: HotKeyContext?
    /// 事件签名（任意 OSType，取 'TRAD' 便于调试器辨识；ID 1 = 壳仅注册一枚热键）
    private static let hotKeySignature = OSType(0x54_52_41_44) // 'TRAD'
    private static let hotKeyID: UInt32 = 1
    /// 本实例的注册 ID（eventUPP 匹配判据；注册前构造即锁定）
    private let expectedID = EventHotKeyID(signature: OSType(0x54_52_41_44), id: 1)

    /// registrar 为 nil 走真 Carbon 注册；注入桩（单测）替换注册调用本身。
    init(keyCode: UInt32 = GlobalHotKey.defaultKeyCode,
         modifiers: UInt32 = GlobalHotKey.defaultModifiers,
         registrar: ((UInt32, UInt32) -> OSStatus)? = nil,
         handler: @escaping () -> Void,
         onError: @escaping (OSStatus) -> Void = { _ in }) {
        self.handler = handler
        self.onError = onError
        let status = registrar.map { $0(keyCode, modifiers) }
            ?? installEventHandlerAndRegister(keyCode: keyCode, modifiers: modifiers)
        guard status == noErr else {
            onError(status)
            return
        }
        isRegistered = true
    }

    deinit { unregister() }

    /// 热卸载（hotkey_enabled 关闭时调用；幂等，无真实句柄时为 no-op）
    func unregister() {
        if let hotKeyRef { UnregisterEventHotKey(hotKeyRef) }
        if let eventHandlerRef { RemoveEventHandler(eventHandlerRef) }
        hotKeyRef = nil
        eventHandlerRef = nil
        hotKeyContext = nil // 事件面已拆除，断开可达性（code-reviewer P1-1）
        isRegistered = false
    }

    /// 事件回调入口（internal：单测直调验证 handler 接线；生产由 Carbon 事件派发）。
    /// WHY 同步直调：热键事件由 application event target 在主线程主循环派发，
    /// 壳的 UI 更新同处主线程，无需队列跳转（与 popover 点击同一线程语义）。
    func fire() { handler() }

    /// 事件派发判定（internal：单测直调验证 P3-2 过滤；生产由 eventUPP 从 Carbon
    /// 事件参数提取 EventHotKeyID 后调用）——signature+id 双匹配才 fire。
    func dispatchIfMatches(_ received: EventHotKeyID) {
        guard Self.shouldFire(received: received, expected: expectedID) else { return }
        fire()
    }

    /// ID 匹配判定（纯函数：signature+id 双匹配）。
    static func shouldFire(received: EventHotKeyID, expected: EventHotKeyID) -> Bool {
        received.signature == expected.signature && received.id == expected.id
    }

    // MARK: Carbon 装配（真机路径）

    /// 装 application 级事件处理器 + 注册热键；两步任一失败即中止并返回错误码。
    /// userData 传 HotKeyContext（P3-2：回调需要实例引用 + 匹配 ID 两个数据）。
    private func installEventHandlerAndRegister(keyCode: UInt32, modifiers: UInt32) -> OSStatus {
        var eventType = EventTypeSpec(eventClass: OSType(kEventClassKeyboard),
                                      eventKind: UInt32(kEventHotKeyPressed))
        // WHY 实例存储持有 context（code-reviewer P1-1）：passUnretained 不持引用，
        // 局部变量随本函数返回即释放——热键触发时 takeUnretainedValue 是
        // use-after-free（单测全注入 registrar 绕过真装配路径，故测试全绿未暴露）。
        // unowned owner 不成环；unregister 置 nil 断开事件面可达性。
        let context = HotKeyContext(owner: self, expected: expectedID)
        hotKeyContext = context
        let userData = Unmanaged.passUnretained(context).toOpaque()
        var installed: EventHandlerRef?
        let installStatus = InstallEventHandler(
            GetApplicationEventTarget(), Self.eventUPP, 1, &eventType, userData, &installed)
        guard installStatus == noErr else { return installStatus }
        eventHandlerRef = installed
        let hotKeyID = EventHotKeyID(signature: Self.hotKeySignature, id: Self.hotKeyID)
        var registered: EventHotKeyRef?
        let registerStatus = RegisterEventHotKey(
            keyCode, modifiers, hotKeyID, GetApplicationEventTarget(), 0, &registered)
        guard registerStatus == noErr else {
            // 注册失败不留半套：已装的事件处理器一并拆除
            RemoveEventHandler(installed)
            eventHandlerRef = nil
            return registerStatus
        }
        hotKeyRef = registered
        return noErr
    }

    /// C 回调（@convention(c) 不能捕获上下文）：userData 取回 context，从事件参数
    /// 提取 EventHotKeyID（kEventParamDirectObject / typeEventHotKeyID）匹配后转发。
    /// 提取失败（无该参数的事件）不 fire——P3-2 过滤的保守面。
    private static let eventUPP: EventHandlerUPP = { _, inEvent, userData in
        guard let userData, let inEvent,
              let received = GlobalHotKey.extractEventHotKeyID(inEvent) else { return noErr }
        let context = Unmanaged<HotKeyContext>.fromOpaque(userData).takeUnretainedValue()
        context.owner.dispatchIfMatches(received)
        return noErr
    }

    /// 从 Carbon 事件提取 kEventParamDirectObject 的 EventHotKeyID。
    private static func extractEventHotKeyID(_ event: EventRef) -> EventHotKeyID? {
        var id = EventHotKeyID()
        let status = GetEventParameter(
            event,
            EventParamName(kEventParamDirectObject),
            EventParamType(typeEventHotKeyID),
            nil,
            MemoryLayout<EventHotKeyID>.size,
            nil,
            &id)
        return status == noErr ? id : nil
    }
}

/// eventUPP 的 userData 载荷（实例引用 + 匹配 ID；unowned 防循环持用——实例由
/// AppDelegate 持有，context 随实例生命周期）。
private final class HotKeyContext {
    unowned let owner: GlobalHotKey
    let expected: EventHotKeyID

    init(owner: GlobalHotKey, expected: EventHotKeyID) {
        self.owner = owner
        self.expected = expected
    }
}
