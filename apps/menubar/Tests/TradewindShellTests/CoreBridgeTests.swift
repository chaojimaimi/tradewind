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
    /// runWithTimeout 收到的超时档（批③：selfUpdate 180s 档断言；读/写档缺省转发不记录）
    private(set) var timeouts: [TimeInterval] = []
    private var script: [Reply] = []
    private let lock = NSLock()

    init(_ replies: Reply...) {
        script = replies
    }

    /// 数组形态（脚本化场景与 Swift 无数组展开变参的折中）。
    init(_ replies: [Reply]) {
        script = replies
    }

    func run(_ args: [String], tolerate: Set<Int32>) async throws -> String {
        // WHY withLock 而非 lock/defer unlock：async 上下文禁用手动 lock/unlock
        // （编译器警告 unavailable from asynchronous contexts），scoped 形态互斥语义不变。
        try lock.withLock {
            calls.append(args)
            let reply = script.isEmpty ? Reply(output: "", exitCode: 0) : script.removeFirst()
            if reply.exitCode != 0 && !tolerate.contains(reply.exitCode) {
                throw CoreBridgeError.cliFailed(command: args.first ?? "tradewind")
            }
            return reply.output
        }
    }

    /// 记录超时档后转发常规 run（假执行器无进程可杀，超时值仅真实执行器有语义；
    /// 记录供 selfUpdate/updateTimeout 契约断言）。
    func runWithTimeout(_ args: [String], timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        lock.withLock { timeouts.append(timeout) }
        return try await run(args, tolerate: tolerate)
    }

    /// 记录 stdin 内容后转发（批③ c6：enhance set 内容经 stdin 传参断言）。
    func runWithInput(_ args: [String], input: String, timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        lock.withLock {
            timeouts.append(timeout)
            inputs.append(input)
        }
        return try await run(args, tolerate: tolerate)
    }

    /// runWithInput 收到的 stdin 内容（序与 calls 对齐）。
    private(set) var inputs: [String] = []
}

@Suite struct CoreBridgeTests {
    static let statusJSON = """
    {"core":{"running":true,"pid":2640,"version":"v1.19.34"},"mode":"rule","sys_proxy":{"applied":true,"services":1},"profile":{"uid":"b89ed456","name":"TrojanFlare"},"panel_url":"http://127.0.0.1:39097/ui/abc123/"}
    """

    @Test func statusDecodesFromStdoutEvenWithExitOne() async throws {
        // 退出码契约：exit 1（内核异常）不算进程错误，stdout JSON 仍解码（M2-plan §2.5）
        let fake = FakeRunner(FakeRunner.Reply(output: Self.statusJSON + "\n", exitCode: 1))
        let report = try await CoreBridge(runner: fake).status()
        #expect(report.core.running)
        #expect(report.core.pid == 2640)
        #expect(report.mode == "rule")
        #expect(report.sysProxy.applied && report.sysProxy.services == 1)
        #expect(report.profile.uid == "b89ed456")
        #expect(report.panelURL == "http://127.0.0.1:39097/ui/abc123/")
        #expect(fake.calls == [["status", "--json"]])
    }

    @Test func statusRejectsNonContractExitCodes() async {
        let fake = FakeRunner(FakeRunner.Reply(output: "", exitCode: 2))
        await #expect(throws: CoreBridgeError.cliFailed(command: "status")) {
            try await CoreBridge(runner: fake).status()
        }
    }

    @Test func statusRejectsGarbageOutput() async {
        let fake = FakeRunner(FakeRunner.Reply(output: "core: not reachable\n", exitCode: 1))
        await #expect {
            _ = try await CoreBridge(runner: fake).status()
        } throws: { error in
            if case CoreBridgeError.decode = error { return true }
            return false
        }
    }

    /// 批④终审 P2：sysProxy 升 65s write 档——CLI on/off 纳 mutation.lock（最坏等锁
    /// 35s），10s 档会把正常等待误判超时抛"可能已部分生效"。
    @Test func sysProxyRoutesExtendedTimeout() async throws {
        let fake = TimeoutRecordingRunner()
        try await CoreBridge(runner: fake).sysProxy(on: true)
        #expect(fake.calls == [["sysproxy", "on"]])
        #expect(fake.timeouts == [65])
    }

    @Test func controlPlaneOperationsPassExactArguments() async throws {
        let fake = FakeRunner(
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0)
        )
        let bridge = CoreBridge(runner: fake)
        try await bridge.select(group: "🚀 节点选择", node: "🇭🇰 HK-01")
        try await bridge.setMode("global")
        try await bridge.sysProxy(on: false)
        try await bridge.stopCore()
        _ = try await bridge.panelURL()
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

    @Test func requestsNeverOverrideHostHeader() throws {
        // 回归（M2 真机缺陷）：手动 Host 头携带 scheme 属非法值 → 网关 400 → 数据面降级；
        // Host 由 CFNetwork 按 URL 生成，请求不得覆写
        let client = try #require(GatewayClient(panelURL: "http://127.0.0.1:39097/ui/tok/"))
        for path in ["proxies", "traffic"] {
            let request = client.makeRequest(path: path)
            #expect(request.value(forHTTPHeaderField: "Host") == nil)
        }
    }
}
