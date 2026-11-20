import Foundation

/// `settings get --json` 的解码模型（M3-plan §11.3 页 3 数据源；字段名与 Go 侧
/// store.Settings 的 json tag 精确对齐，snake_case 直用零转换层）。
/// WHY var：写后本地即时回显（§11.2-10）按字段改写副本，免整份拷贝构造器。
struct SettingsSnapshot: Codable, Equatable {
    var mixedPort: Int
    var gatewayPort: Int
    var panelEnabled: Bool
    var allowLan: Bool
    var mode: String
    var logLevel: String
    var logRetentionDays: Int
    /// PAC 模式开关（批④ §3.1，Go store.Settings.PacMode 对齐）。
    /// WHY 可选：版本偏斜窗口（新壳 + 旧 CLI）输出缺键——非可选 Bool 解码 keyNotFound
    /// 会让整个 settings 读取失败（ProfileIntervalEntry.updateInterval 同款陷阱）；
    /// nil=旧 CLI 未提供（UI 按 false 处理，legacy 语义即手工模式）。
    var pacMode: Bool?
    /// 绕过域名（逗号分隔存储的数组形态；nil=未设置 → 生效值取缺省集）。
    /// Go omitempty 对 nil slice 缺键，同上必须可选。
    var bypassDomains: [String]?

    enum CodingKeys: String, CodingKey {
        case mixedPort = "mixed_port"
        case gatewayPort = "gateway_port"
        case panelEnabled = "panel_enabled"
        case allowLan = "allow_lan"
        case mode
        case logLevel = "log_level"
        case logRetentionDays = "log_retention_days"
        case pacMode = "pac_mode"
        case bypassDomains = "bypass_domains"
    }

    /// 解码单行 JSON 输出；解析失败显式报错（壳按降级态处理而非崩溃）。
    static func decode(_ line: String) throws -> SettingsSnapshot {
        try decodeJSON(SettingsSnapshot.self, line: line, label: "settings get --json")
    }
}

/// `autostart status --json` 的解码模型（Go autostartStatusJSON 对齐）。
/// installed：plist 已安装；loaded：launchd 已加载；两者都真 = 自启已生效。
struct AutostartStatus: Codable, Equatable {
    let installed: Bool
    let loaded: Bool

    /// 开关态口径：plist + launchd 双真（installed 未 loaded 需下次登录才生效）。
    var enabled: Bool { installed && loaded }

    static func decode(_ line: String) throws -> AutostartStatus {
        try decodeJSON(AutostartStatus.self, line: line, label: "autostart status --json")
    }
}

/// `list --json` 单条目的间隔初值裁剪解码（通用页 interval select 初值取自当前
/// 条目，M3-plan §11.3 评审 P3；完整订阅卡模型归 c3 SubsModel）。字段命名与 Go
/// ProfileJSON 对齐；allow_auto_update 指针语义与 Go 一致：nil=未设置（默认允许）。
struct ProfileIntervalEntry: Codable, Equatable {
    let current: Bool
    let allowAutoUpdate: Bool?
    /// WHY 可选（c2 审查 P1）：Go ProfileJSON 的 update_interval 带 omitempty——
    /// off 档写 0、新订阅恒 0，两种真实输出该键**缺省**而非为零值；非可选解码
    /// keyNotFound → try? 吞掉 → off 档回读静默显示"每 24h"（与真实状态相反）
    let updateInterval: Int?

    enum CodingKeys: String, CodingKey {
        case current
        case allowAutoUpdate = "allow_auto_update"
        case updateInterval = "update_interval"
    }

    /// 解码单行 JSON 数组（空表输出 []）。
    static func decodeArray(_ line: String) throws -> [ProfileIntervalEntry] {
        try decodeJSON([ProfileIntervalEntry].self, line: line, label: "list --json")
    }
}

/// 单行 JSON 解码统一入口（StatusReport.decode 的去重形态；标签仅入错误文案，
/// 不含任何本机路径/token）。c3 起 SubsModel 的订阅卡解码共用本入口。
func decodeJSON<T: Decodable>(_ type: T.Type, line: String, label: String) throws -> T {
    guard let data = line.data(using: .utf8) else {
        throw CoreBridgeError.decode("\(label): output is not UTF-8")
    }
    do {
        return try JSONDecoder().decode(type, from: data)
    } catch {
        throw CoreBridgeError.decode("\(label): \(error.localizedDescription)")
    }
}

// MARK: - 壳版本展示（批③ B3 v1.2 P2-3 全形契约：display 返回含 v 展示形态）

/// 壳版本展示（契约改写）：返回**含 "v" 前缀的全形**——"v0.5.0-m3c1"；
/// dev 原样返回；已带 v 前缀不动；裸版本号（数字开头）补 v。空串原样
///（兜底值由调用方决定）。AboutPane 直用、不再手写 "v" 前缀
///（vdev 缺陷根源=手写插值，v1.2 评审 P2-3）。
enum ShellVersion {
    static func display(_ raw: String) -> String {
        guard !raw.isEmpty, raw != "dev" else { return raw }
        if let first = raw.first, first == "v" {
            return raw // 已含前缀：原样（不做双写防御性改写）
        }
        guard let first = raw.first, first.isNumber else {
            return raw // 非版本语义的注入值（纯哈希等）：不做假设性改写
        }
        return "v" + raw
    }
}
