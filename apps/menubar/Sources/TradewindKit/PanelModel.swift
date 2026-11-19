import AppKit
import Combine
import Foundation
import os

/// 策略组视图模型（/proxies 裁剪）：组内成员保持内核顺序（即订阅配置顺序）。
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
    let nodes: [Node]
    let currentNode: String?
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
    /// 接入路径：系统代理开启即视为有接入（demo：sysProxy || tun；M2 无 TUN）。
    /// WHY effective 口径：菜单栏图标绿点随系统代理点击立即翻转（v0.4.7）
    var intake: Bool { effectiveSysProxy }
    var iconState: StatusIconState { StatusIcon.state(running: effectiveRunning, intake: intake) }
    /// 当前节点延迟：activeGroup.nodes 中 currentNode 匹配项的 delay。
    /// WHY 下沉为模型计算属性：视图内查找无法单测（M3-plan §3.2）
    var currentNodeDelay: Int? {
        guard let group = activeGroup, let node = group.currentNode else { return nil }
        return group.nodes.first { $0.name == node }?.delay
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
    func toggleMaster(on: Bool) async {
        guard let bridge else { return }
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

    /// 测速：CLI delay（并发 ≤8 由 CLI 保证）→ 重拉 /proxies 刷新延迟历史。
    func testLatency() async {
        guard let bridge, let group = activeGroup, !busy else { return }
        busy = true
        defer { busy = false }
        do {
            try await bridge.runDelay(group: group.id)
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
                return ProxyGroup(
                    id: name,
                    name: name,
                    nodes: (entry.all ?? []).map { node in
                        ProxyGroup.Node(id: node, name: node, delay: proxies.proxies[node]?.lastDelay,
                                        isGroup: proxies.proxies[node]?.isGroup ?? false)
                    },
                    currentNode: entry.now
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
