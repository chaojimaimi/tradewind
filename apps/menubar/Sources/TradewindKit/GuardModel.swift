import Foundation

// MARK: - 代理守护 toast 模型（批④ §2.3：HijackEvents 变化轮询）

/// 守护事件轮询（批④ §2.1 status 三新键的消费端）：设置窗口打开期间轮询
/// status --json 的 sys_proxy.hijack_events，计数上升 → toast（macOS 桌面通知已由
/// 守护 osascript 覆盖即时性，壳 toast 是打开窗口场景的补充；窗口关闭即停——
/// 壳不做常驻轮询）。WHY 新模型而非挂 SettingsModel：SettingsModel/PanelModel
/// 不动的约束（批④纪律 8）。
@MainActor
final class GuardModel: ObservableObject {
    /// 轮询周期（桌面通知为主，壳为辅；10s 与窗口刷新节奏同量级）。
    static let pollInterval: TimeInterval = 10

    /// 最近采样到的抢改事件计数（首采样静默播种——打开窗口不 toast 历史事件）。
    @Published private(set) var hijackEvents = 0
    /// 操作提示（3s 自动清除，SettingsModel.toast 同形）。
    @Published var toast: String?
    /// 是否已采样（首采样播种基线）。
    private var seeded = false
    private let bridge: CoreBridge?
    private var timer: Timer?

    /// nil = CLI 不可用（轮询静默不启动；守护事件权威面在 state.json 与桌面通知）。
    init(bridge: CoreBridge?) {
        self.bridge = bridge
    }

    /// 窗口打开期轮询（SettingsWindowController.showSettings 调用；幂等）。
    func startPolling() {
        guard bridge != nil, timer == nil else { return }
        seeded = false
        Task { await sample() }
        timer = Timer.scheduledTimer(withTimeInterval: Self.pollInterval, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.sample() }
        }
    }

    /// 窗口关闭停止（windowShouldClose 调用）。
    func stopPolling() {
        timer?.invalidate()
        timer = nil
    }

    /// 单次采样：计数上升 → toast；下降（CLI on 重开系统代理会整段重写 sys_proxy
    /// 计数清零）→ 静默跟随新基线，不 toast 历史事件。
    func sample() async {
        guard let bridge else { return }
        guard let report = try? await bridge.status() else { return } // 探测失败保持旧基线
        let events = report.sysProxy.hijackEvents ?? 0
        defer { seeded = true }
        if !seeded {
            hijackEvents = events
            return
        }
        guard events > hijackEvents else {
            if events < hijackEvents { hijackEvents = events } // 计数重置：跟随新基线
            return
        }
        hijackEvents = events
        showToast("检测到系统代理被外部修改，已自动恢复")
    }

    private func showToast(_ message: String) {
        toast = message
        Task { [weak self] in
            try? await Task.sleep(nanoseconds: 3_000_000_000)
            if self?.toast == message {
                self?.toast = nil
            }
        }
    }
}
