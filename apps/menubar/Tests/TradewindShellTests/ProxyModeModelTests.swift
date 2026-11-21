import Foundation
import Testing
@testable import TradewindKit

// MARK: - 系统代理形态编辑（批④ §3.4）：BypassEditorLogic 纯函数 + ProxyModeModel 编排

@Suite struct BypassEditorLogicTests {
    @Test func parseTrimsAndDropsEmptySegments() {
        #expect(BypassEditorLogic.parse("*.local, 169.254/16 , <local> ,") ==
            ["*.local", "169.254/16", "<local>"])
        #expect(BypassEditorLogic.parse("") == [])
        #expect(BypassEditorLogic.parse("   ") == [])
    }

    @Test func effectiveDisplayFallsBackToDefaultSet() {
        // 未设置（nil/空）→ 缺省集（占位=当前生效值语义）
        #expect(BypassEditorLogic.effectiveDisplay(current: nil) == BypassEditorLogic.defaultDisplay)
        #expect(BypassEditorLogic.effectiveDisplay(current: []) == BypassEditorLogic.defaultDisplay)
        #expect(BypassEditorLogic.effectiveDisplay(current: ["*.corp.example"]) == "*.corp.example")
    }

    @Test func classifyUnchangedShortCircuits() {
        #expect(BypassEditorLogic.classify("", current: nil) == .unchanged)
        #expect(BypassEditorLogic.classify("  ", current: []) == .unchanged)
        #expect(BypassEditorLogic.classify("a.example, b.example", current: ["a.example", "b.example"]) == .unchanged)
    }

    @Test func classifyEmptyResetsToDefault() {
        // 空文本=恢复缺省（Go 空=恢复缺省同语义）；当前有自定义 → write([])
        #expect(BypassEditorLogic.classify("", current: ["*.local"]) == .write([]))
    }

    @Test func classifyRejectsInjectionPatterns() {
        // 空段被 parse 丢弃（设计语义），不在注入拒绝面内
        for bad in ["evil;x", "a b", "x&y", "quote\"d", "brace'}"] {
            let commit = BypassEditorLogic.classify("\(bad),ok.example", current: nil)
            if case .invalid = commit {} else {
                #expect(Bool(false), "pattern \(bad) must be rejected")
            }
        }
    }

    @Test func classifyRejectsOverlongPattern() {
        let long = String(repeating: "a", count: 254)
        if case .invalid = BypassEditorLogic.classify("\(long)", current: nil) {} else {
            #expect(Bool(false), "over-253 pattern must be rejected")
        }
        let ok = String(repeating: "a", count: 253)
        #expect(BypassEditorLogic.classify("\(ok)", current: nil) == .write([ok]))
    }

    @Test func classifyRejectsOverLimitEntries() {
        let entries = (0..<65).map { "h\($0).example" }
        if case .invalid = BypassEditorLogic.classify(entries.joined(separator: ","), current: nil) {} else {
            #expect(Bool(false), "65 entries must be rejected")
        }
        let atLimit = (0..<64).map { "h\($0).example" }
        #expect(BypassEditorLogic.classify(atLimit.joined(separator: ","), current: nil)
            == .write(atLimit))
    }

    @Test func classifyAcceptsDefaultSetShapes() {
        let defaultSet = ["127.0.0.1", "localhost", "*.local", "169.254/16", "<local>"]
        #expect(BypassEditorLogic.classify(defaultSet.joined(separator: ","), current: nil)
            == .write(defaultSet))
    }
}

/// ProxyModeModel 编排（fake runner 绝不真拉 tradewind 进程）：读链、PAC 开关乐观
/// 回显+失败回退、bypass 提交分类、busy 防抖。
@MainActor @Suite struct ProxyModeModelTests {
    private static func settingsJSON(pac: Bool, bypass: [String]?, tun: Bool = false) -> String {
        let bypassPart: String
        switch bypass {
        case nil: bypassPart = ""
        case []: bypassPart = #","bypass_domains":[]"#
        case let .some(list): bypassPart = #","bypass_domains":["# + list.map { "\"\($0)\"" }.joined(separator: ",") + "]"
        }
        return #"{"mixed_port":7897,"gateway_port":39097,"panel_enabled":true,"allow_lan":false,"mode":"rule","log_level":"info","log_retention_days":14,"pac_mode":"#
            + (pac ? "true" : "false") + bypassPart
            + #","tun_enabled":"# + (tun ? "true" : "false") + "}"
    }

    @Test func loadReadsPacKeysAndToleratesMissing() async {
        // 旧 CLI 输出缺 pac_mode/bypass_domains（c1 兼容语义）：pac=false、bypass=nil
        let legacy = FakeRunner(FakeRunner.Reply(
            output: #"{"mixed_port":7897,"gateway_port":39097,"panel_enabled":true,"allow_lan":false,"mode":"rule","log_level":"info","log_retention_days":14}"#,
            exitCode: 0))
        let legacyModel = ProxyModeModel(bridge: CoreBridge(runner: legacy))
        await legacyModel.load()
        #expect(!legacyModel.loadFailed)
        #expect(legacyModel.pacMode == false)
        #expect(legacyModel.currentBypass == nil)

        let fake = FakeRunner(FakeRunner.Reply(output: Self.settingsJSON(pac: true, bypass: ["*.local"]) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        #expect(model.pacMode)
        #expect(model.currentBypass == ["*.local"])
        #expect(fake.calls == [["settings", "get", "--json"]])
    }

    @Test func setPacModeWritesOnOffAndRefreshes() async {
        let fake = FakeRunner(
            FakeRunner.Reply(output: "", exitCode: 0), // settings set pac_mode on
            FakeRunner.Reply(output: Self.settingsJSON(pac: true, bypass: nil) + "\n", exitCode: 0)) // refresh
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.setPacMode(true)
        #expect(fake.calls.contains(["settings", "set", "pac_mode", "on"]))
        #expect(model.pacMode) // 权威态回拉
        #expect(model.toast == "PAC 模式已开启（需面板网关运行）")
    }

    @Test func setPacModeFailureRevertsEcho() async {
        let fake = FakeRunner(
            FakeRunner.Reply(output: Self.settingsJSON(pac: false, bypass: nil) + "\n", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 1)) // settings set 失败
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.setPacMode(true)
        #expect(!model.pacMode) // 失败回退乐观回显
        #expect(model.toast == "PAC 模式保存失败")
    }

    @Test func commitBypassUnchangedSkipsWrite() async {
        let fake = FakeRunner(FakeRunner.Reply(
            output: Self.settingsJSON(pac: false, bypass: ["*.local"]) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.commitBypass(" *.local ") // 规范化后与生效值相同
        #expect(!fake.calls.contains { $0.first == "settings" && $0.count > 2 && $0[1] == "set" })
    }

    @Test func commitBypassInvalidShowsReason() async {
        let fake = FakeRunner(FakeRunner.Reply(
            output: Self.settingsJSON(pac: false, bypass: nil) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.commitBypass("evil;x")
        #expect(model.toast?.hasPrefix("保存失败：非法绕过域名") == true)
        #expect(!fake.calls.contains { $0.first == "settings" && $0.count > 2 && $0[1] == "set" })
    }

    @Test func commitBypassEmptyResetsToDefault() async {
        let fake = FakeRunner(
            FakeRunner.Reply(output: Self.settingsJSON(pac: false, bypass: ["*.local"]) + "\n", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 0), // settings set bypass_domains ""
            FakeRunner.Reply(output: Self.settingsJSON(pac: false, bypass: nil) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.commitBypass("") // 当前有自定义 → 空=恢复缺省
        #expect(fake.calls.contains(["settings", "set", "bypass_domains", ""]))
        #expect(model.currentBypass == nil)
    }

    @Test func commitBypassEmptyOnDefaultIsUnchanged() async {
        let fake = FakeRunner(FakeRunner.Reply(
            output: Self.settingsJSON(pac: false, bypass: nil) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.commitBypass("") // 当前已是缺省（nil）→ unchanged 不写
        #expect(fake.calls.count == 1) // 仅初始 load
    }

    @Test func busyInFlightRejectsMutation() async {
        let fake = FakeRunner()
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        model.testHooksSetBusy(true)
        await model.setPacMode(true)
        #expect(!model.pacMode) // busy 在途：不写不回显
        #expect(fake.calls.isEmpty)
    }

    @Test func missingBridgeDegrades() async {
        let model = ProxyModeModel(bridge: nil)
        await model.load() // 无桥：load no-op（视图层仅在 CLI 可用分支渲染本节）
        #expect(!model.loadFailed)
        #expect(model.currentBypass == nil)
        await model.setPacMode(true) // 无桥：静默拒绝
        #expect(!model.pacMode)
    }

    // MARK: TUN 开关（批⑥ §4）

    @Test func loadReadsTunEnabledKey() async {
        let fake = FakeRunner(FakeRunner.Reply(
            output: Self.settingsJSON(pac: false, bypass: nil, tun: true) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        #expect(model.tunEnabled) // 开关初值来自 settings get --json（读取面）
    }

    @Test func setTUNWritesViaTunOrchestrationAndRefreshes() async {
        // 写入口必须是 `tun on|off` 编排（90s 档）——settings set tun_enabled 在
        // Go 侧被双入口封死，绝不出现在壳的调用里
        let fake = FakeRunner(
            FakeRunner.Reply(output: "", exitCode: 0), // tun on
            FakeRunner.Reply(output: Self.settingsJSON(pac: false, bypass: nil, tun: true) + "\n", exitCode: 0))
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.setTUN(true)
        #expect(fake.calls.contains(["tun", "on"]))
        #expect(!fake.calls.contains { $0 == ["settings", "set", "tun_enabled", "on"] })
        #expect(model.tunEnabled) // 权威态回拉
        #expect(model.toast == "TUN 模式已开启（特权服务托管内核）")
    }

    @Test func setTUNFailureRevertsEcho() async {
        let fake = FakeRunner(
            FakeRunner.Reply(output: Self.settingsJSON(pac: false, bypass: nil, tun: false) + "\n", exitCode: 0),
            FakeRunner.Reply(output: "", exitCode: 1)) // tun on 失败
        let model = ProxyModeModel(bridge: CoreBridge(runner: fake))
        await model.load()
        await model.setTUN(true)
        #expect(!model.tunEnabled) // 失败回退乐观回显
        #expect(model.toast == "TUN 模式切换失败")
    }
}
