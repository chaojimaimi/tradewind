import Foundation

// MARK: - 绕过域名编辑判定纯函数（批④ §3.4；可单测）

/// 绕过域名编辑分类（Go store.Settings 白名单同口径，壳侧前置拦截给出有因由的
/// toast；Go Validate 落盘前仍是最终闸门——手改 YAML 直达面在 Go 侧拦）。
enum BypassEditorLogic {
    /// 条数上限（Go MaxBypassDomains 同值）。
    static let maxEntries = 64
    /// 单条 pattern 长度上限（域名上限口径）。
    static let maxPatternLength = 253
    /// Go 缺省集展示形态（未设置时的生效值）。
    static let defaultDisplay = "127.0.0.1,localhost,*.local,169.254/16"

    enum Commit: Equatable {
        case unchanged // 与生效值相同（规范化后）：不写
        case invalid(reason: String) // 非法条目/超限：拒绝提交
        case write([String]) // 写入该列表（空=恢复缺省，Go 空=恢复缺省同语义）
    }

    /// 白名单正则（Go bypassPatternRe 同口径：域名/通配/CIDR/<local>；引号/分号/
    /// 空格/& 全拒——PAC JS 注入面）。
    private static let patternRe = try? NSRegularExpression(
        pattern: "^[A-Za-z0-9._*?<>/-]{1,253}$")

    /// 逗号分隔解析（trim、丢空段；空文本=恢复缺省）。
    static func parse(_ text: String) -> [String] {
        text.split(separator: ",")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
    }

    /// 当前生效值展示形态（未设置 → 缺省集；编辑行占位）。
    static func effectiveDisplay(current: [String]?) -> String {
        guard let current, !current.isEmpty else { return defaultDisplay }
        return current.joined(separator: ",")
    }

    /// 提交分类：非法条目先行（逐条白名单+长度）、条数上限、unchanged 短路。
    static func classify(_ text: String, current: [String]?) -> Commit {
        let entries = parse(text)
        if entries.isEmpty {
            // 空文本=恢复缺省；当前已是缺省（nil/空）则不写
            if current == nil || current!.isEmpty { return .unchanged }
            return .write([])
        }
        var invalid: [String] = []
        for entry in entries {
            if entry.utf8.count > maxPatternLength || !matchesWhitelist(entry) {
                invalid.append(entry)
            }
        }
        if !invalid.isEmpty {
            return .invalid(reason: "非法绕过域名（仅允许域名/通配/CIDR/<local>）："
                + invalid.prefix(3).joined(separator: ", "))
        }
        if entries.count > maxEntries {
            return .invalid(reason: "绕过域名最多 \(maxEntries) 条")
        }
        if entries == (current ?? []) { return .unchanged }
        return .write(entries)
    }

    private static func matchesWhitelist(_ entry: String) -> Bool {
        guard let patternRe else { return false }
        let range = NSRange(entry.startIndex..., in: entry)
        return patternRe.firstMatch(in: entry, range: range) != nil
    }
}

// MARK: - 系统代理形态编辑模型（批④ §3.4）

/// PAC 模式 + 绕过域名编辑数据中枢。WHY 新模型而非挂 SettingsModel：SettingsModel
/// 不动的约束（批④纪律 8）；数据链同源——settingsGetJSON 读、settingsSet 写
/// （65s write 档，双轨制不走 @AppStorage）。busy 防抖与 toast 口径照 SubsModel
/// 平行实现（SettingsModel.performWrite 模式勿动约束下的既有惯例）。
@MainActor
final class ProxyModeModel: ObservableObject {
    /// PAC 模式开关（乐观回显；写编排含翻转重放，权威态写后重拉）。
    @Published private(set) var pacMode = false
    /// 当前生效 bypass（权威态；nil=未设置 → 生效值取 Go 缺省集）。
    @Published private(set) var currentBypass: [String]?
    @Published private(set) var busy = false
    /// 初始加载失败标记（区分"加载中"与失败态）。
    @Published private(set) var loadFailed = false
    /// 操作结果提示（3s 自动清除，SettingsModel.toast 同形）。
    @Published var toast: String?

    /// nil = CLI 不可用（降级：控件禁用，GeneralPane 说明行兜底）。
    private let bridge: CoreBridge?
    var available: Bool { bridge != nil }

    init(bridge: CoreBridge?) {
        self.bridge = bridge
    }

    /// 生产构造（CLI 路径解析失败落 nil——页面降级；EnhanceModel.live 同先例）。
    static func live() -> ProxyModeModel {
        ProxyModeModel(bridge: try? CoreBridge.live())
    }

    /// 页面装载（GeneralPane 出现时；写后权威态经 refresh 无 busy 闸重拉）。
    func load() async {
        guard let bridge, !busy else { return }
        loadFailed = false
        do {
            let snapshot = try await bridge.settingsGetJSON()
            pacMode = snapshot.pacMode ?? false
            currentBypass = snapshot.bypassDomains
        } catch {
            loadFailed = true
            showToast("设置读取失败")
        }
    }

    /// PAC 模式开关（→ settings set pac_mode on|off；写编排含 applied 态翻转重放）。
    func setPacMode(_ on: Bool) async {
        let old = pacMode
        pacMode = on // 乐观回显（65s 写编排期 UI 不悬空）
        await performWrite(success: on ? "PAC 模式已开启（需面板网关运行）" : "已切换为手工代理模式",
                           failure: "PAC 模式保存失败",
                           onRevert: { self.pacMode = old }) {
            try await self.bridge?.settingsSet(key: "pac_mode", value: on ? "on" : "off")
            await self.refresh()
        }
    }

    /// 绕过域名提交（BypassEditorLogic 分类：unchanged 短路/非法拒绝/空=恢复缺省）。
    func commitBypass(_ raw: String) async {
        switch BypassEditorLogic.classify(raw, current: currentBypass) {
        case .unchanged:
            return
        case .invalid(let reason):
            showToast("保存失败：\(reason)")
        case .write(let entries):
            await performWrite(success: "绕过域名已更新", failure: "绕过域名保存失败") {
                try await self.bridge?.settingsSet(key: "bypass_domains",
                                                   value: entries.joined(separator: ","))
                await self.refresh()
            }
        }
    }

    /// 测试钩子：置 busy 模拟在途（busy 防抖断言）。
    func testHooksSetBusy(_ value: Bool) { busy = value }

    // MARK: 私有

    /// 写动作统一包装（busy 防抖 + 成功/失败 toast + 失败回退；超时=可能已部分生效）。
    @discardableResult
    private func performWrite(success: String, failure: String,
                              onRevert: (() -> Void)? = nil,
                              mutation: () async throws -> Void) async -> Bool {
        guard available, !busy else {
            onRevert?()
            return false
        }
        busy = true
        defer { busy = false }
        do {
            try await mutation()
            showToast(success)
            return true
        } catch CoreBridgeError.timedOut {
            showToast("保存超时：可能已部分生效，请重开设置窗口确认")
        } catch {
            onRevert?()
            showToast(failure)
        }
        return false
    }

    /// 写后权威态重拉（无 busy 闸——mutation 闭包内调用；静默失败保持旧值）。
    private func refresh() async {
        guard let bridge else { return }
        if let snapshot = try? await bridge.settingsGetJSON() {
            pacMode = snapshot.pacMode ?? false
            currentBypass = snapshot.bypassDomains
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
