import Foundation
import Testing
@testable import TradewindKit

/// 自升级解码模型三态 + UpdateModel 动作（M3 批③ B1，batch3 §2.4）。
/// WHY .serialized：与共享 FakeRunner 状态隔离；本套件自身串行即可。
@Suite(.serialized) @MainActor struct UpdateModelTests {
    private let checkAvailable = """
    {"current":"v0.5.0-m3b5","latest":"v0.5.0-m3c1","update_available":true,"asset":"tradewind-v0.5.0-m3c1-darwin-arm64.tar.gz","asset_size":27848296}
    """
    private let checkUpToDate = """
    {"current":"v0.5.0-m3c1","latest":"v0.5.0-m3c1","update_available":false,"asset":"tradewind-v0.5.0-m3c1-darwin-arm64.tar.gz","asset_size":27848296}
    """
    private let checkError = """
    {"error":"未配置 release_token（数据目录），见 README 自升级章节"}
    """
    private let selfUpdateOK = """
    {"ok":true,"updated_to":"v0.5.0-m3c1"}
    """
    private let selfUpdateError = """
    {"error":"token 缺失或无效"}
    """

    // MARK: 解码三态映射

    @Test func checkReportMapsToThreeStates() throws {
        let available = try UpdateCheckReport.decode(self.checkAvailable)
        #expect(available.state == .available(version: "v0.5.0-m3c1",
                                             asset: "tradewind-v0.5.0-m3c1-darwin-arm64.tar.gz"))
        let upToDate = try UpdateCheckReport.decode(self.checkUpToDate)
        #expect(upToDate.state == .upToDate(version: "v0.5.0-m3c1"))
        let failed = try UpdateCheckReport.decode(self.checkError)
        #expect(failed.state == .failed(message: "未配置 release_token（数据目录），见 README 自升级章节"))
    }

    @Test func checkReportResultWithoutLatestKeyFails() throws {
        // 恒退出 0 契约下结果行必有 latest；缺键=输出损坏 → 失败态而非误报最新
        let broken = try UpdateCheckReport.decode(#"{"current":"v1","update_available":false}"#)
        #expect(broken.state == .failed(message: "版本信息缺失"))
    }

    @Test func selfUpdateReportThrowsErrorFieldWording() throws {
        let ok = try SelfUpdateReport.decode(self.selfUpdateOK)
        #expect(try ok.updatedToOrThrow() == "v0.5.0-m3c1")
        let failed = try SelfUpdateReport.decode(self.selfUpdateError)
        #expect(throws: CoreBridgeError.cliMessage("token 缺失或无效")) {
            try failed.updatedToOrThrow()
        }
    }

    // MARK: 桥参数与超时档

    @Test func versionCheckPassesExactArguments() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: self.checkAvailable + "\n", exitCode: 0))
        let report = try await CoreBridge(runner: fake).versionCheckJSON()
        #expect(report.updateAvailable == true)
        #expect(fake.calls == [["version", "--check", "--json"]])
        #expect(fake.timeouts.isEmpty) // 读档走常规 run，不记超时
    }

    @Test func selfUpdateUses180sTimeout() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: self.selfUpdateOK, exitCode: 0))
        let version = try await CoreBridge(runner: fake).selfUpdate()
        #expect(version == "v0.5.0-m3c1")
        #expect(fake.calls == [["self-update", "--json"]])
        #expect(fake.timeouts == [CoreBridge.updateTimeout])
        #expect(CoreBridge.updateTimeout == 180)
    }

    @Test func selfUpdateSurfacesErrorFieldAsCliMessage() async {
        let fake = FakeRunner(FakeRunner.Reply(output: self.selfUpdateError, exitCode: 0))
        // 恒退出 0 契约：exit 0 + error 字段 → 抛 cliMessage（非 cliFailed）
        await #expect(throws: CoreBridgeError.cliMessage("token 缺失或无效")) {
            try await CoreBridge(runner: fake).selfUpdate()
        }
    }

    // MARK: UpdateModel 动作与 busy 防抖

    private func makeModel(_ replies: [FakeRunner.Reply]) -> (UpdateModel, FakeRunner) {
        let fake = FakeRunner(replies)
        return (UpdateModel(bridge: CoreBridge(runner: fake)), fake)
    }

    @Test func checkForUpdatePopulatesStateAndNoBusyLeak() async {
        let (model, fake) = makeModel([FakeRunner.Reply(output: self.checkAvailable + "\n", exitCode: 0)])
        await model.checkForUpdate()
        #expect(model.checkState == .available(version: "v0.5.0-m3c1",
                                               asset: "tradewind-v0.5.0-m3c1-darwin-arm64.tar.gz"))
        #expect(!model.busy)
        #expect(fake.calls.count == 1)
    }

    @Test func checkFailureShowsErrorFieldMessage() async {
        let (model, _) = makeModel([FakeRunner.Reply(output: self.checkError, exitCode: 0)])
        await model.checkForUpdate()
        guard case .failed(let message) = model.checkState else {
            Issue.record("state = \(model.checkState)")
            return
        }
        // 文案直接取 error 字段（含 token 缺失引导），不掺壳侧措辞
        #expect(message == "未配置 release_token（数据目录），见 README 自升级章节")
    }

    @Test func applyUpdateSuccessToastsAndRechecks() async {
        // 更新成功 → toast 文案 + 复查回"已是最新"
        let (model, fake) = makeModel([
            FakeRunner.Reply(output: self.selfUpdateOK, exitCode: 0),      // self-update
            FakeRunner.Reply(output: self.checkUpToDate + "\n", exitCode: 0), // 复查
        ])
        await model.applyUpdate()
        #expect(model.lastUpdatedTo == "v0.5.0-m3c1")
        #expect(model.checkState == .upToDate(version: "v0.5.0-m3c1"))
        #expect(model.toast?.contains("已更新至 v0.5.0-m3c1") == true)
        #expect(model.toast?.contains("更新过程代理有数秒中断") == true)
        #expect(fake.calls == [["self-update", "--json"], ["version", "--check", "--json"]])
    }

    @Test func applyUpdateFailureToastsErrorFieldWording() async {
        let (model, fake) = makeModel([FakeRunner.Reply(output: self.selfUpdateError, exitCode: 0)])
        await model.applyUpdate()
        #expect(model.toast == "token 缺失或无效")
        #expect(model.lastUpdatedTo == nil)
        #expect(fake.calls.count == 1) // 失败不复查
    }

    @Test func busyGuardRejectsReentry() async {
        let (model, fake) = makeModel([FakeRunner.Reply(output: self.checkAvailable, exitCode: 0)])
        model.testHooksSetBusy(true)
        await model.checkForUpdate()
        await model.applyUpdate()
        #expect(fake.calls.isEmpty) // 防抖：在途拒绝一切动作
        #expect(model.checkState == .idle)
        model.testHooksSetBusy(false)
    }

    @Test func unavailableBridgeKeepsIdleAndAvailableFalse() async {
        let model = UpdateModel(bridge: nil)
        #expect(!model.available)
        await model.checkForUpdate()
        await model.applyUpdate()
        #expect(model.checkState == .idle) // CLI 缺失：区块只读，无崩溃无请求
    }
}
