import AppKit
import Combine
import Foundation

/// 策略组视图模型（/proxies 裁剪）：组内成员保持内核顺序（即订阅配置顺序）。
struct ProxyGroup: Identifiable, Equatable {
    struct Node: Identifiable, Equatable {
        let id: String
        let name: String
        /// 最近延迟（毫秒）；nil=未测（灰点）
        let delay: Int?
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
    private(set) var gateway: GatewayClient?
    private let bridge: CoreBridge?
    /// 网关会话注入点（URLProtocol stub 单测；生产默认 .shared）
    private let gatewaySession: URLSession
    /// WS 速率流（速率卡 + 菜单栏速率文本的数据源）
    let traffic: TrafficStore
    /// popover 开合事件（壳装配层在 show 时发送；面板据此刷新）
    let popoverDidShow = PassthroughSubject<Void, Never>()

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
    /// 接入路径：系统代理开启即视为有接入（demo：sysProxy || tun；M2 无 TUN）
    var intake: Bool { report?.sysProxy.applied ?? false }
    var iconState: StatusIconState { StatusIcon.state(running: running, intake: intake) }

    // MARK: 刷新与动作

    /// 拉取 status + 节点列表；失败不抛出（降级态呈现）。
    func refresh() async {
        guard let bridge else { return }
        do {
            let report = try bridge.status()
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
    func toggleMaster(on: Bool) async {
        guard let bridge else { return }
        busy = true
        defer { busy = false }
        do {
            if on {
                try bridge.startCoreDetached()
                showToast("Tradewind 已启动：内核就绪后自动刷新")
            } else {
                try bridge.stopCore()
                showToast("Tradewind 已停止：系统代理已按备份精确还原")
            }
        } catch {
            showToast(on ? "启动失败" : "停止失败")
        }
        await refresh()
    }

    /// 出站模式切换（PATCH 热生效；settings 回写由 CLI 保证崩溃不回退）。
    func setMode(_ mode: String) async {
        guard let bridge else { return }
        do {
            try bridge.setMode(mode)
        } catch {
            showToast("模式切换失败")
        }
        await refresh()
    }

    /// 系统代理开关（幂等；on 需内核可达——CLI 侧守卫，失败明确报错）。
    func setSysProxy(on: Bool) async {
        guard let bridge else { return }
        do {
            try bridge.sysProxy(on: on)
        } catch {
            showToast(on ? "系统代理开启失败（内核未运行？）" : "系统代理关闭失败")
        }
        await refresh()
    }

    /// 切换节点：PUT 成功后本地乐观更新（内核失败不更新，重拉修正）。
    func selectNode(_ node: String) async {
        guard let group = activeGroup, let bridge else { return }
        do {
            try bridge.select(group: group.id, node: node)
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
            try bridge.runDelay(group: group.id)
        } catch {
            showToast("测速失败")
        }
        await refreshGroups()
    }

    /// 网页面板：panel --url → 浏览器唤起（URL 打开失败静默——网关已可达才走到这）。
    func openPanel() async {
        guard let bridge else { return }
        do {
            let url = try bridge.panelURL()
            if let u = URL(string: url), !url.isEmpty {
                NSWorkspace.shared.open(u)
            }
        } catch {
            showToast("面板不可用：\(error.localizedDescription)")
        }
    }

    /// 面板底部退出：停内核 + 精确还原代理 → 退壳。
    func stopCoreAndQuit() async {
        guard let bridge else { NSApp.terminate(nil); return }
        do {
            try bridge.stopCore()
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
                        ProxyGroup.Node(id: node, name: node, delay: proxies.proxies[node]?.lastDelay)
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
