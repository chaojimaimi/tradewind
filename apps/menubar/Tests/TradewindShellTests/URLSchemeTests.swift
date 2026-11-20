import AppKit
import Foundation
import Testing
@testable import TradewindKit

// MARK: - tradewind:// URL Scheme 测试（批④ §2.3）

/// URL 解析矩阵（含 4KB 上限/非 import host/percent-decode/host-only 正文）。
@Suite struct URLSchemeParserTests {
    @Test func parsesCanonicalImportURL() throws {
        let raw = "tradewind://import?url=https%3A%2F%2Fexample.com%2Fapi%2Fsubscribe%3Ftoken%3Dabc123"
        let event = try #require(URLSchemeParser.parse(raw))
        // percent-decode 后为完整订阅 URL（含 token——不进日志/toast，仅结构化传递）
        #expect(event.url == "https://example.com/api/subscribe?token=abc123")
    }

    @Test func acceptsUppercaseSchemeAndHost() throws {
        let event = URLSchemeParser.parse("TRADEWIND://IMPORT?url=https%3A%2F%2Fexample.com")
        #expect(event?.url == "https://example.com")
    }

    @Test func rejectsNonTradewindScheme() {
        #expect(URLSchemeParser.parse("https://import?url=x") == nil)
        #expect(URLSchemeParser.parse("otherscheme://import?url=x") == nil)
    }

    @Test func rejectsNonImportHost() {
        #expect(URLSchemeParser.parse("tradewind://settings?url=x") == nil)
        #expect(URLSchemeParser.parse("tradewind://import.evil.example?url=x") == nil)
    }

    @Test func rejectsPathCarryingVariants() {
        // 严格形态：host 后不得带路径段（防御形似 host 的社工变体）
        #expect(URLSchemeParser.parse("tradewind://import/steal?url=x") == nil)
    }

    @Test func rejectsMissingOrEmptyURLParam() {
        #expect(URLSchemeParser.parse("tradewind://import") == nil)
        #expect(URLSchemeParser.parse("tradewind://import?other=x") == nil)
        #expect(URLSchemeParser.parse("tradewind://import?url=") == nil)
    }

    @Test func rejectsOver4KB() {
        let longValue = String(repeating: "a", count: 4096)
        #expect(URLSchemeParser.parse("tradewind://import?url=\(longValue)") == nil)
        // 恰好 4KB 内放行
        let okValue = String(repeating: "a", count: 4000)
        #expect(URLSchemeParser.parse("tradewind://import?url=https%3A%2F%2F\(okValue)") != nil)
    }

    @Test func rejectsGarbageInput() {
        #expect(URLSchemeParser.parse("") == nil)
        #expect(URLSchemeParser.parse("::not a url::") == nil)
    }

    @Test func displayHostIsHostOnly() {
        // token/路径/查询串一律不显示（确认 alert 正文 host-only，评审 P1-4/P2-5）
        #expect(URLSchemeParser.displayHost("https://example.com/api/subscribe?token=secret123") == "example.com")
        #expect(URLSchemeParser.displayHost("http://sub.example.co.uk:8443/path?x=1") == "sub.example.co.uk")
        #expect(URLSchemeParser.displayHost("not a url") == "")
    }
}

/// 导入协调器（pendingURL 生命周期 + host-only 正文）。
@MainActor @Suite struct ImportCoordinatorTests {
    @Test func pendingURLRoundTripAndClear() {
        let coordinator = ImportCoordinator()
        #expect(coordinator.pendingURL == nil)
        #expect(coordinator.pendingHost == "")
        coordinator.pendingURL = "https://example.com/sub?token=t"
        #expect(coordinator.pendingHost == "example.com") // token 不进正文
        coordinator.clear()
        #expect(coordinator.pendingURL == nil)
        #expect(coordinator.pendingHost == "")
    }
}

/// handler 装配注入缝：stub 捕获 (target, selector)，不真注册全局 Apple 事件面。
@MainActor @Suite struct URLSchemeInstallTests {
    @Test func installCapturesTargetAndSelector() throws {
        let delegate = AppDelegate()
        var captured: (target: AnyObject, selector: Selector)?
        delegate.installGetURLHandler = { target, selector in captured = (target, selector) }
        delegate.installURLSchemeHandler()
        let c = try #require(captured)
        #expect(c.target === delegate)
        #expect(c.selector == #selector(AppDelegate.handleGetURLEvent(_:withReplyEvent:)))
    }

    /// handleIncomingURL 接线：合法 import URL → pendingURL 落位（openSettings 在
    /// stub 环境无窗口可开，仅验证状态流转）；非法输入 → 忽略（不改动既有 pending 态，
    /// 已挂起的确认不被挤掉）。
    @Test func handleIncomingURLRoutesValidImport() {
        let delegate = AppDelegate()
        delegate.handleIncomingURL("tradewind://import?url=https%3A%2F%2Fexample.com%2Fs")
        #expect(delegate.importCoordinator.pendingURL == "https://example.com/s")
        delegate.importCoordinator.clear()
        delegate.handleIncomingURL("tradewind://settings?url=x")
        #expect(delegate.importCoordinator.pendingURL == nil)
        delegate.handleIncomingURL("")
        #expect(delegate.importCoordinator.pendingURL == nil)
    }
}

/// 守护 toast：首采样播种不 toast；计数上升 toast；重置（下降）跟随新基线不 toast。
@MainActor @Suite struct GuardModelTests {
    private static func statusJSON(events: Int?) -> String {
        let eventPart = events.map { ",\"hijack_events\":\($0)" } ?? ""
        return #"{"core":{"running":false,"pid":0,"version":""},"mode":"rule","sys_proxy":{"applied":false,"services":0"# + eventPart + #"},"profile":{"uid":"","name":""},"panel_url":""}"#
    }

    @Test func firstSampleSeedsWithoutToast() async {
        let fake = FakeRunner(FakeRunner.Reply(output: Self.statusJSON(events: 3) + "\n", exitCode: 0))
        let model = GuardModel(bridge: CoreBridge(runner: fake))
        // 首采样播种基线（startPolling 的 timer 面依赖真实 RunLoop，单测直接驱动 sample）
        await model.sample()
        #expect(model.hijackEvents == 3)
        #expect(model.toast == nil) // 打开窗口不 toast 历史事件
    }

    @Test func risingCountShowsToast() async {
        let fake = FakeRunner(
            FakeRunner.Reply(output: Self.statusJSON(events: 0) + "\n", exitCode: 0),
            FakeRunner.Reply(output: Self.statusJSON(events: 1) + "\n", exitCode: 0))
        let model = GuardModel(bridge: CoreBridge(runner: fake))
        await model.sample() // 播种基线 0
        await model.sample() // 上升 → toast
        #expect(model.hijackEvents == 1)
        #expect(model.toast == "检测到系统代理被外部修改，已自动恢复")
    }

    @Test func resetFollowsNewBaselineWithoutToast() async {
        let fake = FakeRunner(
            FakeRunner.Reply(output: Self.statusJSON(events: 5) + "\n", exitCode: 0),
            FakeRunner.Reply(output: Self.statusJSON(events: 0) + "\n", exitCode: 0))
        let model = GuardModel(bridge: CoreBridge(runner: fake))
        await model.sample()
        await model.sample() // 计数重置（CLI on 重写 sys_proxy）→ 跟随，不 toast
        #expect(model.hijackEvents == 0)
        #expect(model.toast == nil)
    }

    @Test func pollingWithoutBridgeIsNoop() {
        let model = GuardModel(bridge: nil)
        model.startPolling() // CLI 缺失：静默不启动（守护权威面在 state.json/桌面通知）
        model.stopPolling()
    }
}

/// URL 导入执行链：busy 在途=拒绝+toast（performWrite 真实语义为在途拒绝，评审 P3
/// 对齐）；成功走 add 全链（65s 档）并补落点提示。
@MainActor @Suite struct URLSchemeImportTests {
    @Test func busyInFlightRejectsWithToast() async {
        let fake = FakeRunner()
        let model = SubsModel(bridge: CoreBridge(runner: fake))
        model.testHooksSetBusy(true)
        let ok = await model.importFromURLScheme("https://example.com/sub")
        #expect(!ok)
        #expect(model.toast == "有操作在途，请稍后再试")
        #expect(fake.calls.isEmpty) // 拒绝不落 CLI
    }

    @Test func importWithoutCLIShowsDegradationToast() async {
        let model = SubsModel(bridge: nil)
        let ok = await model.importFromURLScheme("https://example.com/sub")
        #expect(!ok)
        #expect(model.toast == "tradewind CLI 未找到，无法导入订阅")
    }

    @Test func confirmedImportRoutesAddWithLandingHint() async throws {
        let fake = FakeRunner(
            FakeRunner.Reply(output: "", exitCode: 0), // add
            FakeRunner.Reply(output: "[]", exitCode: 0)) // reloadList
        let model = SubsModel(bridge: CoreBridge(runner: fake))
        let ok = await model.importFromURLScheme("https://example.com/sub")
        #expect(ok)
        #expect(fake.calls.first == ["add", "https://example.com/sub"]) // 名称空白省略 --name
        #expect(model.toast == "订阅已导入（可订阅管理页改名）")
    }
}
