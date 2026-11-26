import Foundation

// MARK: - 通用页提交判定纯函数（可单测；视图/模型只做布线，M3-plan §11.3 页 3）

/// 通用页可测逻辑集合（非隔离：Swift Testing 直调无需 actor 跳转）。
enum GeneralPaneLogic {
    /// number 输入提交判定（评审 P2：写编排含全量热重载断连接，值未变化不写）。
    enum NumberCommit: Equatable {
        case unchanged // 值未变化：不写
        case invalid // 非整数/越界：拒绝提交（视图回显现值）
        case write(Int) // 写入该值
    }

    /// 混合端口/网关端口合法范围（Go store.Settings.Validate 同口径 1-65535）
    static let portRange: ClosedRange<Int> = 1...65535
    /// 日志保留天数范围（Go Validate 拒负值/超 10 年；0 会被归一为 14，UI 不发 0）
    static let retentionRange: ClosedRange<Int> = 1...3650

    /// number 输入分类：空白容忍（trim）、非整数拒绝、未变化不写、越界拒绝。
    static func classifyNumberEntry(_ raw: String, current: Int, validRange: ClosedRange<Int>) -> NumberCommit {
        guard let value = Int(raw.trimmingCharacters(in: .whitespacesAndNewlines)) else {
            return .invalid
        }
        if value == current { return .unchanged }
        guard validRange.contains(value) else { return .invalid }
        return .write(value)
    }

    /// interval select 档位值（demo 选项序：24h/12h/1h/关闭）
    static let defaultIntervalMode = "24"
    static let intervalModes = ["24", "12", "1", "off"]

    /// interval select 初值（纯函数）：allow=false → "off"（评审 P1-2：off 是真实
    /// 关闭开关 AllowAutoUpdate=false）；interval 秒值 /3600 命中 1/12/24；
    /// updateInterval 为 nil（Go omitempty 缺省键：off 档写 0/新订阅恒 0，c2 审查
    /// P1）或已归一的 86400 落默认 24。
    static func intervalMode(allowAutoUpdate: Bool?, updateInterval: Int?) -> String {
        if allowAutoUpdate == false { return "off" }
        switch (updateInterval ?? 86_400) / 3600 {
        case 1: return "1"
        case 12: return "12"
        default: return defaultIntervalMode
        }
    }

    /// 从 list --json 条目集取当前订阅的 interval 档位（查无当前条目落默认档）。
    static func intervalMode(in entries: [ProfileIntervalEntry]) -> String {
        guard let current = entries.first(where: { $0.current }) else { return defaultIntervalMode }
        return intervalMode(allowAutoUpdate: current.allowAutoUpdate, updateInterval: current.updateInterval)
    }

    /// P3-6（批④ §4，F13）：file 型订阅无自动更新语义（Go dueForUpdate 对 file 型
    /// 恒 false，interval 落盘即静默无效）→ interval 下拉禁用。type 缺省（旧 CLI）
    /// 与查无当前条目 → 可用（保守口径，不误禁 remote 场景）。
    /// WHY 字面量 "remote" 而非 SubsModel.remoteType：本函数 nonisolated（GeneralPane
    /// Logic 非隔离纯函数），@MainActor 类的 static 属性不可从非隔离上下文引用
    /// （release 构建零警告纪律）；字面量与 SubsModel.remoteType 同源（Go
    /// ProfileTypeRemote），漂移面由两处注释互指封堵。
    static func intervalDisabled(type: String?) -> Bool {
        guard let type else { return false }
        return type != "remote"
    }

    /// c8 ④：TUN 形态下"允许局域网"开关置灰——allow-lan 真值在服务端归一化器被硬拒
    /// （2026-11-26 真机发现 root 内核可被局域网无鉴权使用，用户拍板），推送注定被拒。
    /// tunEnabled 为 nil（旧 CLI 缺键）按未开启处理：保守可用口径，与 intervalDisabled
    /// 的缺省语义一致（不误禁旧 CLI 场景）。
    static func allowLanDisabled(tunEnabled: Bool?) -> Bool {
        tunEnabled == true
    }
}

/// 通用页 number 字段元数据（键名与 Go settings set 白名单对齐，范围同 Validate）。
enum SettingsNumberField: String {
    case mixedPort = "mixed_port"
    case gatewayPort = "gateway_port"
    case logRetention = "log_retention_days"

    var validRange: ClosedRange<Int> {
        switch self {
        case .mixedPort, .gatewayPort: return GeneralPaneLogic.portRange
        case .logRetention: return GeneralPaneLogic.retentionRange
        }
    }

    func currentValue(in snapshot: SettingsSnapshot) -> Int {
        switch self {
        case .mixedPort: return snapshot.mixedPort
        case .gatewayPort: return snapshot.gatewayPort
        case .logRetention: return snapshot.logRetentionDays
        }
    }
}

// MARK: - 设置窗口模型（M3-plan §11.2-10：读走 settingsGetJSON，写走 45s runner）

/// 设置窗口数据中枢：读操作（get --json/autostart --json/list --json/status）
/// 装配初始值；写操作乐观回显 + 失败回退（PanelModel v0.4.7 乐观口径同理——
/// 45s 写编排期间开关保持点击态，成功 toast、失败 toast+视觉回退）。
@MainActor
final class SettingsModel: ObservableObject {
    @Published private(set) var settings: SettingsSnapshot?
    @Published private(set) var autostartOn = false
    @Published private(set) var intervalMode = GeneralPaneLogic.defaultIntervalMode
    /// 关于页数据源（status --json 附带读取，避免二次探测）
    @Published private(set) var coreVersion: String?
    @Published private(set) var currentProfileName: String?
    @Published private(set) var busy = false
    /// 操作结果提示（3s 自动清除，PanelModel.toast 同形）
    @Published var toast: String?

    /// nil = CLI 不可用（壳降级口径：页面只读、写控件禁用）
    private let bridge: CoreBridge?
    /// CLI 不可用判定（视图据此禁用写控件并给出替代文案）
    var available: Bool { bridge != nil }
    /// 主数据（settings get --json）读取失败标记（区分"加载中"与失败态）
    @Published private(set) var loadFailed = false
    /// 测试钩子：置 busy 模拟在途（busy 防抖断言；生产路径写 performWrite 内部置位）
    func testHooksSetBusy(_ value: Bool) { busy = value }

    init(bridge: CoreBridge?) {
        self.bridge = bridge
    }

    /// 窗口打开时拉全量初始值。主数据（settings）失败置 loadFailed + toast 明示；
    /// 次级数据（自启态/间隔档/内核版本）失败落默认展示不阻塞窗口（口径见各分支注释）。
    func load() async {
        guard let bridge else { return }
        guard !busy else { return } // 45s 写在途：过时权威读不得覆盖乐观回显（审查 P3-3）
        loadFailed = false
        do {
            settings = try await bridge.settingsGetJSON()
        } catch {
            loadFailed = true
            showToast("设置读取失败")
        }
        // 自启态失败：开关保持 off；误导面小（真实态以 launchctl 为准）
        autostartOn = (try? await bridge.autostartStatusJSON())?.enabled ?? false
        // 间隔档失败：保持默认档显示（下次打开重拉；interval 是订阅页 c3 亦用的字段）
        if let entries = try? await bridge.listJSON() {
            intervalMode = GeneralPaneLogic.intervalMode(in: entries)
        }
        // 关于页信息失败：留空显示 "—"（status 探测失败非设置窗口的职责面）
        if let report = try? await bridge.status() {
            coreVersion = report.core.version.isEmpty ? nil : report.core.version
            currentProfileName = report.profile.name.isEmpty ? nil : report.profile.name
        }
    }

    // MARK: 写动作（乐观回显 + 失败回退）

    /// number 字段提交（失焦/回车触发；值未变化不写由 GeneralPaneLogic 判定）。
    /// 返回是否落写（视图据此回显规范化值或拒绝态）。
    func commitNumber(_ field: SettingsNumberField, raw: String) async -> Bool {
        guard let settings else { return false }
        let current = field.currentValue(in: settings)
        guard case .write(let value) = GeneralPaneLogic.classifyNumberEntry(
            raw, current: current, validRange: field.validRange) else {
            return false
        }
        // 跨字段互斥壳侧预检（审查 P3-1）：mixed_port==gateway_port 被 Go Validate
        // 拒绝但 stderr 不回显——前置拦截给出有因由的 toast
        let other: Int? = field == .mixedPort ? settings.gatewayPort : (field == .gatewayPort ? settings.mixedPort : nil)
        if let other, value == other {
            showToast("保存失败：混合端口与面板网关端口不能相同")
            return false
        }
        let old = current
        apply(field: field, value: value) // 乐观回显（写编排最长 45s，UI 不能悬空）
        var reverted = false
        await performWrite(success: "设置已保存（\(field.rawValue) = \(value)）",
                           failure: "设置保存失败",
                           onRevert: { reverted = true }) {
            try await self.bridge?.settingsSet(key: field.rawValue, value: String(value))
        }
        if reverted { apply(field: field, value: old) }
        return !reverted
    }

    /// 允许局域网开关（→ settings set allow_lan on|off）。
    func setAllowLan(_ on: Bool) async {
        let old = settings?.allowLan
        settings?.allowLan = on // 乐观回显
        var reverted = false
        await performWrite(success: on ? "允许局域网已开启" : "允许局域网已关闭",
                           failure: "设置保存失败",
                           onRevert: { reverted = true }) {
            try await self.bridge?.settingsSet(key: "allow_lan", value: on ? "on" : "off")
        }
        if reverted, let old { settings?.allowLan = old }
    }

    /// 自动更新间隔 select（作用于当前订阅，偏差 #3；off = AllowAutoUpdate=false）。
    func setInterval(_ mode: String) async {
        let old = intervalMode
        intervalMode = mode // 乐观回显
        var reverted = false
        await performWrite(success: "自动更新间隔已更新", failure: "间隔设置失败",
                           onRevert: { reverted = true }) {
            try await self.bridge?.subsIntervalCurrent(mode: mode)
        }
        if reverted { intervalMode = old }
    }

    /// 开机自启开关（→ CLI autostart on/off，初始值 autostart status --json）。
    func setAutostart(_ on: Bool) async {
        let old = autostartOn
        autostartOn = on // 乐观回显
        var reverted = false
        await performWrite(success: on ? "开机自启已开启" : "开机自启已关闭",
                           failure: "开机自启设置失败",
                           onRevert: { reverted = true }) {
            try await self.bridge?.autostart(on: on)
        }
        if reverted { autostartOn = old }
    }

    /// 恢复默认（确认对话框由视图层负责；成功后重拉 get --json 显示权威值）。
    func resetToDefaults() async {
        guard let bridge else { return }
        await performWrite(success: "已恢复默认设置", failure: "恢复默认失败") {
            try await bridge.settingsReset()
            self.settings = try await bridge.settingsGetJSON()
        }
    }

    // MARK: 私有

    /// 写编排统一包装：busy 防抖（在途拒绝再次提交）+ 成功/失败 toast + 失败回退。
    private func performWrite(success: String, failure: String,
                              onRevert: (() -> Void)? = nil,
                              mutation: () async throws -> Void) async {
        guard !busy else {
            onRevert?()
            return
        }
        busy = true
        defer { busy = false }
        do {
            try await mutation()
            showToast(success)
        } catch CoreBridgeError.timedOut {
            // 超时=可能已部分生效（settings.yaml 先于 reload 落盘，审查 P2）：
            // 不回退乐观回显（UI 与磁盘可能相反），提示重开确认权威态
            showToast("保存超时：可能已部分生效，请重开设置窗口确认")
        } catch {
            onRevert?() // 失败 toast 不改本地（§11.2-10）——乐观回显显式回退
            showToast(failure)
        }
    }

    private func apply(field: SettingsNumberField, value: Int) {
        guard settings != nil else { return }
        switch field {
        case .mixedPort: settings?.mixedPort = value
        case .gatewayPort: settings?.gatewayPort = value
        case .logRetention: settings?.logRetentionDays = value
        }
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
