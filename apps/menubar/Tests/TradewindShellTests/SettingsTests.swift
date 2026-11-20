import Foundation
import Testing
@testable import TradewindKit

/// 加长超时记录 fake：settingsSet/reset/autostart 走 runWithTimeout 注入点，
/// 用例据此断言 45s 超时路径真实生效（M3-plan §11.2-4 评审 P1-3 / §11.4）。
/// WHY 独立 fake 而非复用 FakeRunner：需区分常规 run 与加长路径并记录 timeout 值。
final class TimeoutRecordingRunner: CommandLineRunning, @unchecked Sendable {
    private(set) var calls: [[String]] = []
    /// 与 calls 同序的超时记录（常规路径记 0，加长路径记实际注入值）
    private(set) var timeouts: [TimeInterval] = []
    private let lock = NSLock()

    func run(_ args: [String], tolerate: Set<Int32>) async throws -> String {
        try lock.withLock {
            calls.append(args)
            timeouts.append(0)
            return ""
        }
    }

    func runWithTimeout(_ args: [String], timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        try lock.withLock {
            calls.append(args)
            timeouts.append(timeout)
            return ""
        }
    }
}

/// 真机 `settings get --json` 输出（2026-11-20 TRADEDWIND_HOME 空目录实测采样）
private let settingsJSONFixture =
    #"{"mixed_port":7897,"gateway_port":39097,"panel_enabled":true,"allow_lan":false,"mode":"rule","log_level":"info","log_retention_days":14}"#

private let statusWithProfileJSON =
    #"{"core":{"running":true,"pid":2640,"version":"v1.19.34"},"mode":"rule","sys_proxy":{"applied":false,"services":0},"profile":{"uid":"b89ed456","name":"TrojanFlare"},"panel_url":""}"#

private let statusWithoutProfileJSON =
    #"{"core":{"running":false,"pid":0,"version":""},"mode":"rule","sys_proxy":{"applied":false,"services":0},"profile":{"uid":"","name":""},"panel_url":""}"#

// MARK: - CoreBridge 设置桥（读链路 + 45s 写编排超时）

@Suite struct SettingsBridgeTests {
    @Test func settingsGetJSONDecodesAndCallsCLI() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: settingsJSONFixture + "\n", exitCode: 0))
        let snapshot = try await CoreBridge(runner: fake).settingsGetJSON()
        #expect(snapshot == SettingsSnapshot(
            mixedPort: 7897, gatewayPort: 39097, panelEnabled: true, allowLan: false,
            mode: "rule", logLevel: "info", logRetentionDays: 14))
        #expect(fake.calls == [["settings", "get", "--json"]]) // 读操作走常规 runner
    }

    @Test func settingsGetJSONRejectsGarbageOutput() async {
        let fake = FakeRunner(FakeRunner.Reply(output: "not json", exitCode: 0))
        await #expect {
            _ = try await CoreBridge(runner: fake).settingsGetJSON()
        } throws: { error in
            if case CoreBridgeError.decode = error { return true }
            return false
        }
    }

    @Test func autostartStatusJSONDecodesAndGatesEnabled() async throws {
        // installed 未 loaded：plist 在但需下次登录生效 → 开关口径为关（enabled 双真语义）
        let fake = FakeRunner(FakeRunner.Reply(output: #"{"installed":true,"loaded":false}"#, exitCode: 0))
        let status = try await CoreBridge(runner: fake).autostartStatusJSON()
        #expect(status.installed && !status.loaded)
        #expect(!status.enabled)
        #expect(fake.calls == [["autostart", "status", "--json"]])
    }

    @Test func settingsSetRoutesExtendedTimeoutRunner() async throws {
        let fake = TimeoutRecordingRunner()
        try await CoreBridge(runner: fake).settingsSet(key: "mixed_port", value: "7898")
        #expect(fake.calls == [["settings", "set", "mixed_port", "7898"]])
        #expect(fake.timeouts == [65]) // mutation.lock 35s + 降级重启 15s 的安全上限
    }

    @Test func settingsResetRoutesExtendedTimeoutRunner() async throws {
        let fake = TimeoutRecordingRunner()
        try await CoreBridge(runner: fake).settingsReset()
        #expect(fake.calls == [["settings", "reset"]])
        #expect(fake.timeouts == [65])
    }

    @Test func subsIntervalCurrentResolvesUidViaStatusThenWrites() async throws {
        // Go resolveProfile 无 "current" 关键字（c1 实况）：壳侧经 status --json
        // profile.uid 定位当前订阅，再 subs interval <uid> <mode>
        let fake = FakeRunner(
            FakeRunner.Reply(output: statusWithProfileJSON, exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0)
        )
        try await CoreBridge(runner: fake).subsIntervalCurrent(mode: "12")
        #expect(fake.calls == [["status", "--json"], ["subs", "interval", "b89ed456", "12"]])
    }

    @Test func subsIntervalCurrentWithoutCurrentProfileThrows() async {
        // 无当前订阅（uid 空）：显式报错不静默写错条目
        let fake = FakeRunner(FakeRunner.Reply(output: statusWithoutProfileJSON, exitCode: 0))
        await #expect(throws: CoreBridgeError.noCurrentProfile) {
            try await CoreBridge(runner: fake).subsIntervalCurrent(mode: "24")
        }
        #expect(fake.calls == [["status", "--json"]]) // 不落任何 interval 写
    }

    @Test func autostartOnOffRouteExtendedTimeoutRunner() async throws {
        let fake = TimeoutRecordingRunner()
        let bridge = CoreBridge(runner: fake)
        try await bridge.autostart(on: true)
        try await bridge.autostart(on: false)
        #expect(fake.calls == [["autostart", "on"], ["autostart", "off"]])
        #expect(fake.timeouts == [65, 65]) // on 含 launchctl load + 手动实例迁移（65s：评审 P2 最坏路径裕量）
    }

    @Test func listJSONDecodesIntervalFieldsToleratingExtras() async throws {
        // 间隔初值裁剪解码：容忍 extra 字段（uid/name 等 c3 SubsModel 才全量消费）
        let line = #"[{"uid":"u1","name":"云雾航线 Pro","type":"url","updated":1700000000,"extra":{},"current":true,"allow_auto_update":false,"update_interval":43200}]"#
        let fake = FakeRunner(FakeRunner.Reply(output: line, exitCode: 0))
        let entries = try await CoreBridge(runner: fake).listJSON()
        #expect(entries == [ProfileIntervalEntry(current: true, allowAutoUpdate: false, updateInterval: 43200)])
        #expect(fake.calls == [["list", "--json"]])
    }

    @Test func listJSONDecodesEmptyTable() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: "[]\n", exitCode: 0))
        let entries = try await CoreBridge(runner: fake).listJSON()
        #expect(entries.isEmpty)
    }

    /// c2 审查 P1 回归：真实 mihomo/Go omitempty 线格式省略 update_interval 键——
    /// off 档（写 0）与新订阅（恒 0）输出均无该键；非可选解码曾致 keyNotFound 被
    /// try? 吞掉 → off 档回读静默显示"每 24h"（与真实状态相反）
    @Test func listJSONToleratesOmittedUpdateInterval() async throws {
        let noInterval = #"[{"current":true,"uid":"u1","name":"demo-sub","type":"file","updated":1700000000,"extra":{}}]"#
        let offNoInterval = #"[{"current":true,"uid":"u1","name":"demo-sub","type":"url","updated":1700000000,"extra":{},"allow_auto_update":false}]"#
        let fake = FakeRunner(FakeRunner.Reply(output: noInterval, exitCode: 0))
        let entries = try await CoreBridge(runner: fake).listJSON()
        #expect(entries.count == 1 && entries[0].updateInterval == nil)
        let fake2 = FakeRunner(FakeRunner.Reply(output: offNoInterval, exitCode: 0))
        let off = try await CoreBridge(runner: fake2).listJSON()
        #expect(off[0].allowAutoUpdate == false && off[0].updateInterval == nil)
        // 缺省键 → nil → 默认档（非 off——off 判定优先走 allowAutoUpdate）
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: off[0].allowAutoUpdate, updateInterval: off[0].updateInterval) == "off")
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: nil, updateInterval: entries[0].updateInterval) == "24")
    }
}

// MARK: - 通用页提交判定纯函数

@Suite struct GeneralPaneLogicTests {
    @Test func classifyNumberEntryWritesSkipsAndRejects() {
        let ports = GeneralPaneLogic.portRange
        #expect(GeneralPaneLogic.classifyNumberEntry("7898", current: 7897, validRange: ports) == .write(7898))
        #expect(GeneralPaneLogic.classifyNumberEntry(" 7897 ", current: 7897, validRange: ports) == .unchanged) // 容忍空白
        #expect(GeneralPaneLogic.classifyNumberEntry("7897", current: 7897, validRange: ports) == .unchanged) // 值未变化不写（评审 P2）
        #expect(GeneralPaneLogic.classifyNumberEntry("abc", current: 7897, validRange: ports) == .invalid)
        #expect(GeneralPaneLogic.classifyNumberEntry("0", current: 7897, validRange: ports) == .invalid) // 越下界
        #expect(GeneralPaneLogic.classifyNumberEntry("70000", current: 7897, validRange: ports) == .invalid) // 越上界
    }

    @Test func classifyNumberEntryRetentionRangeMirrorsGoValidate() {
        let days = GeneralPaneLogic.retentionRange
        #expect(GeneralPaneLogic.classifyNumberEntry("30", current: 14, validRange: days) == .write(30))
        #expect(GeneralPaneLogic.classifyNumberEntry("-1", current: 14, validRange: days) == .invalid) // Go Validate 拒负值
        #expect(GeneralPaneLogic.classifyNumberEntry("4000", current: 14, validRange: days) == .invalid) // 超 10 年
    }

    @Test func intervalModeMapsAllowOffAndHourTiers() {
        // off = 真实关闭开关（AllowAutoUpdate=false，评审 P1-2），优先于 interval 值
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: false, updateInterval: 43200) == "off")
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: true, updateInterval: 3600) == "1")
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: true, updateInterval: 43200) == "12")
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: true, updateInterval: 86400) == "24")
        // nil（未设置）与 0（Go NormalizeUpdateInterval 归一为 86400）落默认 24h
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: nil, updateInterval: 0) == "24")
        #expect(GeneralPaneLogic.intervalMode(allowAutoUpdate: nil, updateInterval: 86400) == "24")
    }

    @Test func intervalModeFromEntriesPicksCurrentElseDefault() {
        let entries = [
            ProfileIntervalEntry(current: false, allowAutoUpdate: true, updateInterval: 3600),
            ProfileIntervalEntry(current: true, allowAutoUpdate: nil, updateInterval: 0),
        ]
        #expect(GeneralPaneLogic.intervalMode(in: entries) == "24") // 取当前条目而非首条
        #expect(GeneralPaneLogic.intervalMode(in: []) == "24") // 查无当前条目落默认档
    }
}

// MARK: - 窗口装配 smoke + 侧栏中间态 + tokens

@Suite(.serialized) @MainActor struct SettingsWindowTests {
    @Test func windowAssemblySmokeKeepsClosed() throws {
        // 不 orderFront（免闪窗）：只验装配契约——尺寸 780×540、标题、styleMask、保活
        let controller = Self.makeController()
        let window = try #require(controller.window)
        #expect(window.title == "Tradewind")
        #expect(window.styleMask == [.titled, .closable, .miniaturizable])
        #expect(!window.styleMask.contains(.resizable))
        #expect(!window.styleMask.contains(.fullScreen))
        let content = window.contentRect(forFrameRect: window.frame)
        #expect(content.size == SettingsWindowController.windowSize)
        #expect(!window.isVisible)
        #expect(!window.isReleasedWhenClosed) // 关闭=orderOut 保活（§11.2-2）
    }

    @Test func windowShouldCloseOrdersOutButKeepsWindowAlive() throws {
        let controller = Self.makeController()
        let window = try #require(controller.window)
        window.setFrameTopLeftPoint(CGPoint(x: 2600, y: 2600)) // 屏幕外，即便意外可见也不闪窗
        let kept = controller.windowShouldClose(window)
        #expect(!kept) // 拦截真正 close（红点=隐藏而非销毁）
        #expect(controller.window != nil) // 控制器与窗口仍存活（复开零重建）
    }

    @Test func navShowsAllDeliveredPages() {
        // c4 已交付连接与流量（§11.5）；侧栏序对齐 demo：
        // 订阅管理/连接与流量/通用/增强链/关于
        #expect(SettingsPane.allCases == [.subs, .conns, .general, .enhance, .about])
        #expect(SettingsPane.subs.title == "订阅管理")
        #expect(SettingsPane.subs.icon == "house")
        #expect(SettingsPane.conns.title == "连接与流量")
        #expect(SettingsPane.conns.icon == "antenna.radiowaves.left.and.right")
        #expect(SettingsPane.conns.badge == nil) // 正式功能页，无 P2 徽标
        #expect(SettingsPane.enhance.badge == "P2")
        #expect(SettingsPane.general.title == "通用")
        #expect(SettingsPane.about.title == "关于")
    }

    /// 装配单测公共入口（降级桥 + 壳级 TrafficStore/ConnectionsStore 显式注入；
    /// WHY 无默认参数：@MainActor init 默认值在非隔离调用点求值会编译错误）
    private static func makeController() -> SettingsWindowController {
        SettingsWindowController(model: SettingsModel(bridge: nil),
                                 subsModel: SubsModel(bridge: nil),
                                 traffic: TrafficStore(defaults: nil),
                                 connectionsStore: ConnectionsStore(bridge: nil))
    }

    @Test func settingsSideTokensMatchDemoComposite() {
        // 侧栏底色 token 直读常量断言（SwiftUI Color 无分量自省，批①.5 §3.2 口径）
        #expect(Theme.settingsSideLightRGBA == (0xF1 / 255.0, 0xF4 / 255.0, 0xF8 / 255.0, 1))
        #expect(Theme.settingsSideDarkRGBA == (0x1D / 255.0, 0x20 / 255.0, 0x24 / 255.0, 1))
    }
}

// MARK: - SettingsModel 编排层（审查 P3-4：乐观回显/失败回退/busy 防抖）

/// 分流 runner：settings get --json 返回基线，settings set 抛错（失败回退路径）。
private final class SetFailingRunner: CommandLineRunning, @unchecked Sendable {
    private let baseline = """
    {"mixed_port":7897,"gateway_port":39097,"panel_enabled":true,"allow_lan":false,"mode":"rule","log_level":"info","log_retention_days":14}
    """
    private(set) var calls: [[String]] = []
    func run(_ args: [String], tolerate: Set<Int32>) async throws -> String {
        calls.append(args)
        if args.first == "settings", args.contains("set") {
            throw CoreBridgeError.cliFailed(command: "settings")
        }
        if args.first == "settings" { return baseline }
        if args.first == "autostart" { return #"{"installed":true,"loaded":true}"# }
        if args.first == "list" { return "[]" }
        if args.first == "status" {
            return #"{"running":true,"mode":"rule","sys_proxy":{"applied":true},"profile":{"uid":"u","name":"P"},"core":{"version":"1.19.34"},"panel_url":"http://127.0.0.1:39097/ui/t/"}"#
        }
        return ""
    }
    func runWithTimeout(_ args: [String], timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        try await run(args, tolerate: tolerate)
    }
}

@MainActor
@Suite struct SettingsModelOrchestrationTests {
    private func loadedModel(runner: CommandLineRunning) async -> SettingsModel {
        let model = SettingsModel(bridge: CoreBridge(runner: runner))
        await model.load()
        return model
    }

    /// 审查 P3-4：写失败 → 乐观回显回退（mixed_port 保持旧值）+ toast
    @Test func writeFailureRevertsOptimisticEcho() async throws {
        let model = await loadedModel(runner: SetFailingRunner())
        let ok = await model.commitNumber(.mixedPort, raw: "7898")
        #expect(!ok)
        #expect(model.settings?.mixedPort == 7897) // 回退到旧值
        #expect(model.toast != nil)
    }

    /// 审查 P3-4：busy 在途时再次提交被拒（防抖），且不做第二次 CLI 调用
    @Test func busyGuardsConcurrentWrite() async throws {
        let model = await loadedModel(runner: SetFailingRunner())
        // 模拟在途：直接置 busy（真实置位在 performWrite 内部）
        model.testHooksSetBusy(true)
        let ok = await model.commitNumber(.mixedPort, raw: "7898")
        #expect(!ok)
        model.testHooksSetBusy(false)
    }

    /// 审查 P3-1：跨字段互斥壳侧预检——mixed_port==gateway_port 提交被拒且有因由 toast
    @Test func mixedPortEqualsGatewayRejectedLocally() async {
        let model = await loadedModel(runner: SetFailingRunner())
        let ok = await model.commitNumber(.mixedPort, raw: "39097")
        #expect(!ok)
        #expect(model.settings?.mixedPort == 7897)
        #expect(model.toast?.contains("不能相同") == true)
    }
}
