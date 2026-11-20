import Foundation
import Testing
@testable import TradewindKit

/// URLProtocol 桩：网关 HTTP 响应脚本化（壳层单测不发真实网络请求）。
final class StubURLProtocol: URLProtocol {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) -> (Int, String))?
    private static let lock = NSLock()

    static func install() -> URLSession {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        return URLSession(configuration: config)
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let handler = Self.handler, let url = request.url else {
            client?.urlProtocol(self, didFailWithError: URLError(.badURL))
            return
        }
        let (code, body) = handler(request)
        let response = HTTPURLResponse(url: url, statusCode: code, httpVersion: nil, headerFields: nil)!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body.data(using: .utf8) ?? Data())
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

/// WHY .serialized：共享的 URLProtocol 全局桩在并行用例间互踩，必须串行。
@Suite(.serialized) @MainActor struct PanelModelTests {
    private nonisolated static let statusWithPanel = """
    {"core":{"running":true,"pid":2640,"version":"v1.19.34"},"mode":"rule","sys_proxy":{"applied":true,"services":1},"profile":{"uid":"u1","name":"sub"},"panel_url":"http://127.0.0.1:19097/ui/tok123/"}
    """

    /// v0.4.4 扩展：加 URLTest 嵌套组 "Auto" 并入 组A.all（真实订阅选择组内常嵌套组）
    private nonisolated static let proxiesJSON = """
    {"proxies":{
      "GLOBAL":{"name":"GLOBAL","type":"Selector","now":"组A"},
      "组A":{"name":"组A","type":"Selector","now":"HK-1","all":["HK-1","JP-1","US-1","Auto"]},
      "组B":{"name":"组B","type":"URLTest","now":"JP-1","all":["JP-1"]},
      "Auto":{"name":"Auto","type":"URLTest","now":"HK-1","all":["HK-1","JP-1","US-1"]},
      "HK-1":{"name":"HK-1","type":"Socks5","history":[{"delay":42}]},
      "JP-1":{"name":"JP-1","type":"Socks5","history":[{"delay":180}]},
      "US-1":{"name":"US-1","type":"Socks5","history":[]}
    }}
    """

    private func makeModel(_ replies: [FakeRunner.Reply]) -> PanelModel {
        PanelModel(bridge: CoreBridge(runner: FakeRunner(replies)), session: StubURLProtocol.install())
    }

    @Test func refreshPopulatesGroupsSortedWithoutGlobal() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        #expect(!model.degraded)
        #expect(model.groups.map(\.id) == ["Auto", "组A", "组B"]) // GLOBAL 排除 + 字典序（Auto ASCII 在前）
        #expect(model.activeGroupID == "Auto")
        let groupA = model.groups.first { $0.id == "组A" }!
        #expect(groupA.currentNode == "HK-1")
        // v0.4.15：节点按实测延迟升序（HK-1 42 → JP-1 180 → US-1/Auto 未测排尾，
        // 同延迟保持内核稳定序）；currentNode 仍按名匹配 ✓ 不受排序影响
        #expect(groupA.nodes.map(\.name) == ["HK-1", "JP-1", "US-1", "Auto"])
        #expect(groupA.nodes[0].delay == 42)
        #expect(groupA.nodes[2].delay == nil)
    }

    /// v0.4.15：延迟动态排序锁点——内核序故意乱序（300/100/100/未测），
    /// 刷新后升序 + 同延迟稳定 + 未测排尾
    @Test func nodesSortedByDelayAscending() async {
        let json = """
        {"proxies":{
          "GLOBAL":{"name":"GLOBAL","type":"Selector","now":"G"},
          "G":{"name":"G","type":"Selector","now":"B","all":["B","C","A","D"]},
          "A":{"name":"A","type":"Socks5","history":[{"delay":100}]},
          "B":{"name":"B","type":"Socks5","history":[{"delay":300}]},
          "C":{"name":"C","type":"Socks5","history":[{"delay":100}]},
          "D":{"name":"D","type":"Socks5","history":[]}
        }}
        """
        StubURLProtocol.handler = { _ in (200, json) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        let g = model.groups.first { $0.id == "G" }!
        #expect(g.nodes.map(\.name) == ["C", "A", "B", "D"]) // C/A 同 100ms 按内核稳定序
        #expect(g.currentNode == "B") // 选中标记按名匹配，排序不改变语义
    }

    /// v0.4.15：策略组展示形态——常见英文名映射 demo 中文语义（icon+名）；
    /// 未命中组保留原名+按类型配图标（多地区组不重名）
    @Test func groupDisplayMapsDemoSemantics() {
        func group(_ name: String, _ type: String) -> ProxyGroup {
            ProxyGroup(id: name, name: name, type: type, nodes: [], currentNode: nil, fixed: nil)
        }
        #expect(group("Proxy", "Selector").display == ("🚀", "节点选择"))
        #expect(group("Auto", "URLTest").display == ("♻️", "自动选择"))
        #expect(group("DIRECT", "Direct").display == ("🎯", "全球直连"))
        // 中文命名订阅直接命中表（审查 P3-5：名字表全覆盖）
        #expect(group("节点选择", "Selector").display == ("🚀", "节点选择"))
        #expect(group("手动选择", "Selector").display == ("🚀", "节点选择"))
        #expect(group("手动切换", "Selector").display == ("🚀", "节点选择"))
        #expect(group("自动选择", "URLTest").display == ("♻️", "自动选择"))
        #expect(group("自动", "URLTest").display == ("♻️", "自动选择"))
        #expect(group("全球直连", "Direct").display == ("🎯", "全球直连"))
        #expect(group("直连", "Direct").display == ("🎯", "全球直连"))
        // 未命中：原名保留 + 类型图标（区分度不丢；Relay/default 分支全覆盖）
        #expect(group("香港IEPL", "Selector").display == ("🚀", "香港IEPL"))
        #expect(group("Custom", "LoadBalance").display == ("⚖️", "Custom"))
        #expect(group("Custom", "Fallback").display == ("🪂", "Custom"))
        #expect(group("Custom", "Relay").display == ("🔗", "Custom"))
        #expect(group("Custom", "Unknown").display == ("", "Custom"))
    }

    @Test func degradedWhenPanelURLMissing() async {
        // 降级契约（M2-plan §5 v1.1）：panel_url 空 → degraded，控制面仍可用
        let noPanel = Self.statusWithPanel.replacingOccurrences(
            of: "\"panel_url\":\"http://127.0.0.1:19097/ui/tok123/\"",
            with: "\"panel_url\":\"\""
        )
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: noPanel, exitCode: 0)])
        await model.refresh()
        #expect(model.degraded)
        #expect(model.groups.isEmpty)
        #expect(model.report != nil) // 控制面状态仍上报
    }

    @Test func degradedWhenGatewayRespondsError() async {
        StubURLProtocol.handler = { _ in (502, "gateway error") }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        #expect(model.degraded)
        #expect(model.groups.isEmpty)
    }

    /// v0.4.16：模式徽标三态（用户拍板"全自动/手动固定可视化"）——
    /// Selector 出口落嵌套组=自动、落节点=手动；URLTest 看 fixed 字段
    @Test func modeBadgeFollowsExitSemantics() {
        func node(_ name: String, delay: Int? = nil, isGroup: Bool = false) -> ProxyGroup.Node {
            .init(id: name, name: name, delay: delay, isGroup: isGroup)
        }
        // Selector：出口=嵌套组 → 自动；出口=节点 → 手动
        let selectorToGroup = ProxyGroup(id: "Proxy", name: "Proxy", type: "Selector",
                                         nodes: [node("Auto", isGroup: true)], currentNode: "Auto", fixed: nil)
        #expect(selectorToGroup.modeBadge == "自动")
        let selectorToNode = ProxyGroup(id: "Proxy", name: "Proxy", type: "Selector",
                                        nodes: [node("HK-1")], currentNode: "HK-1", fixed: nil)
        #expect(selectorToNode.modeBadge == "手动")
        // URLTest：fixed 非空=已固定；空=自动
        let urlTestPinned = ProxyGroup(id: "Auto", name: "Auto", type: "URLTest",
                                       nodes: [node("HK-1")], currentNode: "HK-1", fixed: "HK-1")
        #expect(urlTestPinned.modeBadge == "已固定")
        let urlTestFree = ProxyGroup(id: "Auto", name: "Auto", type: "URLTest",
                                     nodes: [node("HK-1")], currentNode: "HK-1", fixed: nil)
        #expect(urlTestFree.modeBadge == "自动")
        // 其他类型无此语义
        #expect(ProxyGroup(id: "X", name: "X", type: "Relay", nodes: [], currentNode: nil, fixed: nil).modeBadge == nil)
    }

    /// v0.4.16：恢复自动动作——CLI unpin 调用后重拉 /proxies，fixed 权威值落地
    @Test func unpinFixedResumesAuto() async {
        let pinnedJSON = """
        {"proxies":{
          "GLOBAL":{"name":"GLOBAL","type":"Selector","now":"Auto"},
          "Auto":{"name":"Auto","type":"URLTest","now":"HK-1","all":["HK-1"],"fixed":"HK-1"},
          "HK-1":{"name":"HK-1","type":"Socks5","history":[{"delay":42}]}
        }}
        """
        // 真实 mihomo 线格式（审查 P1）：未固定态是 "fixed":"" 而非字段缺省
        // （MarshalJSON 无 omitempty）——空串必须归一化为 nil 才能判定"自动"
        let freeJSON = pinnedJSON.replacingOccurrences(of: "\"fixed\":\"HK-1\"", with: "\"fixed\":\"\"")
        StubURLProtocol.handler = { _ in (200, pinnedJSON) }
        let fake = FakeRunner(FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0))
        let model = PanelModel(bridge: CoreBridge(runner: fake), session: StubURLProtocol.install())
        await model.refresh()
        let pinned = model.groups.first { $0.id == "Auto" }
        #expect(pinned?.fixed == "HK-1")
        #expect(pinned?.modeBadge == "已固定")

        // 解除：unpin 成功 → 网关侧 fixed 消失 → 权威值落地
        StubURLProtocol.handler = { _ in (200, freeJSON) }
        await model.unpinFixed()
        let freed = model.groups.first { $0.id == "Auto" }
        #expect(freed?.fixed == nil)
        #expect(freed?.modeBadge == "自动")
        #expect(fake.calls.contains(["unpin", "Auto"]))
    }

    /// v0.4.16（审查 P3-4）：fixed==nil 时恢复自动为 no-op（不发起 CLI 调用）
    @Test func unpinSkipsWhenNotFixed() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let fake = FakeRunner(FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0))
        let model = PanelModel(bridge: CoreBridge(runner: fake), session: StubURLProtocol.install())
        await model.refresh()
        let before = fake.calls.count
        await model.unpinFixed() // 共享 mock 的组均无 fixed → no-op
        #expect(fake.calls.count == before)
    }

    @Test func iconStateFollowsReportSemantics() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        #expect(model.iconState == .active) // running + sysproxy applied

        let offProxy = Self.statusWithPanel.replacingOccurrences(
            of: "\"applied\":true", with: "\"applied\":false"
        )
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model2 = makeModel([FakeRunner.Reply(output: offProxy, exitCode: 0)])
        await model2.refresh()
        #expect(model2.iconState == .idle) // 运行但无接入
    }

    @Test func selectNodeFailureKeepsGroupsUntouched() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // status
            FakeRunner.Reply(output: "", exitCode: 1), // select 失败
        ])
        await model.refresh()
        let before = model.groups
        await model.selectNode("JP-1")
        #expect(model.groups == before) // 内核失败不落盘（乐观更新不发生）
        #expect(model.toast != nil) // 用户可读提示
    }

    /// v0.4.2 热修：openPanel 的 CLI 失败路径走 catch 分支提示（不触发 NSWorkspace.open，
    /// 该路径可单测；open 成功/剪贴板路径真开浏览器，维持不测口径）。
    @Test func openPanelCliFailureShowsToast() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // status
            FakeRunner.Reply(output: "", exitCode: 1), // panel --url 失败
        ])
        await model.refresh()
        await model.openPanel()
        #expect(model.toast != nil) // "面板不可用"用户可读提示
    }

    @Test func setModeAndTestLatencyPassExactArgs() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let fake = FakeRunner(
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // status
            FakeRunner.Reply(output: "", exitCode: 0), // mode
            FakeRunner.Reply(output: "", exitCode: 0), // delay
            FakeRunner.Reply(output: "", exitCode: 0)  // 测速后的刷新 status
        )
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = PanelModel(bridge: CoreBridge(runner: fake), session: StubURLProtocol.install())
        await model.refresh()
        await model.setMode("global")
        await model.testLatency()
        let calls = fake.calls
        #expect(calls.contains(["mode", "global"]))
        #expect(calls.contains(["delay", "Auto"])) // 测速作用于 activeGroup（字典序首个=Auto）
    }

    // MARK: 当前节点卡数据路径（M3-plan §3.2）

    @Test func currentNodeDelayResolvesFromMatchedNode() async {
        // 徽标取 activeGroup.nodes 中 currentNode 匹配项的 delay（activeGroup=Auto current HK-1 → 42）
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        #expect(model.currentNodeDelay == 42)
    }

    @Test func currentNodeDelayNilWithoutLatencyRecord() async {
        // 当前节点无延迟采样 → nil，卡片显示"—"（灰）。替换目标锚定 Auto 组整段前缀
        // （code-reviewer P3：组A 与 Auto 的 now 同值，全局替换会双双命中——曾致断言
        // 经"US-1 非成员查找失败"而非本意的"成员在列但 history 空"路径通过）。
        // activeGroup 为字典序首组 Auto，now→US-1（在 Auto.all 内、history 空）
        let noDelay = Self.proxiesJSON.replacingOccurrences(
            of: "\"Auto\":{\"name\":\"Auto\",\"type\":\"URLTest\",\"now\":\"HK-1\"",
            with: "\"Auto\":{\"name\":\"Auto\",\"type\":\"URLTest\",\"now\":\"US-1\""
        )
        StubURLProtocol.handler = { _ in (200, noDelay) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        #expect(model.groups[0].currentNode == "US-1")
        #expect(model.currentNodeDelay == nil)
    }

    @Test func flagDataPathResolvesCurrentNodeName() async {
        // 旗帜注入 CurrentNodeCard 数据路径：卡片按 CountryFlag.flag(for: currentNode) 取旗帜
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        let currentNode = model.groups[0].currentNode ?? ""
        #expect(CountryFlag.flag(for: currentNode) == "🇭🇰") // HK-1 → 🇭🇰（非兜底）
    }

    /// v0.4.4 嵌套组识别：组内成员若本身是策略组 → isGroup=true，普通节点 false。
    @Test func nestedGroupMemberMarkedAsGroup() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        let groupA = model.groups.first { $0.id == "组A" }!
        #expect(groupA.nodes.first { $0.name == "Auto" }?.isGroup == true) // URLTest 嵌套组
        #expect(groupA.nodes.first { $0.name == "HK-1" }?.isGroup == false) // 普通节点
        #expect(model.groups.first { $0.id == "组B" }!.nodes[0].isGroup == false) // 顶层组成员也是普通节点
    }

    // MARK: v0.4.10 链式解析（方案 B，2026-11-19 拍板）

    /// ① 嵌套组链：组A.now=Auto、Auto.now=HK-1 → names=[组A,Auto,HK-1]、tailDelay=42
    ///（替换锚定组A前缀：组A 与 Auto 的 now 同值，全局替换会双双命中——同既有 P3 口径）
    @Test func exitChainResolvesNestedGroupToTailNode() async throws {
        let nested = Self.proxiesJSON.replacingOccurrences(
            of: "\"组A\":{\"name\":\"组A\",\"type\":\"Selector\",\"now\":\"HK-1\"",
            with: "\"组A\":{\"name\":\"组A\",\"type\":\"Selector\",\"now\":\"Auto\""
        )
        StubURLProtocol.handler = { _ in (200, nested) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        model.activeGroupID = "组A"
        let chain = try #require(model.exitChain())
        #expect(chain.names == ["组A", "Auto", "HK-1"]) // 深度与 fixture 链一致（上限 3）
        #expect(chain.tailDelay == 42) // 尾节点 HK-1 的延迟采样
    }

    /// ② 普通直选组（now=普通节点）→ nil：卡片维持单节点形态
    @Test func exitChainNilWhenCurrentNodeIsPlainNode() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        model.activeGroupID = "组B" // URLTest 组但 now=JP-1（普通节点）
        #expect(model.exitChain() == nil)
        #expect(model.currentNodeDelay == 180) // 单节点延迟口径不受链式解析影响
    }

    /// ③ 防环：组X.now=组Y、组Y.now=组X 互指 → names 封顶（≤3）、有界终止不死循环
    @Test func exitChainBreaksReferenceCycleWithBoundedDepth() async throws {
        let cyclic = """
        {"proxies":{
          "组X":{"name":"组X","type":"Selector","now":"组Y","all":["组Y"]},
          "组Y":{"name":"组Y","type":"Selector","now":"组X","all":["组X"]},
          "HK-1":{"name":"HK-1","type":"Socks5","history":[{"delay":42}]}
        }}
        """
        StubURLProtocol.handler = { _ in (200, cyclic) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        model.activeGroupID = "组X"
        let chain = try #require(model.exitChain())
        #expect((chain.names.count) <= 3) // 深度封顶
        #expect(chain.names.first == "组X") // 首位恒为当前组名
        #expect(chain.tailDelay == nil) // 链尾仍是组：无尾节点延迟语义
    }

    // MARK: v0.4.7 乐观更新（方案 B）：动作发起即翻转 UI，权威值落地/失败后清除

    /// ① CLI 失败 → 乐观值清除（UI 视觉回退）+ toast 用户可读提示
    @Test func sysProxyOptimisticRevertsOnCliFailure() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // 初始 status（applied=true）
            FakeRunner.Reply(output: "", exitCode: 1),                   // sys-proxy off CLI 失败
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // 失败后的刷新 status
        ])
        await model.refresh()
        await model.setSysProxy(on: false)
        #expect(model.pendingSysProxy == nil) // 失败回退：乐观值已清除
        #expect(model.toast != nil)           // 用户可读提示
        #expect(model.effectiveSysProxy == true) // 权威值未变，effective 仍为 applied=true
    }

    /// ② 成功路径：乐观值清除，refresh 权威值落地并接管 effective 口径
    @Test func sysProxyOptimisticClearsOnSuccessAndReportTakesOver() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let appliedOff = Self.statusWithPanel.replacingOccurrences(
            of: "\"applied\":true", with: "\"applied\":false"
        )
        let model = makeModel([
            FakeRunner.Reply(output: appliedOff, exitCode: 0),           // 初始 status（applied=false）
            FakeRunner.Reply(output: "", exitCode: 0),                   // sys-proxy on CLI 成功
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // 刷新 status（applied=true）
        ])
        await model.refresh()
        #expect(model.effectiveSysProxy == false) // 初始权威值 off
        await model.setSysProxy(on: true)
        #expect(model.pendingSysProxy == nil)            // 成功后乐观值清除
        #expect(model.report?.sysProxy.applied == true)  // report 权威值已落地
        #expect(model.effectiveSysProxy == true)         // effective 由权威值接管
    }

    // MARK: P3-3（审查折入）：toggleMaster 开启路径 pendingRunning 生命周期（P2 有界轮询）

    /// start 走注入缝隙的模型构造：startDetached 注入 no-op（单测绝不 spawn 真 CLI），
    /// 其余 CLI 调用仍走 FakeRunner 脚本；实际调用序列 = 初始 status → start（不耗脚本）→
    /// 轮询 refresh 的 status × N。
    private func makeStartModel(_ replies: [FakeRunner.Reply]) -> PanelModel {
        let bridge = CoreBridge(runner: FakeRunner(replies))
        bridge.startDetached = {}
        return PanelModel(bridge: bridge, session: StubURLProtocol.install())
    }

    /// ① start 后内核未即时就绪（纯 spawn，socket 就绪晚于首个 status 探测）：
    /// 轮询探测两轮 false、第三轮 true → break；终态 pendingRunning==nil 且权威值接管
    @Test func masterStartPollsUntilKernelReadyThenHandsOver() async {
        let stopped = Self.statusWithPanel.replacingOccurrences(
            of: "\"running\":true", with: "\"running\":false"
        )
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeStartModel([
            FakeRunner.Reply(output: stopped, exitCode: 0),              // 初始 status：未运行
            FakeRunner.Reply(output: stopped, exitCode: 0),              // 轮询探测 1：false
            FakeRunner.Reply(output: stopped, exitCode: 0),              // 轮询探测 2：false
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // 轮询探测 3：true → break
        ])
        await model.refresh()
        await model.toggleMaster(on: true)
        #expect(model.pendingRunning == nil)     // 内核就绪后清除（权威值接管）
        #expect(model.report?.core.running == true)
        #expect(model.effectiveRunning == true)  // 无"绿→灰→绿"闪回：乐观值撑到真就绪
    }

    /// ② start 后内核持续未起：5 轮有界轮询耗尽仍未 true → pendingRunning==nil、
    /// effectiveRunning==false（乐观值被权威值否定，回落实名停止态，不无限乐观）
    @Test func masterStartBoundedPollingFallsBackToStopped() async {
        let stopped = Self.statusWithPanel.replacingOccurrences(
            of: "\"running\":true", with: "\"running\":false"
        )
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        var replies = [
            FakeRunner.Reply(output: stopped, exitCode: 0), // 初始 status：未运行
        ]
        for _ in 0..<5 {
            replies.append(FakeRunner.Reply(output: stopped, exitCode: 0)) // 探测 1..5：均 false
        }
        let model = makeStartModel(replies)
        await model.refresh()
        await model.toggleMaster(on: true)
        #expect(model.pendingRunning == nil)
        #expect(model.report?.core.running == false)
        #expect(model.effectiveRunning == false)
    }

    /// ③ 入口 busy 守卫（批② c5）：busy 在途时 toggleMaster 拒绝重入——popover 开关
    /// 视图层本就 .disabled(busy)，此处锁模型层契约（快捷键路径无视图，全靠此守卫）
    @Test func toggleMasterRejectedWhileBusy() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let fake = FakeRunner()
        let bridge = CoreBridge(runner: fake)
        var spawns = 0
        bridge.startDetached = { spawns += 1 }
        let model = PanelModel(bridge: bridge, session: StubURLProtocol.install())
        model.busy = true
        await model.toggleMaster(on: true)
        #expect(spawns == 0)
        #expect(fake.calls.isEmpty)
    }

    // MARK: 批② c5 快捷键 toggle（M3-plan §11.2-7：现场 status 探测定向 + busy 防抖共享）

    /// ① 探测报告 stopped → start：即使 model.report 为 nil/陈旧也以现场探测为准
    /// （评审 P2：收起态 report 30s 刷新，缓存判定会在 stale 窗口双 start）
    @Test func hotkeyToggleStartsWhenFreshProbeReportsStopped() async {
        let stopped = Self.statusWithPanel.replacingOccurrences(
            of: "\"running\":true", with: "\"running\":false"
        )
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let fake = FakeRunner([
            FakeRunner.Reply(output: stopped, exitCode: 0),              // 现场探测：未运行
            FakeRunner.Reply(output: stopped, exitCode: 0),              // 轮询探测 1：false
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // 轮询探测 2：true → break
        ])
        let bridge = CoreBridge(runner: fake)
        var spawns = 0
        bridge.startDetached = { spawns += 1 }
        let model = PanelModel(bridge: bridge, session: StubURLProtocol.install())
        await model.toggleFromHotkey()
        #expect(spawns == 1) // detached start 已触发（不误 stop：无 ["stop"] 调用）
        #expect(!fake.calls.contains(["stop"]))
        #expect(model.effectiveRunning)
    }

    /// ② 探测报告 running → stop：调用序列 = 现场探测 → stop → 权威值刷新
    @Test func hotkeyToggleStopsWhenFreshProbeReportsRunning() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let fake = FakeRunner([
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // 现场探测：运行中
            FakeRunner.Reply(output: "", exitCode: 0),                   // stop
            FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0), // stop 后刷新 status
        ])
        let model = PanelModel(bridge: CoreBridge(runner: fake), session: StubURLProtocol.install())
        await model.toggleFromHotkey()
        #expect(fake.calls == [["status", "--json"], ["stop"], ["status", "--json"]])
    }

    /// ③ busy 在途快捷键整体拒绝：不探测、不 toggle（与 popover 共享 busy 防抖）
    @Test func hotkeyToggleRejectedWhileBusy() async {
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let fake = FakeRunner()
        let model = PanelModel(bridge: CoreBridge(runner: fake), session: StubURLProtocol.install())
        model.busy = true
        await model.toggleFromHotkey()
        #expect(fake.calls.isEmpty) // 连现场探测都不发起
    }

    /// ④ 现场探测失败：toast 显式提示（不静默），不进入 toggle
    @Test func hotkeyToggleShowsToastWhenProbeFails() async {
        let fake = FakeRunner(FakeRunner.Reply(output: "", exitCode: 2)) // 非契约退出码
        let model = PanelModel(bridge: CoreBridge(runner: fake), session: StubURLProtocol.install())
        await model.toggleFromHotkey()
        #expect(model.toast != nil)
        #expect(fake.calls == [["status", "--json"]]) // 仅探测，无启停调用
    }
}
