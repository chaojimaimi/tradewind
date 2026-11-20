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

    /// 键名锁点（M3-plan §11.2-1：壳偏好统一 tradewind.shell.* 前缀）
    @Test func shellPrefsKeysFollowShellConvention() {
        #expect(ShellPrefs.hotkeyEnabledKey == "tradewind.shell.hotkey_enabled")
        #expect(ShellPrefs.menubarRateKey == "tradewind.shell.menubar_rate")
    }
}
