import AppKit
import Combine
import Foundation
import os

/// 策略组视图模型（/proxies 裁剪）：组内成员按实测延迟升序（v0.4.15，用户指令
/// "延迟最低排最上"——偏离 demo 的订阅配置顺序，记偏差）。
struct ProxyGroup: Identifiable, Equatable {
    struct Node: Identifiable, Equatable {
        let id: String
        let name: String
        /// 最近延迟（毫秒）；nil=未测（灰点）
        let delay: Int?
        /// 成员本身是策略组=嵌套组，视图据此渲染「组」徽标（v0.4.4）
        let isGroup: Bool
    }

    let id: String
    let name: String
    /// mihomo 组类型（Selector/URLTest/Fallback/LoadBalance/Relay），展示名映射依据
    let type: String
    let nodes: [Node]
    let currentNode: String?
    /// URLTest/Fallback 组的手动固定节点名（v0.4.16；nil=未固定=自动选择中）
    let fixed: String?

    /// 出站模式徽标（v0.4.16 用户拍板"全自动/手动固定两种模式可视化"）。
    /// 锚定当前查看组的出口语义：Selector 出口落嵌套组=自动引擎接管；落节点=手动；
    /// URLTest 看内核 fixed 字段（固定态 vs 自动选择中）。其余类型无此语义不显示。
    var modeBadge: String? {
        switch type {
        case "Selector":
            guard let now = currentNode else { return nil }
            return nodes.first(where: { $0.name == now })?.isGroup == true ? "自动" : "手动"
        case "URLTest", "Fallback":
            return fixed == nil ? "自动" : "已固定"
        default:
            return nil
        }
    }

    /// 策略组展示形态（v0.4.15，用户反馈 pill 显示"Auto/Proxy"与 demo 的
    /// "节点选择/自动选择/全球直连"不一致）：中文名+图标按 demo 语义映射。
    /// WHY 名字表优先于类型：订阅可含多个同类型组（如多个地区 Selector），
    /// 全按类型映射会重名；仅常见英文名（demo mock 形态）映射为 demo 中文语义。
    /// 未命中组保持原名+类型图标（不丢区分度）。
    var display: (icon: String, name: String) {
        switch name {
        case "Proxy", "节点选择", "手动选择", "手动切换":
            return ("🚀", "节点选择")
        case "Auto", "自动选择", "自动":
            return ("♻️", "自动选择")
        case "DIRECT", "全球直连", "直连":
            return ("🎯", "全球直连")
        default:
            switch type {
            case "Selector": return ("🚀", name)
            case "URLTest": return ("♻️", name)
            case "Fallback": return ("🪂", name)
            case "LoadBalance": return ("⚖️", name)
            case "Relay": return ("🔗", name)
            default: return ("", name)
            }
        }
    }
}

/// popover 主面板状态（demo 主面板语义，M2-plan §5）。
/// 降级契约：panel_url 缺失/网关不可达 → degraded=true 显示"面板数据不可用"，
/// 控制面（CLI）仍可用（M2-plan §5 v1.1）。
@MainActor
final class PanelModel: ObservableObject {
    @Published var report: StatusReport?
    @Published var groups: [ProxyGroup] = []
    @Published var activeGroupID: String?
    /// true = 面板数据不可用（无 panel_url 或网关拉取失败）
    @Published var degraded = true
    @Published var busy = false
    /// 操作失败的用户可读提示（toast 位）；自动 3s 清除
    @Published var toast: String?
    /// 乐观值（v0.4.7 方案 B）：动作发起即翻转 UI，refresh 权威值落地后清除；
    /// 失败清除即视觉回退 + toast。nil = 无在途动作，一切以 report 权威值为准。
    /// WHY 2026-11-19 用户复测"点击仍有卡顿（较前减轻）"——async 化后主线程已不阻塞，
    /// 残余是状态确认延迟（sysproxy 走 networksetup 数百 ms-秒级）；乐观翻转给出即时反馈。
    @Published private(set) var pendingSysProxy: Bool?
    /// 同上，主开关（start/stop）的乐观值。
    @Published private(set) var pendingRunning: Bool?
    /// 同上，TUN 开关（批⑥ §4）的乐观值——90s 编排（首装含提权弹窗）期 UI 不悬空。
    @Published private(set) var pendingTUN: Bool?
    private(set) var gateway: GatewayClient?
    private let bridge: CoreBridge?
    /// 网关会话注入点（URLProtocol stub 单测；生产默认 .shared）
    private let gatewaySession: URLSession
    /// WS 速率流（速率卡 + 菜单栏速率文本的数据源）
    let traffic: TrafficStore
    /// popover 开合事件（壳装配层在 show 时发送；面板据此刷新）
    let popoverDidShow = PassthroughSubject<Void, Never>()
    /// v0.4.5 openPanel 诊断面包屑：用户端"面板依然打不开"且 v0.4.2 失败分支未见触发，
    /// 经统一日志区分"动作未触发 / open 返回 false / CLI 抛错"三情形。
    /// WHY 日志绝不携带 URL/token 本体（token 内嵌 panel URL，落日志即泄露）；
    /// 排查后保留（个人工具，无害且利于日后定位）。
    private static let panelLog = Logger(subsystem: "io.github.chaojimaimi.tradewind", category: "panel")

    init(bridge: CoreBridge?, session: URLSession = .shared, traffic: TrafficStore? = nil) {
        self.bridge = bridge
        self.gatewaySession = session
        self.traffic = traffic ?? TrafficStore()
    }

    /// 壳装配层：popover 展示时触发一次刷新。
    func notifyPopoverShown() {
        popoverDidShow.send(())
    }

    /// 默认模型：live CLI 桥；CLI 不可用仍可进面板（控制面禁用 + 明确提示）。
    static func live() -> PanelModel {
        do {
            return PanelModel(bridge: try CoreBridge.live())
        } catch {
            let model = PanelModel(bridge: nil)
            model.toast = "tradewind CLI 未找到（设 TRADEDWIND_CLI 或安装）"
            return model
        }
    }

    // MARK: 派生状态（demo 状态行/图标口径）

    var running: Bool { report?.core.running ?? false }
    /// 派生口径（v0.4.7 乐观优先）：pending 在途即取乐观值，UI 点击立即翻转；
    /// refresh 权威值落地后 pending 清除，自动回权威口径。
    var effectiveSysProxy: Bool { pendingSysProxy ?? report?.sysProxy.applied ?? false }
    var effectiveRunning: Bool { pendingRunning ?? running }
    /// TUN 开关 effective 口径（批⑥ §4）：pending 优先，权威值取 status --json tun
    /// 段（nil=旧 CLI 缺键 → false 渲染）。running=enabled 且服务托管内核健康
    /// （Go 侧三态：enabled+running / enabled+服务异常 / disabled）。
    var effectiveTUN: Bool { pendingTUN ?? (report?.tun?.enabled ?? false) }
    var tunRunning: Bool { report?.tun?.running ?? false }
    /// TUN 行副注（三态呈现，§4"单图标+菜单态文案"）：服务异常态显著提示。
    var tunNote: String? {
        guard effectiveTUN, report != nil, !tunRunning else { return nil }
        return "服务异常"
    }
    /// 接入路径：系统代理或 TUN 至少一开（demo 口径 sysProxy || tun；批⑥ TUN 转正）。
    /// WHY effective 口径：菜单栏图标绿点随开关点击立即翻转（v0.4.7）
    /// WHY 不按 FeatureFlags.privilegedTUN 关门（M3 batch6 release-gate）：开关管的是
    /// "用户能否在壳里点 TUN"，不是"本机是否实际在走 TUN"——经 CLI 显式确认开启的
    /// 会话，status --json 的 tun 段如实报告 enabled，图标应照常显示有接入。本属性是
    /// 纯计算：不订阅、不触发额外的解码或刷新（tun 段的解码在 StatusReport.decode，
    /// 有意保留），所以"不订阅"也省不下任何工作。
    var intake: Bool { effectiveSysProxy || effectiveTUN }
    var iconState: StatusIconState { StatusIcon.state(running: effectiveRunning, intake: intake) }
    /// 当前节点延迟：activeGroup.nodes 中 currentNode 匹配项的 delay。
    /// WHY 下沉为模型计算属性：视图内查找无法单测（M3-plan §3.2）
    var currentNodeDelay: Int? {
        guard let group = activeGroup, let node = group.currentNode else { return nil }
        return group.nodes.first { $0.name == node }?.delay
    }

    /// 链式解析：当前节点是嵌套组时沿 now 逐层下探至尾节点（v0.4.10 方案 B）。
    /// WHY 2026-11-19 用户拍板方案 B：Proxy(Selector).now 可能是嵌套组（如 Auto），
    /// 真实链路 组A→Auto→香港节点；旧卡片在嵌套态显示「Auto [组] —」末端信息缺失。
    /// 返回 names（首位为当前组名，其后自 activeGroup.currentNode 起依次为链上
    /// 嵌套组名与尾节点名）与 tailDelay（尾节点延迟毫秒；链尾仍是组=深度截断/环、
    /// 或全局查无该节点延迟记录时为 nil）。当前节点是普通节点时返回 nil（卡片维持
    /// 单节点形态）。组名入链上限 3、尾节点另计（names 至多 4）+ names.contains
    /// 防环（code-reviewer P3：注释精度修正），保证有界终止。
    func exitChain() -> (names: [String], tailDelay: Int?)? {
        guard let group = activeGroup, let start = group.currentNode,
              groups.contains(where: { $0.id == start }) else { return nil }
        var names = [group.name]
        var current = start
        // 自指防护（code-reviewer P3）：now==自身名时直接收尾，避免 names 重复项
        // 破坏 ForEach(id: \.self) 唯一性不变量（规整内核数据不可达，防御性收口）
        if current == group.name {
            return (names, nil)
        }
        while names.count < 3, let nested = groups.first(where: { $0.id == current }) {
            names.append(nested.name)
            guard let next = nested.currentNode, !names.contains(next) else { break }
            current = next
        }
        // 尾节点入链：链尾落在普通节点才追加；仍是组（截断/环）保持组名收尾
        if !names.contains(current), !groups.contains(where: { $0.id == current }) {
            names.append(current)
        }
        let tail = names.last ?? group.name
        if groups.contains(where: { $0.id == tail }) {
            return (names, nil) // 链尾仍是组：无尾节点延迟语义
        }
        // 尾节点延迟：全局按名查找（遍历各组 nodes 找同名成员），查无记录为 nil
        let tailDelay = groups.flatMap(\.nodes).first { $0.name == tail }?.delay
        return (names, tailDelay)
    }

    // MARK: 刷新与动作

    /// 拉取 status + 节点列表；失败不抛出（降级态呈现）。
    func refresh() async {
        guard let bridge else { return }
        do {
            let report = try await bridge.status()
            self.report = report
            gateway = GatewayClient(panelURL: report.panelURL, session: gatewaySession)
            traffic.attach(gateway: gateway)
            await refreshGroups()
        } catch {
            degraded = true
            traffic.attach(gateway: nil)
            showToast("状态获取失败：CLI 不可用")
        }
    }

    /// 主开关：开=detached start（壳是观察者），关=stop（优雅还原）。
    /// 乐观更新（v0.4.7）：入口即置 pendingRunning 翻转 UI；CLI 失败清除（视觉回退），
    /// 终态统一清 pending 交接权威口径。WHY 见 pendingRunning 定义处。
    /// WHY 入口 busy 守卫（批② c5，M3-plan §11.2-7）：快捷键与 popover 开关共享本路径，
    /// busy 在途时拒绝重入——toggle 期间 popover 点击与快捷键连按都被防抖。
    func toggleMaster(on: Bool) async {
        guard let bridge, !busy else { return }
        pendingRunning = on
        busy = true
        defer { busy = false }
        do {
            if on {
                try bridge.startCoreDetached()
                showToast("Tradewind 已启动：内核就绪后自动刷新")
                // WHY P2（2026-11-19 审查折入）：startCoreDetached 是纯 spawn，内核 socket
                // 就绪晚于首个 status 探测——原先"refresh 一次即无条件清 pending"会让
                // 权威值（未起）先落地，图标"绿→灰→绿"闪回，每次冷启动必现。改有界轮询：
                // 保留 pendingRunning 反复 refresh，running 命中 true 即 break（权威值
                // 接管=运行）；5 轮仍未起则回落停止态（真未起，如实呈现，不无限乐观）。
                for _ in 0..<5 {
                    await refresh()
                    if running { break }
                    try? await Task.sleep(nanoseconds: 1_000_000_000)
                }
            } else {
                try await bridge.stopCore()
                showToast("Tradewind 已停止：系统代理已按备份精确还原")
                await refresh() // stop 管线同步生效，无 P2 时序，权威值一次落地即交接
            }
        } catch {
            pendingRunning = nil
            showToast(on ? "启动失败" : "停止失败")
        }
        pendingRunning = nil
    }

    /// 全局快捷键启停动作（M3-plan §11.2-7，批② c5）：方向判定用**现场 status 探测**
    /// （评审 P2：收起态 report 每 30s 才刷新一次，按缓存判定在 stale 窗口会双 start/
    /// 误 stop），随后复用 toggleMaster——busy 防抖、乐观翻转、有界轮询、toast 全部
    /// 与 popover 主开关同路（共享 busy：toggle 期间 popover 点击同样被拒）。
    func toggleFromHotkey() async {
        guard let bridge, !busy else { return }
        guard let report = try? await bridge.status() else {
            showToast("状态探测失败：快捷键未执行")
            return
        }
        await toggleMaster(on: !report.core.running)
    }

    /// 出站模式切换（PATCH 热生效；settings 回写由 CLI 保证崩溃不回退）。
    func setMode(_ mode: String) async {
        guard let bridge else { return }
        do {
            try await bridge.setMode(mode)
        } catch {
            showToast("模式切换失败")
        }
        await refresh()
    }

    /// 系统代理开关（幂等；on 需内核可达——CLI 侧守卫，失败明确报错）。
    /// 乐观更新（v0.4.7）：入口即置 pendingSysProxy 翻转 UI，CLI 失败清除（视觉回退），
    /// refresh 权威值落地后清除（交接给权威口径）。WHY 见 pendingSysProxy 定义处。
    func setSysProxy(on: Bool) async {
        guard let bridge else { return }
        pendingSysProxy = on
        do {
            try await bridge.sysProxy(on: on)
        } catch {
            pendingSysProxy = nil
            showToast(on ? "系统代理开启失败（内核未运行？）" : "系统代理关闭失败")
        }
        await refresh()
        pendingSysProxy = nil
    }

    /// TUN 模式开关（批⑥ §4 → `tun on|off`，90s 档：首装含提权弹窗等待）。
    /// 乐观回显同 setSysProxy（pendingTUN）；失败视觉回退 + toast。
    /// WHY 不设 busy 独占重入守卫：Go 编排幂等（已是目标态 no-op），双发无害。
    func setTUN(on: Bool) async {
        guard let bridge else { return }
        pendingTUN = on
        do {
            try await bridge.setTUN(enabled: on)
        } catch {
            pendingTUN = nil
            showToast(on ? "TUN 开启失败（提权被取消或服务安装失败）" : "TUN 关闭失败")
        }
        await refresh()
        pendingTUN = nil
    }

    /// 切换节点：PUT 成功后本地乐观更新（内核失败不更新，重拉修正）。
    func selectNode(_ node: String) async {
        guard let group = activeGroup, let bridge else { return }
        do {
            try await bridge.select(group: group.id, node: node)
        } catch {
            showToast("节点切换失败")
        }
        await refreshGroups()
    }

    /// 恢复自动（v0.4.16）：DELETE /proxies/{group} 清除 URLTest/Fallback 手动固定。
    /// WHY 不做显式乐观：DELETE 是即时生效语义且网关本地毫秒级，refreshGroups 权威值
    /// 落地即正确态；selectNode 的乐观模式针对 PUT 后内核确认延迟，此处无此时序。
    /// §13.2 c3 返回 Bool=是否实际清除：fixed==nil 早退返 false；DELETE 失败
    /// （toast 后）返 false，成功 true。WHY 不按组类型过滤：Fallback 组 fixed 可
    /// 非 nil（modeBadge/unpin 链同口径），本函数对 URLTest/Fallback 均真实清除。
    /// @discardableResult：恢复自动按钮链路（GroupsCardView）无需返回值。
    /// WHY 组参数（§13 review P2 身份漂移）：testLatency 的 runDelay 期间用户可点
    /// chips 换组，unpinFixed 内部重解析 activeGroup 会清错组/漏清被测组——测速链
    /// 显式传入口捕获组；缺省 nil=现行为（按钮链路零改动）。
    @discardableResult
    func unpinFixed(for target: ProxyGroup? = nil) async -> Bool {
        guard let group = target ?? activeGroup, let bridge, group.fixed != nil else { return false }
        let cleared: Bool
        do {
            try await bridge.unpin(group: group.id)
            cleared = true
        } catch {
            showToast("恢复自动失败")
            cleared = false
        }
        await refreshGroups()
        return cleared
    }

    /// 测速：CLI delay（并发 ≤8 由 CLI 保证）→ 清历史固定态 → 重拉 /proxies。
    /// §13.2 c3 b：URLTest 组曾误点击固定（fixed≠nil）则内核永不自动切换，测速后
    /// unpinFixed 真实清除（与 mihomo GET /group/:name/delay 清 fixed 同构）；内核
    /// 300s tick 用新延迟重选。调用序定死（评审 P2）：unpinFixed 在前、保留尾部
    /// refreshGroups——"URLTest 且曾固定"罕见路径双拉 /proxies 显式接受（多一次
    /// 本地网关 GET 无害）。toast 仅 URLTest 且确有清除（Fallback 清固定静默，
    /// 状态变化经 refreshGroups 徽标呈现——"选中最低延迟"对 fallback 优先级选主
    /// 是误导）。
    func testLatency() async {
        guard let bridge, let group = activeGroup, !busy else { return }
        busy = true
        defer { busy = false }
        do {
            try await bridge.runDelay(group: group.id)
            let cleared = await unpinFixed(for: group) // 显式传被测组：防 await 期间换组致身份漂移
            if cleared, group.type == "URLTest" {
                showToast("已恢复自动切换，内核将在测速周期内选中最低延迟")
            }
        } catch {
            showToast("测速失败")
        }
        await refreshGroups()
    }

    /// 网页面板：panel --url → 浏览器唤起。
    /// WHY 接收 open 返回值并做剪贴板兜底（v0.4.2 热修，2026-11-19 真机反馈"点击没反应"）：
    /// 旧实现静默丢弃 NSWorkspace.open 的 Bool 返回值，违反"不静默吞错"准则；
    /// 打开失败时把入口 URL 复制到剪贴板，保证用户总能拿到入口。catch 分支维持原提示。
    /// code-reviewer P3：CLI exit 0 但输出空时不复制（清空用户剪贴板）也不谎报"已复制"。
    /// v0.4.5 追加诊断面包屑（panelLog WHY 见定义处）：fired 无后续即桥缺失，
    /// 各失败分支各留一条含分支名的记录；动态值显式 .public，否则统一日志里被脱敏成 <private>。
    func openPanel() async {
        Self.panelLog.notice("openPanel fired")
        guard let bridge else { return }
        do {
            let url = try await bridge.panelURL()
            // 只记长度不记内容，足以区分"URL 空"与"URL 正常"
            Self.panelLog.notice("panelURL acquired (length=\(url.count, privacy: .public))")
            guard let u = URL(string: url), !url.isEmpty else {
                Self.panelLog.notice("branch: URL empty")
                showToast("面板不可用：入口 URL 为空")
                return
            }
            // WHY 先存后判：open 的 Bool 结果落日志再走剪贴板兜底
            let ok = NSWorkspace.shared.open(u)
            Self.panelLog.notice("NSWorkspace.open -> \(ok, privacy: .public)")
            if ok { return }
            Self.panelLog.notice("branch: open false")
            copyToClipboard(url)
            showToast("面板打开失败，入口 URL 已复制到剪贴板")
        } catch {
            // error 为 CoreBridgeError.cliFailed：该类型无 LocalizedError 遵从，
            // localizedDescription 落系统默认文案（仅类型名+case 序号，无关联值/路径/token）。
            // ⚠️ 若日后为其添加回显 payload 的 LocalizedError（如内嵌 URL），此处 .public 即泄露通道
            Self.panelLog.notice("branch: cli error: \(error.localizedDescription, privacy: .public)")
            showToast("面板不可用：\(error.localizedDescription)")
        }
    }

    /// 剪贴板兜底（v0.4.2）：面板打不开时用户仍可手动粘贴入口 URL。
    private func copyToClipboard(_ string: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(string, forType: .string)
    }

    /// 面板底部退出：停内核 + 精确还原代理 → 退壳。
    func stopCoreAndQuit() async {
        guard let bridge else { NSApp.terminate(nil); return }
        do {
            try await bridge.stopCore()
        } catch {
            showToast("停止失败，仅退出壳")
        }
        NSApp.terminate(nil)
    }

    // MARK: 私有（数据整形）

    private var activeGroup: ProxyGroup? {
        groups.first { $0.id == activeGroupID } ?? groups.first
    }

    /// /proxies → 组视图模型；GLOBAL 聚合组与非组条目排除（与 CLI nodes 口径一致）。
    /// v0.4.15：组内成员按实测延迟升序（未测排尾，用户指令"延迟最低排最上"；
    /// 同延迟保持内核稳定序）——展示层排序，内核/CLI 口径不变。
    private func refreshGroups() async {
        guard let gateway else {
            degraded = true
            return
        }
        do {
            let proxies = try await gateway.proxies()
            let names = proxies.proxies.keys
                .filter { $0 != "GLOBAL" && proxies.proxies[$0]?.isGroup == true }
                .sorted()
            groups = names.map { name in
                let entry = proxies.proxies[name]!
                let nodes = (entry.all ?? []).map { node in
                    ProxyGroup.Node(id: node, name: node, delay: proxies.proxies[node]?.lastDelay,
                                    isGroup: proxies.proxies[node]?.isGroup ?? false)
                }
                return ProxyGroup(
                    id: name,
                    name: name,
                    type: entry.type,
                    nodes: nodes.enumerated().sorted { a, b in
                        switch (a.element.delay, b.element.delay) {
                        case let (x?, y?): return x != y ? x < y : a.offset < b.offset
                        case (.some, nil): return true // 有延迟在前
                        case (nil, .some): return false // 未测排尾
                        case (nil, nil): return a.offset < b.offset // 同未测保持内核稳定序
                        }
                    }.map(\.element),
                    currentNode: entry.now,
                    // 归一化空串（审查 P1，mihomo MarshalJSON 无 omitempty：未固定态
                    // 线格式是 "fixed":"" 而非缺省——String? 解码得 Optional("")，不
                    // 归一化则徽标恒"已固定"、解除闭环失效）
                    fixed: entry.fixed.flatMap { $0.isEmpty ? nil : $0 }
                )
            }
            activeGroupID = activeGroupID ?? groups.first?.id
            degraded = false
        } catch {
            groups = []
            degraded = true
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

extension GatewayProxies.Entry {
    /// 策略组判定（与 Go 侧 core.Proxy.IsGroup 同口径）。
    var isGroup: Bool {
        ["Selector", "URLTest", "Fallback", "LoadBalance", "Relay"].contains(type)
    }

    /// 最近一次延迟采样；无记录或 0 视为未测。
    var lastDelay: Int? {
        guard let last = history?.last, last.delay > 0 else { return nil }
        return last.delay
    }
}
