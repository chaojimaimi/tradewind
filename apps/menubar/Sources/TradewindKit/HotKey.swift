import AppKit
import Carbon.HIToolbox
import Foundation

// MARK: - 壳偏好键（M3-plan §11.2-1 双轨制：壳偏好走 UserDefaults tradewind.shell.*，壳不写 YAML）

/// 壳偏好键集中声明：设置窗口 GeneralPane 的 @AppStorage 与 App 层 KVO/读取同源，
/// 防字面量漂移。值语义：hotkey_enabled 缺省 true（开）、menubar_rate 缺省 false
/// （opt-in，v0.4.5 用户反馈速率文本占宽抖动故默认关闭）、appearance 缺省 system
/// （跟随系统，§12 c2）。
enum ShellPrefs {
    static let hotkeyEnabledKey = "tradewind.shell.hotkey_enabled"
    static let menubarRateKey = "tradewind.shell.menubar_rate"
    static let appearanceKey = "tradewind.shell.appearance"

    /// 快捷键开关：object 为 nil 即未写入 → true（与 bool(forKey:) 的 false 缺省区分）
    static func hotkeyEnabled(defaults: UserDefaults = .standard) -> Bool {
        defaults.object(forKey: hotkeyEnabledKey) == nil ? true : defaults.bool(forKey: hotkeyEnabledKey)
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
    /// 事件签名（任意 OSType，取 'TRAD' 便于调试器辨识；ID 1 = 壳仅注册一枚热键）
    private static let hotKeySignature = OSType(0x54_52_41_44) // 'TRAD'
    private static let hotKeyID: UInt32 = 1

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
        isRegistered = false
    }

    /// 事件回调入口（internal：单测直调验证 handler 接线；生产由 Carbon 事件派发）。
    /// WHY 同步直调：热键事件由 application event target 在主线程主循环派发，
    /// 壳的 UI 更新同处主线程，无需队列跳转（与 popover 点击同一线程语义）。
    func fire() { handler() }

    // MARK: Carbon 装配（真机路径）

    /// 装 application 级事件处理器 + 注册热键；两步任一失败即中止并返回错误码。
    private func installEventHandlerAndRegister(keyCode: UInt32, modifiers: UInt32) -> OSStatus {
        var eventType = EventTypeSpec(eventClass: OSType(kEventClassKeyboard),
                                      eventKind: UInt32(kEventHotKeyPressed))
        // WHY passUnretained：实例由 AppDelegate 持有，事件面不持引用；deinit 中
        // unregister 先拆除处理器再释放，回调不会触达已释放实例
        let userData = Unmanaged.passUnretained(self).toOpaque()
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

    /// C 回调（@convention(c) 不能捕获上下文）：userData 取回实例后转发 fire()。
    private static let eventUPP: EventHandlerUPP = { _, _, userData in
        guard let userData else { return noErr }
        Unmanaged<GlobalHotKey>.fromOpaque(userData).takeUnretainedValue().fire()
        return noErr
    }
}
