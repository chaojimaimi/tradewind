import Foundation

// MARK: - 增强链解码模型（M3 批③ B2：--json 恒退出 0 契约）

/// `enhance show <slot> --json` 解码模型（Go enhanceShowJSON 对齐）。
/// exists=false 是空转态非错误（缺文件=槽空转，batch3 §3.1）。
struct EnhanceSlotReport: Codable, Equatable {
    let slot: String
    let uid: String?
    let exists: Bool
    let content: String?

    static func decode(_ line: String) throws -> EnhanceSlotReport {
        try decodeJSON(EnhanceSlotReport.self, line: line, label: "enhance show --json")
    }
}

/// `enhance set|clear --json` 解码模型：`{"ok":true}` 或 `{"error":...}`。
struct EnhanceOKReport: Codable, Equatable {
    let ok: Bool?
    let error: String?

    static func decode(_ line: String, label: String) throws -> EnhanceOKReport {
        let report = try decodeJSON(EnhanceOKReport.self, line: line, label: label)
        if let error = report.error, !error.isEmpty {
            throw CoreBridgeError.cliMessage(error)
        }
        if report.ok != true {
            throw CoreBridgeError.decode("\(label): missing ok")
        }
        return report
    }
}

// MARK: - 增强链四槽

/// 增强链槽名（Go CLI enhance <slot> 契约；Script.js 槽后续版本不做壳编辑）。
enum EnhanceSlot: String, CaseIterable {
    case globalMerge = "global-merge"
    case rules = "rules"
    case proxies = "proxies"
    case groups = "groups"

    /// 行展示名（demo 槽位命名）。
    var title: String {
        switch self {
        case .globalMerge: return "Merge.yaml"
        case .rules: return "rules"
        case .proxies: return "proxies"
        case .groups: return "groups"
        }
    }
}

// MARK: - 增强链页模型（SettingsModel/SubsModel 不动约束下的并行实现）

/// 增强链页数据中枢：四槽状态经 enhanceShow 探测；保存/清除走 enhanceSet/enhanceClear
///（65s write 档，内容经 stdin）。busy 防抖与 toast 口径照 SubsModel.performWrite
/// 模式；失败 toast=CLI error 字段文案（batch3 §3.4）。
@MainActor
final class EnhanceModel: ObservableObject {
    struct SlotStatus: Equatable {
        let exists: Bool
        let content: String
    }

    /// 四槽状态（slot → enabled/content；load 后有效）
    @Published private(set) var slots: [EnhanceSlot: SlotStatus] = [:]
    @Published private(set) var busy = false
    /// 操作结果提示（3s 自动清除，SettingsModel.toast 同形）
    @Published var toast: String?

    /// nil = CLI 不可用（壳降级口径：页面只读、写控件禁用）
    private let bridge: CoreBridge?
    var available: Bool { bridge != nil }

    init(bridge: CoreBridge?) {
        self.bridge = bridge
    }

    /// 生产构造（CLI 路径解析失败落 nil——页面降级只读）。
    static func live() -> EnhanceModel {
        EnhanceModel(bridge: try? CoreBridge.live())
    }

    /// 页面装载（onAppear）：逐槽探测（单槽失败不影响其余，静默保持旧值）。
    /// busy 在途跳过（写后刷新走 refreshSlots 无 busy 闸）。
    func load() async {
        guard !busy else { return }
        await refreshSlots()
    }

    /// 槽是否已启用（enhanceShow 探测的 exists）。
    func isEnabled(_ slot: EnhanceSlot) -> Bool {
        slots[slot]?.exists ?? false
    }

    /// sheet 预填原文（未探测过=空串）。
    func content(of slot: EnhanceSlot) -> String {
        slots[slot]?.content ?? ""
    }

    /// 保存槽内容（sheet 保存动作）。返回是否成功（视图据此收起 sheet）。
    @discardableResult
    func save(slot: EnhanceSlot, content: String) async -> Bool {
        guard let bridge, !busy else { return false }
        busy = true
        defer { busy = false }
        do {
            try await bridge.enhanceSet(slot: slot.rawValue, content: content)
            await refreshSlots()
            showToast("增强链已更新，配置已热重载")
            return true
        } catch CoreBridgeError.timedOut {
            showToast("操作超时：可能已部分生效，请重开窗口确认")
        } catch let CoreBridgeError.cliMessage(message) {
            showToast(message) // 失败 toast=enhanceSet error 字段文案（batch3 §3.4）
        } catch {
            showToast("增强链保存失败")
        }
        return false
    }

    /// 清除槽（写 0 字节即清除；confirm alert 由视图层负责）。
    @discardableResult
    func clear(slot: EnhanceSlot) async -> Bool {
        guard let bridge, !busy else { return false }
        busy = true
        defer { busy = false }
        do {
            try await bridge.enhanceClear(slot: slot.rawValue)
            await refreshSlots()
            showToast("增强链已清除，配置已热重载")
            return true
        } catch CoreBridgeError.timedOut {
            showToast("操作超时：可能已部分生效，请重开窗口确认")
        } catch let CoreBridgeError.cliMessage(message) {
            showToast(message)
        } catch {
            showToast("增强链清除失败")
        }
        return false
    }

    /// 测试钩子：置 busy 模拟在途（busy 防抖断言）。
    func testHooksSetBusy(_ value: Bool) { busy = value }

    // MARK: 私有

    /// 逐槽探测刷新（写后权威态；单槽失败静默保留旧值——写本身已成功）。
    private func refreshSlots() async {
        guard let bridge else { return }
        var next: [EnhanceSlot: SlotStatus] = [:]
        for slot in EnhanceSlot.allCases {
            if let report = try? await bridge.enhanceShow(slot: slot.rawValue) {
                next[slot] = SlotStatus(exists: report.exists, content: report.content ?? "")
            } else if let old = slots[slot] {
                next[slot] = old
            }
        }
        slots = next
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
