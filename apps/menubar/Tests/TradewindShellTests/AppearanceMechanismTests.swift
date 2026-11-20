import AppKit
import SwiftUI
import Testing
@testable import TradewindKit

/// §12 c2 返修取证与机制锁定（2026-11-20 真机反馈"深色主题无任何变化"）：
/// ①（历史结论，已由 kvoDeliversDottedKeyWrites 失败实锤并驱动修复）UserDefaults
///    KVO 对含点 key（tradewind.shell.*）不送达——被解析成嵌套 keyPath；壳偏好热
///    变更投递因此改用 NotificationCenter（ShellPrefs.prefChangedNotification）。
/// ②window.appearance 运行期改写有效：铬 + 宿主 SwiftUI colorScheme 即时跟随。
/// ③NSApp.appearance 根改写有效：外观继承链 App→window→view 全跟随（popover
///    内容/sheet 走继承）。
@Suite struct AppearanceMechanismTests {

    // MARK: ① 壳偏好通知投递契约（现役机制）

    @Test func notificationDeliversPrefChangeWithChangedKey() {
        let key = ShellPrefs.appearanceKey
        var receivedKeys: Set<String> = []
        // queue: nil = post 线程同步执行（GeneralPane 主线程 post → 主线程即时刷新）
        let token = NotificationCenter.default.addObserver(
            forName: ShellPrefs.prefChangedNotification, object: nil, queue: nil
        ) { note in
            receivedKeys = Set((note.userInfo?.keys.compactMap { $0 as? String }) ?? [])
        }
        defer { NotificationCenter.default.removeObserver(token) }

        // GeneralPane 写偏好后的同款 post（同步送达）
        NotificationCenter.default.post(name: ShellPrefs.prefChangedNotification,
                                        object: nil, userInfo: [key: true])
        #expect(receivedKeys == [key], "通知未携带变更键名——App 层无法定向刷新")
    }

    // MARK: ② window.appearance 运行期重塑（chrome + 宿主 SwiftUI）

    /// SwiftUI 探针：把 @Environment(\.colorScheme) 写进类盒（无状态共享不可测）
    private final class SchemeBox: @unchecked Sendable {
        static let shared = SchemeBox()
        var scheme: ColorScheme?
    }

    private struct SchemeProbeView: View {
        @Environment(\.colorScheme) private var scheme
        var body: some View {
            Color.clear.onAppear { SchemeBox.shared.scheme = scheme }
                .onChange(of: scheme) { SchemeBox.shared.scheme = $0 }
        }
    }

    /// 泵 runloop 等 AppKit 派发 effectiveAppearance 变化（上限 1s）
    @MainActor
    private static func pumpUntilDark(_ box: SchemeBox = .shared) {
        let deadline = Date().addingTimeInterval(1.0)
        while box.scheme != .dark && Date() < deadline {
            RunLoop.main.run(mode: RunLoop.Mode.default, before: Date().addingTimeInterval(0.02))
        }
    }

    @MainActor
    @Test func windowAppearanceOverrideRestylesVisibleWindowAndHostedSwiftUI() throws {
        SchemeBox.shared.scheme = nil
        let window = NSWindow(contentRect: NSRect(x: 120, y: 120, width: 120, height: 60),
                              styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.contentView = NSHostingView(rootView: SchemeProbeView())
        window.orderFrontRegardless() // 已显示态（对齐真机：改外观时窗口可见）
        defer { window.orderOut(nil) }
        // 前提守卫：系统外观为浅色才有判别力（深色系统上守卫早退静默跳过，避免
        // 无判别力的硬失败——T3 无此前提不受影响）
        guard window.effectiveAppearance.bestMatch(from: [.aqua, .darkAqua]) == .aqua else {
            return
        }
        window.appearance = NSAppearance(named: .darkAqua)
        Self.pumpUntilDark()
        #expect(window.effectiveAppearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua,
                "window.appearance 改写未生效——铬与宿主 SwiftUI 均不会变")
        #expect(SchemeBox.shared.scheme == .dark,
                "window.appearance 生效但宿主 SwiftUI colorScheme 未跟随")
    }

    // MARK: ③ NSApp.appearance 根改写（popover 内容/sheet 经继承跟随的同链路）

    @MainActor
    @Test func appAppearanceOverridePropagatesToWindowWithoutOwnAppearance() throws {
        SchemeBox.shared.scheme = nil
        let window = NSWindow(contentRect: NSRect(x: 260, y: 120, width: 120, height: 60),
                              styleMask: [.titled, .closable], backing: .buffered, defer: false)
        // 窗口自身无 appearance 覆盖（nil=继承应用根）——对齐 popover 内容的继承语义
        window.contentView = NSHostingView(rootView: SchemeProbeView())
        window.orderFrontRegardless()
        defer {
            window.orderOut(nil)
            NSApp.appearance = nil // 清覆盖还原（后续测试/真机不受残留影响）
        }
        #expect(window.appearance == nil)
        NSApp.appearance = NSAppearance(named: .darkAqua)
        Self.pumpUntilDark()
        #expect(SchemeBox.shared.scheme == .dark,
                "NSApp 根外观未沿继承链（App→window→view）传播——popover 内容同链必断")
    }
}
