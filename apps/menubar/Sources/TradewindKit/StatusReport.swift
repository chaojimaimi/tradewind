import Foundation

/// `tradewind status --json` 的解码模型（壳控制面唯一状态源，M2-plan §2.5）。
/// 字段名与 Go 侧 StatusJSON 精确对齐（snake_case 直用，零转换层）。
struct StatusReport: Codable, Equatable {
    struct Core: Codable, Equatable {
        let running: Bool
        let pid: Int
        let version: String
    }

    struct SysProxy: Codable, Equatable {
        let applied: Bool
        let services: Int
    }

    struct Profile: Codable, Equatable {
        let uid: String
        let name: String
    }

    let core: Core
    let mode: String
    let sysProxy: SysProxy
    let profile: Profile
    let panelURL: String

    enum CodingKeys: String, CodingKey {
        case core
        case mode
        case sysProxy = "sys_proxy"
        case profile
        case panelURL = "panel_url"
    }

    /// 解码单行 JSON 输出；解析失败显式报错（壳按降级态处理而非崩溃）。
    static func decode(_ line: String) throws -> StatusReport {
        let decoder = JSONDecoder()
        guard let data = line.data(using: .utf8) else {
            throw CoreBridgeError.decode("status output is not UTF-8")
        }
        do {
            return try decoder.decode(StatusReport.self, from: data)
        } catch {
            throw CoreBridgeError.decode("status --json: \(error.localizedDescription)")
        }
    }
}

/// 壳侧错误类型：面向用户可读，绝不回显进程内部细节（stderr 可能含路径）。
enum CoreBridgeError: Error, Equatable {
    case cliNotFound
    case cliFailed(command: String)
    case decode(String)
    case badPanelURL(String)
    /// 无当前订阅（subsIntervalCurrent 的 current 定位落空；status --json profile.uid 为空）
    case noCurrentProfile
    /// CLI 写编排超时被壳终止（c2 审查 P2）：settings.yaml 先于 reload 落盘，
    /// 超时=可能已部分生效——消费方不得按普通失败回退乐观回显
    case timedOut(command: String)
    /// CLI --json 契约的 error 字段文案（批③：version --check / self-update /
    /// enhance set|clear 恒退出 0，失败信息在 stdout JSON error 字段——直接透传
    /// 给 toast/三态展示，不掺壳侧措辞）
    case cliMessage(String)
}
