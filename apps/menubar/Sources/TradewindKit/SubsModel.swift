import Foundation

// MARK: - list --json 订阅卡解码模型（M3-plan §11.3 页 1；字段对照 app.ProfileJSON）

/// 流量信息（store.ExtraInfo 对齐；单位字节；expire=unix 秒，0=未知）。
struct SubsExtraTraffic: Codable, Equatable {
    var upload: Int64
    var download: Int64
    var total: Int64
    var expire: Int64
}

/// `list --json` 单条完整解码（Go app.ProfileJSON 对齐，snake_case 直用）。
/// WHY 两个可选（c2 审查 P1 同款陷阱，勿改非可选）：Go omitempty——
/// update_interval 在 off 档（写 0）/新订阅（恒 0）输出**缺键**；allow_auto_update
/// 为指针（nil=未设置=默认允许）且 omitempty 吞 nil。
struct SubsCardEntry: Codable, Equatable {
    let current: Bool
    let uid: String
    let name: String
    let type: String
    /// 订阅源 URL（remote 型；file 型/旧 CLI 缺省 nil——c3 保真：卡片元信息行 host）
    let url: String?
    /// 最近导入/更新的 unix 秒
    let updated: Int64
    var extra: SubsExtraTraffic
    let allowAutoUpdate: Bool?
    let updateInterval: Int?

    enum CodingKeys: String, CodingKey {
        case current, uid, name, type, url, updated, extra
        case allowAutoUpdate = "allow_auto_update"
        case updateInterval = "update_interval"
    }

    /// 解码单行 JSON 数组（空表输出 []）。
    static func decodeArray(_ line: String) throws -> [SubsCardEntry] {
        try decodeJSON([SubsCardEntry].self, line: line, label: "list --json")
    }
}

// MARK: - 订阅页可测逻辑（纯函数，Swift Testing 直调；M3-plan §11.3 页 1）

/// 订阅卡展示判定集合（demo :765-806 renderSubs 数据口径的壳侧映射）。
enum SubsPaneLogic {
    /// 流量条警示阈值（demo `pct>85?'warn':''`，严格大于）
    static let trafficWarnPercent = 85.0

    /// 已用流量 = 上行 + 下行；负值防御归零（异常 subscription-userinfo 头）
    static func usedBytes(upload: Int64, download: Int64) -> Int64 {
        let raw = upload + download
        return raw > 0 ? raw : 0
    }

    /// 流量百分比（0-100，封顶 100）；total<=0 = 不限量/未知 → nil（不出条不出百分比）
    static func trafficPercent(used: Int64, total: Int64) -> Double? {
        guard total > 0 else { return nil }
        return min(Double(used) / Double(total) * 100, 100)
    }

    /// 流量条警示判定（>85% warn 色；nil=不限量不警示）
    static func isTrafficWarn(_ percent: Double?) -> Bool {
        guard let percent else { return false }
        return percent > trafficWarnPercent
    }

    /// 流量字节格式化（demo 单位面 TB/GB/MB/KB/B；RateText.bytes 不含 TB，
    /// 总额动辄数十 TB 会溢出为 GB 长串，故不复用）
    static func trafficBytes(_ value: Int64) -> String {
        let v = Double(max(value, 0))
        switch v {
        case 1e12...: return String(format: "%.1f TB", v / 1e12)
        case 1e9...: return String(format: "%.1f GB", v / 1e9)
        case 1e6...: return String(format: "%.1f MB", v / 1e6)
        case 1e3...: return String(format: "%.0f KB", v / 1e3)
        default: return String(format: "%.0f B", v)
        }
    }

    /// 百分比文本（demo toFixed(1)%；nil=不限量显示 "—"）
    static func percentText(_ percent: Double?) -> String {
        guard let percent else { return "—" }
        return String(format: "%.1f%%", percent)
    }

    /// unix 秒 → formatter 格式日期；非正数 → nil（到期 0=未知，demo expire
    /// '未知' 口径）。formatter 注入便于测试锚定。
    static func dateText(_ unix: Int64, formatter: DateFormatter) -> String? {
        guard unix > 0 else { return nil }
        return formatter.string(from: Date(timeIntervalSince1970: TimeInterval(unix)))
    }

    /// 卡片日期 formatter（"yyyy-MM-dd HH:mm"；设置窗口紧凑口径）。
    /// WHY 每次构造：DateFormatter 非 Sendable，不做静态共享（v5 语言模式下
    /// 调用点少、构造开销可接受）。
    static func makeCardDateFormatter() -> DateFormatter {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyy-MM-dd HH:mm"
        return formatter
    }
}

// MARK: - 订阅管理页模型（M3-plan §11.3 页 1）

/// 订阅页数据中枢：读走 subsListJSON（list --json），写 add/update/use/delete
/// 全走 65s 加长超时 runner（写编排持 mutation.lock + 合成热重载链，§11.2-4）。
/// 写动作策略＝"成功后重拉权威态"（订阅集结构变化大，乐观合并本地不划算）；
/// busy 防抖与超时语义照 SettingsModel.performWrite 模式（勿动该文件的约束下
/// 平行实现，超时=可能已部分生效，不回退）。
@MainActor
final class SubsModel: ObservableObject {
    @Published private(set) var subs: [SubsCardEntry] = []
    @Published private(set) var busy = false
    /// 列表读取失败标记（区分"加载中"与失败态；写后刷新失败不置位——写已成功）
    @Published private(set) var loadFailed = false
    /// 操作结果提示（3s 自动清除，SettingsModel.toast 同形）
    @Published var toast: String?

    /// nil = CLI 不可用（壳降级口径：页面只读、写控件禁用）
    private let bridge: CoreBridge?
    var available: Bool { bridge != nil }

    /// remote 订阅的 type 标记（Go ProfileItem.Type：url=远端可拉取，file=本地导入）
    private static let remoteType = "url"

    init(bridge: CoreBridge?) {
        self.bridge = bridge
    }

    /// 页面装载（窗口打开/首次进入）：失败置 loadFailed + toast 明示；
    /// 写在途不覆盖（SettingsModel.load 同款守卫）。
    func load() async {
        guard let bridge, !busy else { return }
        loadFailed = false
        do {
            subs = try await bridge.subsListJSON()
        } catch {
            loadFailed = true
            showToast("订阅列表读取失败")
        }
    }

    // MARK: 写动作（成功重拉权威态 + toast；失败 toast 不改本地）

    /// 添加订阅（URL 导入，名称空白 → Go 侧默认取 URL host）。
    /// 返回是否成功（视图据此收起 sheet；URL 空为壳侧前置校验，不落 CLI）。
    @discardableResult
    func add(url: String, name: String) async -> Bool {
        let trimmedURL = url.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedURL.isEmpty else {
            showToast("请输入订阅链接")
            return false
        }
        let trimmedName = name.trimmingCharacters(in: .whitespacesAndNewlines)
        return await performWrite(success: "订阅已导入", failure: "订阅导入失败") {
            try await self.bridge?.subsAdd(url: trimmedURL, name: trimmedName)
            await self.reloadList()
        }
    }

    /// 单订阅更新（卡片 ↻ 更新）。
    func update(_ entry: SubsCardEntry) async {
        await performWrite(success: "订阅已更新", failure: "订阅更新失败") {
            try await self.bridge?.subsUpdate(target: self.writeTarget(entry))
            await self.reloadList()
        }
    }

    /// 全部更新（↻ 全部更新）：逐个 remote（type=url）订阅 update——file 型订阅
    /// 无远端可拉，跳过。整组一个 busy 周期，结尾统一重拉 + 汇总 toast。
    func updateAll() async {
        guard let bridge else { return }
        let targets = subs.filter { $0.type == Self.remoteType }
        guard !targets.isEmpty else {
            showToast("没有可更新的订阅")
            return
        }
        guard !busy else { return }
        busy = true
        defer { busy = false }
        var failed = 0
        for entry in targets {
            do {
                try await bridge.subsUpdate(target: writeTarget(entry))
            } catch {
                failed += 1 // 单个失败不中断其余订阅的更新
            }
        }
        await reloadList()
        showToast(failed == 0 ? "全部订阅已更新" : "部分订阅更新失败（\(failed) 个）")
    }

    /// 设为当前订阅（use：合成 + 热重载）。
    func use(_ entry: SubsCardEntry) async {
        await performWrite(success: "已切换当前订阅（\(entry.name)）", failure: "切换订阅失败") {
            try await self.bridge?.subsUse(target: self.writeTarget(entry))
            await self.reloadList()
        }
    }

    /// 删除订阅（confirm 对话框由视图层负责）。current 传 --force（UI 二次确认
    /// 强制语义）；删除当前订阅后裸内核态提示对齐 Go CLI 输出文案（§11.2-5）。
    func delete(_ entry: SubsCardEntry) async {
        let wasCurrent = entry.current
        let success = wasCurrent
            ? "当前订阅已删除，Tradewind 处于裸内核态，请导入或 use 订阅"
            : "订阅已删除（\(entry.name)）"
        await performWrite(success: success, failure: "订阅删除失败") {
            try await self.bridge?.subsDelete(target: self.writeTarget(entry), force: wasCurrent)
            await self.reloadList()
        }
    }

    /// 测试钩子：置 busy 模拟在途（busy 防抖断言；生产置位在各写动作内部）。
    func testHooksSetBusy(_ value: Bool) { busy = value }

    // MARK: 私有

    /// 写动作统一包装：busy 防抖（在途拒绝提交）+ 成功/失败 toast；
    /// 超时=可能已部分生效（合成/热重载链中途被终止），不静默按失败处理。
    /// WHY available 而非 guard let bridge：mutation 闭包经 self.bridge? 调用
    /// （闭包内捕获），此处仅需可用性判定。
    @discardableResult
    private func performWrite(success: String, failure: String,
                              mutation: () async throws -> Void) async -> Bool {
        guard available, !busy else { return false }
        busy = true
        defer { busy = false }
        do {
            try await mutation()
            showToast(success)
            return true
        } catch CoreBridgeError.timedOut {
            showToast("操作超时：可能已部分生效，请重开窗口确认")
            return false
        } catch {
            showToast(failure)
            return false
        }
    }

    /// 写后权威态重拉（静默失败：列表保持旧值——写本身已成功，仅刷新失败，
    /// 不得置 loadFailed 把成功写误报为页面故障）。
    private func reloadList() async {
        guard let bridge else { return }
        subs = (try? await bridge.subsListJSON()) ?? subs
    }

    /// CLI 写目标：uid 优先（Go resolveProfile 按 uid/name 精确匹配）；uid 空回退
    /// name。WHY 不传空串：`update` 的空参数会被 CLI 改写为 "current" 关键字，
    /// 而 resolveProfile 不支持该关键字（subsIntervalCurrent 注释同源事实）。
    private func writeTarget(_ entry: SubsCardEntry) -> String {
        entry.uid.isEmpty ? entry.name : entry.uid
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
