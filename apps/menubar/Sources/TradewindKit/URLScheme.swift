import AppKit
import Carbon
import Foundation

// MARK: - tradewind:// URL Scheme（批④ §2.3，P2 #20）
//
// 注册面在 build.sh Info.plist CFBundleURLTypes；接收面经 NSAppleEventManager
// kAEGetURL/kInternetEventClass handler（AppDelegate 装配，注入缝可测）。
// 解析纯函数独立可测；确认 alert 宿主 = 订阅管理页（先 openSettings 再挂 alert）。

// MARK: 解析纯函数（可单测；外部世界不可信输入面）

/// 解析成功的导入事件（url 含 token——不进日志、不进 toast 正文）。
struct URLImportEvent: Equatable {
    let url: String
}

/// `tradewind://import?url=<encoded>` 解析矩阵（评审 P1-4/P2-5）：
///   - 非 tradewind scheme / 非 import host / 带路径段 → nil（忽略，不提示）；
///   - 缺 url 参数 / 空值 / 超过 4KB → nil（防御恶意超长链接）；
///   - query 值 percent-decode（URLComponents 语义）。
enum URLSchemeParser {
    static let scheme = "tradewind"
    static let importHost = "import"
    /// url 参数 4KB 上限（字节）。
    static let maxURLEventBytes = 4096

    static func parse(_ raw: String) -> URLImportEvent? {
        guard !raw.isEmpty, raw.utf8.count <= maxURLEventBytes else { return nil }
        guard let components = URLComponents(string: raw) else { return nil }
        guard components.scheme?.lowercased() == scheme,
              components.host?.lowercased() == importHost,
              components.path.isEmpty || components.path == "/" else { return nil }
        guard let value = components.queryItems?.first(where: { $0.name == "url" })?.value,
              !value.isEmpty else { return nil }
        return URLImportEvent(url: value)
    }

    /// 确认 alert 正文用的宿主形态：仅 URL host（token/路径/查询串一律不显示）。
    static func displayHost(_ url: String) -> String {
        URL(string: url)?.host ?? ""
    }
}

// MARK: 导入协调器（URL 事件 → 订阅页确认 alert 的桥）

/// URL 导入协调（批④评审 P1-4/P2-5）：收到事件先 openSettings 落订阅管理页（SwiftUI
/// .alert 需视图宿主——SettingsPanes.swift:131-132 既有教训；accessory 态设置窗懒创建
/// 可能不存在），再由本协调器的 pendingURL 驱动 SubscriptionsPane 挂确认 alert。
/// 确认前不导入——add 导入即切 current 并热重载（F9），恶意网页一条链接即可接管
/// 用户流量路由，确认是必需缓解。
@MainActor
final class ImportCoordinator: ObservableObject {
    /// 待确认的订阅 URL（nil=无；alert 的 isPresented 由其派生）。
    @Published var pendingURL: String?

    /// 确认 alert 正文 host（pendingURL 为 nil 时空串）。
    var pendingHost: String {
        guard let pendingURL else { return "" }
        return URLSchemeParser.displayHost(pendingURL)
    }

    /// 确认/取消后清除（alert dismiss 由 pendingURL==nil 派生）。
    func clear() { pendingURL = nil }
}

// MARK: AppDelegate 接收面（装配与事件入口）

extension AppDelegate {
    /// 装配 kAEGetURL handler（applicationDidFinishLaunching 调用一次）。
    /// WHY 经 installGetURLHandler 注入缝：单测 stub 捕获 (target, selector)，
    /// 不真注册全局 Apple 事件面（真注册会污染测试进程的事件路由）。
    func installURLSchemeHandler() {
        installGetURLHandler(self, #selector(handleGetURLEvent(_:withReplyEvent:)))
    }

    /// kAEGetURL 事件入口（Apple 事件机制主线程派发；@MainActor 与壳其余 UI 同线程）。
    /// WHY 不落日志：直对象参数是完整 URL（含 token），原始串不进任何输出。
    @objc func handleGetURLEvent(_ event: NSAppleEventDescriptor, withReplyEvent reply: NSAppleEventDescriptor) {
        guard let raw = event.paramDescriptor(forKeyword: keyDirectObject)?.stringValue else { return }
        handleIncomingURL(raw)
    }

    /// URL 事件处理（internal：单测直调验证接线）：解析 → 先 openSettings（落订阅
    /// 管理页 + alert 宿主）再由 pendingURL 挂确认 alert；解析失败静默忽略。
    func handleIncomingURL(_ raw: String) {
        guard let event = URLSchemeParser.parse(raw) else { return }
        importCoordinator.pendingURL = event.url
        openSettings()
    }
}
