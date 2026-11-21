import Foundation
import Sparkle

// MARK: - Sparkle 更新服务（batch5 §3.2：壳内自动更新，替代 CLI 三态区块，F10）

/// Sparkle 薄包装：feed/公钥由 build.sh 注入 app bundle Info.plist（SUFeedURL/
/// SUPublicEDKey），SPUStandardUpdaterController 自行从主 bundle 读取——本类不做
/// 任何配置搬运，只面向 UI 暴露"检查更新"动作。自动检查沿用 Sparkle 默认（每日）；
/// 有更新 → 标准窗安装流程，无更新 → 标准窗"已是最新"（public 前私有仓 feed 404
/// → Sparkle 标准错误窗不静默，README/报告注明）。
///
/// WHY 不做 nil 失败面：装配失败（feed/密钥配置缺失）由 Sparkle 记为 updater 未就绪，
/// canCheckForUpdates 恒 false——按钮禁用降级即达成，无需自造 optional。
/// Sparkle 网络/UI 行为不进单测（F11：壳测试不触网哲学，装配面锁点即可）。
@MainActor
final class UpdateService: ObservableObject {
    private let controller: SPUStandardUpdaterController
    /// UI 可观察态（updater 异步启动成功后翻 true；配置缺失/未启动恒 false）
    @Published private(set) var canCheckForUpdates = false
    private var observation: NSKeyValueObservation?

    /// WHY startsUpdater 参数：生产默认 true（启动 updater，Sparkle 默认每日自动检查）；
    /// 单测以 false 装配（不触网、不启动自动检查）。SPUUpdater.canCheckForUpdates
    /// 是 KVO 可观察属性（SPUStandardUpdaterController 的菜单项使能同源绑定）——
    /// 映射进 @Published 供 SwiftUI 按钮 disabled 派生；变化可能来自 Sparkle 内部
    /// 线程，统一 hop 主 actor。
    init(startsUpdater: Bool = true) {
        // Sparkle 2.9.1 Swift API：init(startingUpdater:updaterDelegate:userDriverDelegate:)
        controller = SPUStandardUpdaterController(startingUpdater: startsUpdater,
                                                  updaterDelegate: nil,
                                                  userDriverDelegate: nil)
        observation = controller.updater.observe(\.canCheckForUpdates, options: [.initial, .new]) {
            [weak self] updater, _ in
            let value = updater.canCheckForUpdates
            Task { @MainActor [weak self] in
                self?.canCheckForUpdates = value
            }
        }
    }

    /// 检查更新（弹 Sparkle 标准窗）；未就绪时 no-op（按钮同时由 canCheckForUpdates 禁用）。
    func checkForUpdates() {
        guard canCheckForUpdates else { return }
        controller.checkForUpdates(nil)
    }
}
