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
    private static let statusWithPanel = """
    {"core":{"running":true,"pid":2640,"version":"v1.19.34"},"mode":"rule","sys_proxy":{"applied":true,"services":1},"profile":{"uid":"u1","name":"sub"},"panel_url":"http://127.0.0.1:19097/ui/tok123/"}
    """

    private static let proxiesJSON = """
    {"proxies":{
      "GLOBAL":{"name":"GLOBAL","type":"Selector","now":"组A"},
      "组A":{"name":"组A","type":"Selector","now":"HK-1","all":["HK-1","JP-1","US-1"]},
      "组B":{"name":"组B","type":"URLTest","now":"JP-1","all":["JP-1"]},
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
        #expect(model.groups.map(\.id) == ["组A", "组B"]) // GLOBAL 排除 + 字典序（与 CLI nodes 口径一致）
        #expect(model.activeGroupID == "组A")
        let groupA = model.groups[0]
        #expect(groupA.currentNode == "HK-1")
        #expect(groupA.nodes[0].delay == 42) // 未测节点（US-1）delay 为 nil
        #expect(groupA.nodes[2].delay == nil)
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
        #expect(calls.contains(["delay", "组A"]))
    }

    // MARK: 当前节点卡数据路径（M3-plan §3.2）

    @Test func currentNodeDelayResolvesFromMatchedNode() async {
        // 徽标取 activeGroup.nodes 中 currentNode 匹配项的 delay（组A current HK-1 → 42）
        StubURLProtocol.handler = { _ in (200, Self.proxiesJSON) }
        let model = makeModel([FakeRunner.Reply(output: Self.statusWithPanel, exitCode: 0)])
        await model.refresh()
        #expect(model.currentNodeDelay == 42)
    }

    @Test func currentNodeDelayNilWithoutLatencyRecord() async {
        // 当前节点无延迟采样（US-1 history 空）→ nil，卡片显示"—"（灰）
        let noDelay = Self.proxiesJSON.replacingOccurrences(
            of: "\"now\":\"HK-1\"", with: "\"now\":\"US-1\""
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
}
