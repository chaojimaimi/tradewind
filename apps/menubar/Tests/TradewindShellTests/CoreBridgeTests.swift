import Foundation
import Testing
@testable import TradewindKit

/// fake runner：记录调用参数，按脚本返回 stdout（壳层单测绝不真拉 tradewind 进程）。
final class FakeRunner: CommandLineRunning, @unchecked Sendable {
    struct Reply {
        let output: String
        let exitCode: Int32
    }

    private(set) var calls: [[String]] = []
    private var script: [Reply] = []
    private let lock = NSLock()

    init(_ replies: Reply...) {
        script = replies
    }

    /// 数组形态（脚本化场景与 Swift 无数组展开变参的折中）。
    init(_ replies: [Reply]) {
        script = replies
    }

    func run(_ args: [String], tolerate: Set<Int32>) throws -> String {
        lock.lock()
        defer { lock.unlock() }
        calls.append(args)
        let reply = script.isEmpty ? Reply(output: "", exitCode: 0) : script.removeFirst()
        if reply.exitCode != 0 && !tolerate.contains(reply.exitCode) {
            throw CoreBridgeError.cliFailed(command: args.first ?? "tradewind")
        }
        return reply.output
    }
}

@Suite struct CoreBridgeTests {
    static let statusJSON = """
    {"core":{"running":true,"pid":2640,"version":"v1.19.34"},"mode":"rule","sys_proxy":{"applied":true,"services":1},"profile":{"uid":"b89ed456","name":"TrojanFlare"},"panel_url":"http://127.0.0.1:39097/ui/abc123/"}
    """

    @Test func statusDecodesFromStdoutEvenWithExitOne() throws {
        // 退出码契约：exit 1（内核异常）不算进程错误，stdout JSON 仍解码（M2-plan §2.5）
        let fake = FakeRunner(FakeRunner.Reply(output: Self.statusJSON + "\n", exitCode: 1))
        let report = try CoreBridge(runner: fake).status()
        #expect(report.core.running)
        #expect(report.core.pid == 2640)
        #expect(report.mode == "rule")
        #expect(report.sysProxy.applied && report.sysProxy.services == 1)
        #expect(report.profile.uid == "b89ed456")
        #expect(report.panelURL == "http://127.0.0.1:39097/ui/abc123/")
        #expect(fake.calls == [["status", "--json"]])
    }

    @Test func statusRejectsNonContractExitCodes() {
        let fake = FakeRunner(FakeRunner.Reply(output: "", exitCode: 2))
        #expect(throws: CoreBridgeError.cliFailed(command: "status")) {
            try CoreBridge(runner: fake).status()
        }
    }

    @Test func statusRejectsGarbageOutput() {
        let fake = FakeRunner(FakeRunner.Reply(output: "core: not reachable\n", exitCode: 1))
        #expect {
            _ = try CoreBridge(runner: fake).status()
        } throws: { error in
            if case CoreBridgeError.decode = error { return true }
            return false
        }
    }

    @Test func controlPlaneOperationsPassExactArguments() throws {
        let fake = FakeRunner(
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0)
        )
        let bridge = CoreBridge(runner: fake)
        try bridge.select(group: "🚀 节点选择", node: "🇭🇰 HK-01")
        try bridge.setMode("global")
        try bridge.sysProxy(on: false)
        try bridge.stopCore()
        try bridge.panelURL()
        #expect(fake.calls == [
            ["select", "🚀 节点选择", "🇭🇰 HK-01"],
            ["mode", "global"],
            ["sysproxy", "off"],
            ["stop"],
            ["panel", "--url"],
        ])
    }

    @Test func cliPathResolutionPrefersEnvThenCommonLocations() {
        let existing = "/tmp/tw-cli-test"
        #expect(CLIPath.resolve(
            environment: [CLIPath.envKey: existing],
            fileExists: { $0 == existing }
        ) == existing)
        #expect(CLIPath.resolve(environment: [:], fileExists: { _ in false }) == nil)
    }
}

@Suite struct GatewayClientTests {
    @Test func parsesPanelURLIntoBaseAndToken() throws {
        let client = try #require(GatewayClient(panelURL: "http://127.0.0.1:39097/ui/abc123def/"))
        #expect(client.baseURL.absoluteString == "http://127.0.0.1:39097")
        #expect(client.token == "abc123def")
    }

    @Test func rejectsMalformedPanelURLs() {
        #expect(GatewayClient(panelURL: "") == nil)
        #expect(GatewayClient(panelURL: "http://127.0.0.1:39097/") == nil)
        #expect(GatewayClient(panelURL: "http://127.0.0.1:39097/ui/") == nil)
    }

    @Test func requestsCarryExplicitCookieHeader() throws {
        // 凭据显式 Cookie 头（M2-plan §2.1）：不走 URLSession cookie storage
        let client = try #require(GatewayClient(panelURL: "http://127.0.0.1:39097/ui/tok/"))
        let request = client.makeRequest(path: "traffic")
        #expect(request.url?.absoluteString == "http://127.0.0.1:39097/traffic")
        #expect(request.value(forHTTPHeaderField: "Cookie") == "tradewind_panel=tok")
        #expect(request.value(forHTTPHeaderField: "Origin") == "http://127.0.0.1:39097")
    }
}
