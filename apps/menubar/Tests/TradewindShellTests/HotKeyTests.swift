import AppKit
import Carbon.HIToolbox
import Foundation
import Testing
@testable import TradewindKit

/// 全局启停快捷键（批② c5，M3-plan §11.2-7）：注册/注销走 registrar 注入缝
/// （单测不真注册全局热键）；壳偏好键 UserDefaults roundtrip（@AppStorage 同源契约）。
@Suite @MainActor struct HotKeyTests {
    /// 默认键位常量锁点（评审 P2：Carbon optionKey|shiftKey，不含 cmdKey）：
    /// kVK_ANSI_T=17、Option(2048)|Shift(512)=2560
    @Test func defaultKeyBindingIsOptionShiftT() {
        #expect(GlobalHotKey.defaultKeyCode == 17)
        #expect(GlobalHotKey.defaultModifiers == 2560)
    }

    /// 注册成功：isRegistered 置位、键码/修饰键原样送达 registrar、handler 未误触发
    @Test func successfulRegistrationPassesKeyBindingWithoutFiring() throws {
        var received: (UInt32, UInt32)?
        var fired = false
        let hotKey = GlobalHotKey(registrar: { key, mods in
            received = (key, mods)
            return noErr
        }, handler: { fired = true })
        #expect(hotKey.isRegistered)
        #expect(try #require(received) == (GlobalHotKey.defaultKeyCode, GlobalHotKey.defaultModifiers))
        #expect(!fired) // 注册成功≠触发
    }

    /// 自定义键位透传（改键裁剪后的注入面仍完整）
    @Test func customKeyCodeAndModifiersPassToRegistrar() throws {
        var received: (UInt32, UInt32)?
        let hotKey = GlobalHotKey(keyCode: 11, modifiers: 42, registrar: { key, mods in
            received = (key, mods)
            return noErr
        }, handler: {})
        #expect(hotKey.isRegistered)
        #expect(try #require(received) == (11, 42))
    }

    /// 注册失败：isRegistered 不置位、错误码经 onError 显式上报（不静默，§11.6）
    @Test func registrationFailureReportsViaOnError() {
        var reported: OSStatus?
        let hotKey = GlobalHotKey(registrar: { _, _ in OSStatus(paramErr) }, handler: {},
                                  onError: { reported = $0 })
        #expect(!hotKey.isRegistered)
        #expect(reported == OSStatus(paramErr))
    }

    /// 注销：状态清除且幂等（热开关反复开/关不炸）
    @Test func unregisterClearsStateAndIsIdempotent() {
        let hotKey = GlobalHotKey(registrar: { _, _ in noErr }, handler: {})
        #expect(hotKey.isRegistered)
        hotKey.unregister()
        #expect(!hotKey.isRegistered)
        hotKey.unregister()
        #expect(!hotKey.isRegistered)
    }

    /// fire() → 注入 handler 接线（生产由 Carbon 事件派发进入同一路径）
    @Test func fireInvokesInjectedHandler() {
        var fired = 0
        let hotKey = GlobalHotKey(registrar: { _, _ in noErr }, handler: { fired += 1 })
        hotKey.fire()
        hotKey.fire()
        #expect(fired == 2)
    }

    /// 壳偏好缺省值 + roundtrip：hotkey_enabled 缺省 true、menubar_rate 缺省 false
    /// （opt-in），写入后读取同值（GeneralPane @AppStorage 与 App 层读取同键同语义）
    @Test func shellPrefsDefaultsAndRoundTrip() {
        let suite = "test-shellprefs-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        #expect(ShellPrefs.hotkeyEnabled(defaults: defaults))
        #expect(!ShellPrefs.menubarRate(defaults: defaults))
        defaults.set(false, forKey: ShellPrefs.hotkeyEnabledKey)
        defaults.set(true, forKey: ShellPrefs.menubarRateKey)
        #expect(!ShellPrefs.hotkeyEnabled(defaults: defaults))
        #expect(ShellPrefs.menubarRate(defaults: defaults))
        defaults.removePersistentDomain(forName: suite)
    }

    /// 键名锁点（M3-plan §11.2-1：壳偏好统一 tradewind.shell.* 前缀；§14 R3 连接表三列）
    @Test func shellPrefsKeysFollowShellConvention() {
        #expect(ShellPrefs.hotkeyEnabledKey == "tradewind.shell.hotkey_enabled")
        #expect(ShellPrefs.menubarRateKey == "tradewind.shell.menubar_rate")
        #expect(ShellPrefs.appearanceKey == "tradewind.shell.appearance")
        #expect(ShellPrefs.silentLaunchKey == "tradewind.shell.silent_launch")
        #expect(ShellPrefs.connColHostKey == "tradewind.shell.conn_col_host")
        #expect(ShellPrefs.connColRuleKey == "tradewind.shell.conn_col_rule")
        #expect(ShellPrefs.connColNodeKey == "tradewind.shell.conn_col_node")
    }

    /// 界面主题解析（§12 c2）：三合法 raw 原样解析；非法 raw 与未写入均回退 .system
    /// （跟随系统）。WHY 自建 suite：测试进程 .standard 可能残留偏好键，隔离防串扰
    /// （同文件 shellPrefsDefaultsAndRoundTrip 先例）。
    @Test func shellAppearanceParsesLegalValuesAndFallsBackToSystem() {
        let suite = "test-appearance-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        for raw in ["system", "light", "dark"] {
            defaults.set(raw, forKey: ShellPrefs.appearanceKey)
            #expect(ShellPrefs.ShellAppearance.appearance(defaults: defaults).rawValue == raw)
        }
        defaults.set("neon", forKey: ShellPrefs.appearanceKey)
        #expect(ShellPrefs.ShellAppearance.appearance(defaults: defaults) == .system) // 非法 raw 回退
        defaults.removeObject(forKey: ShellPrefs.appearanceKey)
        #expect(ShellPrefs.ShellAppearance.appearance(defaults: defaults) == .system) // 未写缺省
        defaults.removePersistentDomain(forName: suite)
    }

    /// 静默启动解析（§13 R1）：未写入 → true（缺省开=恒静默现状）、显式 true/false
    /// 原样。非法形态不适用：Bool 存储（@AppStorage Bool 写链）无非 Bool 落盘形态，
    /// 无需 appearance 那样的回退分支。WHY 自建 suite：测试进程 .standard 可能残留
    /// 偏好键，隔离防串扰（同 shellAppearanceParses 先例）。
    @Test func silentLaunchDefaultsTrueAndRoundTrips() {
        let suite = "test-silentlaunch-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        #expect(ShellPrefs.silentLaunch(defaults: defaults)) // 未写 → true 缺省开
        defaults.set(false, forKey: ShellPrefs.silentLaunchKey)
        #expect(!ShellPrefs.silentLaunch(defaults: defaults))
        defaults.set(true, forKey: ShellPrefs.silentLaunchKey)
        #expect(ShellPrefs.silentLaunch(defaults: defaults))
        defaults.removePersistentDomain(forName: suite)
    }

    /// 界面主题 → NSAppearance 映射（评审 P1-1 AppKit appearance 机制）：system→nil
    /// （缺省即随系统）、light→aqua、dark→darkAqua（窗口/popover 外观覆盖供 App 层用）
    @Test func shellAppearanceMapsToNSAppearances() {
        #expect(ShellPrefs.ShellAppearance.system.nsAppearance == nil)
        #expect(ShellPrefs.ShellAppearance.light.nsAppearance?.name == .aqua)
        #expect(ShellPrefs.ShellAppearance.dark.nsAppearance?.name == .darkAqua)
    }
}

// MARK: - 批④ D3（P3-2 EventHotKeyID 过滤 / P3-4 失败 toast 持久化）

/// P3-2：dispatchIfMatches 过滤——signature+id 双匹配才 fire（application event
/// target 上其他同类别键盘事件不再误触发启停）。
@Suite @MainActor struct HotKeyIDFilterTests {
    private let expected = EventHotKeyID(signature: OSType(0x54_52_41_44), id: 1)

    @Test func matchingIDFires() {
        var fired = false
        let hotKey = GlobalHotKey(registrar: { _, _ in noErr }, handler: { fired = true })
        hotKey.dispatchIfMatches(EventHotKeyID(signature: expected.signature, id: expected.id))
        #expect(fired)
    }

    @Test func wrongSignatureDoesNotFire() {
        var fired = false
        let hotKey = GlobalHotKey(registrar: { _, _ in noErr }, handler: { fired = true })
        hotKey.dispatchIfMatches(EventHotKeyID(signature: OSType(0x4F_54_48_45), id: expected.id)) // 'OTHE'
        #expect(!fired)
    }

    @Test func wrongIDDoesNotFire() {
        var fired = false
        let hotKey = GlobalHotKey(registrar: { _, _ in noErr }, handler: { fired = true })
        hotKey.dispatchIfMatches(EventHotKeyID(signature: expected.signature, id: 999))
        #expect(!fired)
    }

    @Test func shouldFireIsPureSignatureAndIDMatch() {
        #expect(GlobalHotKey.shouldFire(
            received: EventHotKeyID(signature: 7, id: 8),
            expected: EventHotKeyID(signature: 7, id: 8)))
        #expect(!GlobalHotKey.shouldFire(
            received: EventHotKeyID(signature: 7, id: 9),
            expected: EventHotKeyID(signature: 7, id: 8)))
    }

    /// fire() 既有接线回归：匹配路径经 dispatchIfMatches 与直调 fire 同效。
    @Test func directFireStillWorks() {
        var fired = false
        let hotKey = GlobalHotKey(registrar: { _, _ in noErr }, handler: { fired = true })
        hotKey.fire()
        #expect(fired)
    }
}

/// P3-4：注册失败 toast 持久化口径（失败不自清；下次注册成功清除既有失败 toast，
/// 其他消息不动；失败覆盖任何在显消息）。
@Suite struct HotKeyToastTests {
    @Test func failureOverwritesAndPersists() {
        // 失败：无论此前是什么消息，终态=失败文案（不自清语义由直赋值保证）
        #expect(HotKeyToast.resolve(afterSuccess: false, current: nil)
            == HotKeyToast.failureMessage)
        #expect(HotKeyToast.resolve(afterSuccess: false, current: "其他提示")
            == HotKeyToast.failureMessage)
        #expect(HotKeyToast.resolve(afterSuccess: false, current: HotKeyToast.failureMessage)
            == HotKeyToast.failureMessage)
    }

    @Test func successClearsOnlyFailureToast() {
        #expect(HotKeyToast.resolve(afterSuccess: true, current: HotKeyToast.failureMessage) == nil)
        #expect(HotKeyToast.resolve(afterSuccess: true, current: "其他提示") == "其他提示")
        #expect(HotKeyToast.resolve(afterSuccess: true, current: nil) == nil)
    }
}
