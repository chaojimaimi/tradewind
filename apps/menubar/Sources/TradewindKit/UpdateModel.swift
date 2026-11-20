import Foundation

// MARK: - 自升级解码模型（M3 批③ B1：--json 恒退出 0 契约，batch3 §2.2/§2.3）

/// `version --check --json` 解码模型：成功五字段或 `{"error":...}`（Go 侧恒退出 0，
/// 失败信息在 stdout error 字段——壳侧零容错解析）。
struct UpdateCheckReport: Codable, Equatable {
    let current: String?
    let latest: String?
    let updateAvailable: Bool?
    let asset: String?
    let assetSize: Int64?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case current, latest, asset, error
        case updateAvailable = "update_available"
        case assetSize = "asset_size"
    }

    /// 解码单行 JSON 输出；解析失败显式报错（壳按降级态处理而非崩溃）。
    static func decode(_ line: String) throws -> UpdateCheckReport {
        try decodeJSON(UpdateCheckReport.self, line: line, label: "version --check --json")
    }

    /// 三态映射（AboutPane 更新区块，batch3 §2.4）：已是最新 / 发现新版 / 失败。
    /// WHY latest 为 nil 也算失败：恒退出 0 契约下结果行必有 latest，缺键=输出损坏。
    var state: UpdateCheckState {
        if let error, !error.isEmpty {
            return .failed(message: error)
        }
        guard let latest, !latest.isEmpty else {
            return .failed(message: "版本信息缺失")
        }
        if updateAvailable == true {
            return .available(version: latest, asset: asset ?? "")
        }
        return .upToDate(version: latest)
    }
}

/// 检查更新三态（AboutPane 渲染分支；测试锚点）。
enum UpdateCheckState: Equatable {
    case idle
    case upToDate(version: String)
    case available(version: String, asset: String)
    case failed(message: String)
}

/// `self-update --json` 解码模型：成功 `{"ok":true,"updated_to":tag}` / 失败 `{"error":...}`。
struct SelfUpdateReport: Codable, Equatable {
    let ok: Bool?
    let updatedTo: String?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case ok, error
        case updatedTo = "updated_to"
    }

    static func decode(_ line: String) throws -> SelfUpdateReport {
        try decodeJSON(SelfUpdateReport.self, line: line, label: "self-update --json")
    }

    /// updated_to 或抛 error 字段文案（失败 toast=error 字段，batch3 §2.4）。
    func updatedToOrThrow() throws -> String {
        if let error, !error.isEmpty {
            throw CoreBridgeError.cliMessage(error)
        }
        guard let updatedTo, !updatedTo.isEmpty else {
            throw CoreBridgeError.decode("self-update --json: missing updated_to")
        }
        return updatedTo
    }
}

// MARK: - 关于页更新区块模型（SettingsModel 不动约束下的并行实现，performWrite 模式照搬）

/// 关于页"检查更新/立即更新"数据中枢：读走 versionCheckJSON（10s 读档），
/// 写走 selfUpdate（180s updateTimeout 档）；busy 防抖与失败 toast 口径
/// 与 SubsModel.performWrite 同形（超时=可能已部分生效，不回退）。
@MainActor
final class UpdateModel: ObservableObject {
    @Published private(set) var checkState: UpdateCheckState = .idle
    /// 立即更新阶段（done 后 checkState 复查回"已是最新"）
    @Published private(set) var lastUpdatedTo: String?
    @Published private(set) var busy = false
    /// nil = CLI 不可用（壳降级口径：区块只读、按钮禁用）
    private let bridge: CoreBridge?
    var available: Bool { bridge != nil }
    /// 操作结果提示（3s 自动清除，SettingsModel.toast 同形）
    @Published var toast: String?

    init(bridge: CoreBridge?) {
        self.bridge = bridge
    }

    /// 生产构造（CLI 路径解析失败落 nil——区块禁用而非崩溃）。
    static func live() -> UpdateModel {
        UpdateModel(bridge: try? CoreBridge.live())
    }

    /// 检查更新（busy 防抖；失败=三态 failed，文案取 error 字段）。
    func checkForUpdate() async {
        guard let bridge, !busy else { return }
        busy = true
        defer { busy = false }
        do {
            checkState = try await bridge.versionCheckJSON().state
        } catch CoreBridgeError.timedOut {
            checkState = .failed(message: "检查超时，请稍后重试")
        } catch {
            checkState = .failed(message: "检查更新失败")
        }
    }

    /// 立即更新（180s 档；成功 toast + 复查回"已是最新"；失败 toast=error 字段文案）。
    func applyUpdate() async {
        guard let bridge, !busy else { return }
        busy = true
        defer { busy = false }
        do {
            let version = try await bridge.selfUpdate()
            lastUpdatedTo = version
            showToast("已更新至 \(version)，内核已重启（app 本身无需重启；更新过程代理有数秒中断）")
            // 更新后静默复查：区块回到"已是最新"态（失败保持 done 态，不误报）
            if let report = try? await bridge.versionCheckJSON() {
                checkState = report.state
            }
        } catch CoreBridgeError.timedOut {
            showToast("更新超时：可能已部分生效，请稍后用检查更新确认")
        } catch let CoreBridgeError.cliMessage(message) {
            showToast(message) // 失败 toast = error 字段文案（batch3 §2.4）
        } catch {
            showToast("自更新失败")
        }
    }

    /// 测试钩子：置 busy 模拟在途（busy 防抖断言；生产置位在动作内部）。
    func testHooksSetBusy(_ value: Bool) { busy = value }

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
