import Foundation
import Testing
@testable import TradewindKit

/// 增强链页模型（M3 批③ B2 转正，batch3 §3.4）：桥参数序/stdin 传参/busy 防抖/
/// error 字段 toast。壳层单测绝不真拉 tradewind 进程（FakeRunner 同契约）。
@Suite(.serialized) @MainActor struct EnhanceModelTests {
    private let showEnabled = """
    {"slot":"rules","uid":"old01","exists":true,"content":"- DOMAIN-SUFFIX,keep.example,DIRECT\\n"}
    """
    private let showEmpty = """
    {"slot":"rules","uid":"old01","exists":false,"content":""}
    """
    private let showGlobal = """
    {"slot":"global-merge","uid":"","exists":true,"content":"rules-prepend:\\n  - DOMAIN-SUFFIX,pre.example,DIRECT\\n"}
    """
    private let showClearedResidue = """
    {"slot":"rules","uid":"old01","exists":true,"content":""}
    """
    private let setOK = #"{"ok":true}"#
    private let setError = #"{"error":"enhance 语义校验未通过（mihomo -t），槽已回滚: yaml error at line 9"}"#
    private let clearOK = #"{"ok":true}"#

    // MARK: 桥参数与 stdin

    @Test func enhanceShowPassesExactArguments() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: showEnabled, exitCode: 0))
        let report = try await CoreBridge(runner: fake).enhanceShow(slot: "rules")
        #expect(report.exists)
        #expect(report.uid == "old01")
        #expect(fake.calls == [["enhance", "show", "rules", "--json"]])
    }

    @Test func enhanceSetCarriesContentViaStdinAndWriteTimeout() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: setOK, exitCode: 0))
        try await CoreBridge(runner: fake).enhanceSet(slot: "rules", content: "- MATCH,DIRECT\n")
        // 参数序：位置参数在前、flag 在后（Go flag 在首个非 flag 参数处停止）
        #expect(fake.calls == [["enhance", "set", "rules", "--file", "-", "--json"]])
        // 内容经 stdin（绝不经 argv：fake.calls 内无内容痕迹）
        #expect(fake.inputs == ["- MATCH,DIRECT\n"])
        #expect(fake.timeouts == [CoreBridge.writeTimeout])
    }

    @Test func enhanceClearUsesWriteTimeout() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: clearOK, exitCode: 0))
        try await CoreBridge(runner: fake).enhanceClear(slot: "rules")
        #expect(fake.calls == [["enhance", "clear", "rules", "--json"]])
        #expect(fake.timeouts == [CoreBridge.writeTimeout])
        #expect(fake.inputs.isEmpty) // clear 无 stdin
    }

    @Test func enhanceSetSurfacesErrorFieldAsCliMessage() async {
        let fake = FakeRunner(FakeRunner.Reply(output: setError, exitCode: 0))
        await #expect(throws: CoreBridgeError.cliMessage(
            "enhance 语义校验未通过（mihomo -t），槽已回滚: yaml error at line 9")) {
            try await CoreBridge(runner: fake).enhanceSet(slot: "rules", content: "broken")
        }
    }

    // MARK: 模型动作

    private func makeModel(_ replies: [FakeRunner.Reply]) -> (EnhanceModel, FakeRunner) {
        let fake = FakeRunner(replies)
        return (EnhanceModel(bridge: CoreBridge(runner: fake)), fake)
    }

    @Test func loadPopulatesAllFourSlots() async {
        // 逐槽探测：脚本按调用序应答（Slot.allCases 序 = global-merge/rules/proxies/groups）
        let fake = FakeRunner([
            FakeRunner.Reply(output: showGlobal, exitCode: 0),
            FakeRunner.Reply(output: showEnabled, exitCode: 0),
            FakeRunner.Reply(output: showEmpty, exitCode: 0),
            FakeRunner.Reply(output: showEmpty, exitCode: 0),
        ])
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        await model.load()
        #expect(model.isEnabled(.globalMerge))
        #expect(model.isEnabled(.rules))
        #expect(!model.isEnabled(.proxies))
        #expect(model.content(of: .globalMerge).contains("rules-prepend"))
        #expect(fake.calls.count == 4)
    }

    @Test func clearedResidueFileIsNotEnabled() async {
        // clear 语义=写 0 字节文件（batch3 §3.1）：exists=true 但内容空白属空转态，
        // 徽标不得"已启用"（批③发版冒烟实测修正）
        let fake = FakeRunner(FakeRunner.Reply(output: showClearedResidue, exitCode: 0))
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        await model.load()
        #expect(!model.isEnabled(.rules))
    }

    @Test func loadSkippedWhileBusy() async {
        let fake = FakeRunner()
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        model.testHooksSetBusy(true)
        await model.load()
        #expect(fake.calls.isEmpty) // busy 防抖：在途不探测
    }

    @Test func saveSuccessRefreshesBadgesAndToasts() async {
        // set 后逐槽刷新（4 次 show）→ 徽标权威值落地 + toast 文案
        let fake = FakeRunner([
            FakeRunner.Reply(output: setOK, exitCode: 0),      // set
            FakeRunner.Reply(output: showEnabled, exitCode: 0), // 刷新 global-merge
            FakeRunner.Reply(output: showEnabled, exitCode: 0), // 刷新 rules
            FakeRunner.Reply(output: showEmpty, exitCode: 0),   // 刷新 proxies
            FakeRunner.Reply(output: showEmpty, exitCode: 0),   // 刷新 groups
        ])
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        let saved = await model.save(slot: .rules, content: "- DOMAIN,x,DIRECT\n")
        #expect(saved)
        #expect(model.isEnabled(.rules))
        #expect(model.toast == "增强链已更新，配置已热重载")
        #expect(fake.calls.first == ["enhance", "set", "rules", "--file", "-", "--json"])
    }

    @Test func saveFailureToastsErrorFieldWordingAndKeepsSheetOpen() async {
        let fake = FakeRunner(FakeRunner.Reply(output: setError, exitCode: 0))
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        let saved = await model.save(slot: .rules, content: "broken")
        #expect(!saved) // 视图据此不收起 sheet（槽内容未被破坏）
        #expect(model.toast?.contains("槽已回滚") == true) // 失败 toast=error 字段文案
        #expect(fake.calls.count == 1) // 失败不刷新
    }

    @Test func clearSuccessToasts() async {
        let fake = FakeRunner([
            FakeRunner.Reply(output: clearOK, exitCode: 0),
            FakeRunner.Reply(output: showEmpty, exitCode: 0),
            FakeRunner.Reply(output: showEmpty, exitCode: 0),
            FakeRunner.Reply(output: showEmpty, exitCode: 0),
            FakeRunner.Reply(output: showEmpty, exitCode: 0),
        ])
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        let cleared = await model.clear(slot: .rules)
        #expect(cleared)
        #expect(!model.isEnabled(.rules))
        #expect(model.toast == "增强链已清除，配置已热重载")
    }

    @Test func busyGuardRejectsSaveAndClear() async {
        let fake = FakeRunner()
        let model = EnhanceModel(bridge: CoreBridge(runner: fake))
        model.testHooksSetBusy(true)
        let saved = await model.save(slot: .rules, content: "- x\n")
        let cleared = await model.clear(slot: .rules)
        #expect(!saved && !cleared)
        #expect(fake.calls.isEmpty)
    }

    @Test func unavailableBridgeKeepsReadonly() async {
        let model = EnhanceModel(bridge: nil)
        #expect(!model.available)
        await model.load()
        let saved = await model.save(slot: .rules, content: "- x\n")
        #expect(!saved)
        #expect(model.slots.isEmpty) // CLI 缺失：页面降级只读
    }
}
