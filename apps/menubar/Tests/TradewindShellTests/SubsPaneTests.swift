import Foundation
import Testing
@testable import TradewindKit

// MARK: - c3 订阅管理页测试（M3-plan §11.3 页 1 / §11.4）
//
// 覆盖：CoreBridge 订阅桥 calls/65s 超时断言（含 --force 与 add flags 前置形态）、
// list --json 全量卡解码（omitempty 缺键容忍——ProfileIntervalEntry 同款 fixture
// 形态）、流量条阈值/格式化纯函数、SubsModel 编排（load/add/delete current 重拉/
// busy 防抖/updateAll remote 过滤）。

/// 真机 `list --json` 输出形态采样：双条目——remote 当前订阅（全字段）+ file 型
/// 本地订阅（omitempty 缺 allow_auto_update/update_interval 键，expire=0 未知）。
private let subsListFixture =
    #"[{"current":true,"uid":"b89ed456","name":"TrojanFlare","type":"url","url":"https://sub.trojanflare.one/api/v1/client/subscribe","updated":1763600000,"extra":{"upload":1073741824,"download":5368709120,"total":107374182400,"expire":1798761600},"allow_auto_update":true,"update_interval":43200},"#
    + #"{"current":false,"uid":"a1b2c3d4","name":"本地备份","type":"file","updated":1763500000,"extra":{"upload":0,"download":0,"total":0,"expire":0}}]"#

/// 删除/切换后重拉的权威态（单条 file 型订阅，与 fixture 首条 uid 不同可证重拉生效）
private let subsListAfterMutation =
    #"[{"current":false,"uid":"a1b2c3d4","name":"本地备份","type":"file","updated":1763500000,"extra":{"upload":0,"download":0,"total":0,"expire":0}}]"#

// MARK: - CoreBridge 订阅桥（calls + 65s writeTimeout 路径）

@Suite struct SubsBridgeTests {
    /// add 的 flags 必须前置（cmdAdd 实况核对）：Go flag 在首个非 flag 参数处
    /// 停止解析，`add <url> --name X` 会把 --name X 吞为位置参数致名称静默丢失
    @Test func subsAddRoutesExtendedTimeoutWithNameFlagsFirst() async throws {
        let fake = TimeoutRecordingRunner()
        try await CoreBridge(runner: fake).subsAdd(
            url: "https://example.com/sub?token=x", name: "云雾航线")
        #expect(fake.calls == [["add", "--name", "云雾航线", "https://example.com/sub?token=x"]])
        #expect(fake.timeouts == [65]) // 拉取 + 合成 + 热重载链走 65s writeTimeout
    }

    @Test func subsAddOmitsNameFlagWhenBlank() async throws {
        // 名称空白省略 --name：Go 默认取 URL host
        let fake = TimeoutRecordingRunner()
        try await CoreBridge(runner: fake).subsAdd(url: "https://example.com/sub", name: "")
        #expect(fake.calls == [["add", "https://example.com/sub"]])
    }

    @Test func subsUpdateAndUseRouteExtendedTimeout() async throws {
        let fake = TimeoutRecordingRunner()
        let bridge = CoreBridge(runner: fake)
        try await bridge.subsUpdate(target: "b89ed456")
        try await bridge.subsUse(target: "a1b2c3d4")
        #expect(fake.calls == [["update", "b89ed456"], ["use", "a1b2c3d4"]])
        #expect(fake.timeouts == [65, 65])
    }

    /// delete 的 --force 仅在删 current 时附加（Go current 保护对非 current
    /// 无需 force）；且 target 必须在 --force 之前（cmdSubs fs.Parse(args[2:]) 形态）
    @Test func subsDeleteAppendsForceOnlyWhenRequested() async throws {
        let fake = TimeoutRecordingRunner()
        let bridge = CoreBridge(runner: fake)
        try await bridge.subsDelete(target: "b89ed456", force: true)
        try await bridge.subsDelete(target: "a1b2c3d4", force: false)
        #expect(fake.calls == [["subs", "delete", "b89ed456", "--force"],
                               ["subs", "delete", "a1b2c3d4"]])
        #expect(fake.timeouts == [65, 65])
    }
}

// MARK: - list --json 全量卡解码（含 omitempty 缺键容忍）

@Suite struct SubsCardDecodeTests {
    @Test func bridgeSubsListJSONDecodesFullCard() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: subsListFixture + "\n", exitCode: 0))
        let entries = try await CoreBridge(runner: fake).subsListJSON()
        #expect(entries.count == 2)
        let current = try #require(entries.first)
        #expect(current == SubsCardEntry(
            current: true, uid: "b89ed456", name: "TrojanFlare", type: "url",
            url: "https://sub.trojanflare.one/api/v1/client/subscribe",
            updated: 1763600000,
            extra: SubsExtraTraffic(upload: 1073741824, download: 5368709120,
                                    total: 107374182400, expire: 1798761600),
            allowAutoUpdate: true, updateInterval: 43200))
        #expect(fake.calls == [["list", "--json"]]) // 读操作走常规 runner
    }

    /// c2 审查 P1 同款回归：off 档/新订阅输出缺 update_interval 键、
    /// allow_auto_update 指针 nil 亦缺键——非可选解码会 keyNotFound 被 try? 吞掉
    @Test func decodeToleratesOmittedOptionalKeys() async throws {
        let sparse = #"[{"current":false,"uid":"u1","name":"新订阅","type":"url","updated":1700000000,"extra":{"upload":0,"download":0,"total":0,"expire":0}}]"#
        let fake = FakeRunner(FakeRunner.Reply(output: sparse, exitCode: 0))
        let entries = try await CoreBridge(runner: fake).subsListJSON()
        #expect(entries.count == 1)
        #expect(entries[0].allowAutoUpdate == nil) // nil=未设置（默认允许）
        #expect(entries[0].updateInterval == nil) // omitempty 缺键
        #expect(entries[0].extra.expire == 0) // 0=到期未知（展示层落"未知"）
    }

    @Test func decodeToleratesEmptyTable() async throws {
        let fake = FakeRunner(FakeRunner.Reply(output: "[]\n", exitCode: 0))
        let entries = try await CoreBridge(runner: fake).subsListJSON()
        #expect(entries.isEmpty)
    }
}

// MARK: - 订阅卡纯函数（流量条阈值/格式化/日期）

@Suite struct SubsPaneLogicTests {
    @Test func usedBytesSumsAndGuardsNegative() {
        #expect(SubsPaneLogic.usedBytes(upload: 100, download: 900) == 1_000)
        #expect(SubsPaneLogic.usedBytes(upload: -50, download: 20) == 0) // 异常头负值防御
    }

    @Test func trafficPercentNilForUnknownTotalAndCapsAt100() {
        #expect(SubsPaneLogic.trafficPercent(used: 0, total: 0) == nil) // 不限量/未知
        #expect(SubsPaneLogic.trafficPercent(used: 100, total: -1) == nil)
        #expect(SubsPaneLogic.trafficPercent(used: 50, total: 100) == 50)
        #expect(SubsPaneLogic.trafficPercent(used: 200, total: 100) == 100) // 封顶
    }

    /// demo `pct>85?'warn':''`：严格大于 85 才警示；nil（不限量）不警示
    @Test func trafficWarnThresholdIsStrictAbove85() {
        #expect(!SubsPaneLogic.isTrafficWarn(nil))
        #expect(!SubsPaneLogic.isTrafficWarn(85)) // 85.0 不警示
        #expect(SubsPaneLogic.isTrafficWarn(85.1))
        #expect(SubsPaneLogic.isTrafficWarn(100))
    }

    @Test func trafficBytesFormattingCoversUnitLadder() {
        #expect(SubsPaneLogic.trafficBytes(0) == "0 B")
        #expect(SubsPaneLogic.trafficBytes(-5) == "0 B")
        #expect(SubsPaneLogic.trafficBytes(999) == "999 B")
        #expect(SubsPaneLogic.trafficBytes(1_500_000) == "1.5 MB")
        #expect(SubsPaneLogic.trafficBytes(1_073_741_824) == "1.1 GB")
        #expect(SubsPaneLogic.trafficBytes(1_099_511_627_776) == "1.1 TB") // TB 档（RateText 无）
    }

    @Test func percentTextUnknownAndPrecision() {
        #expect(SubsPaneLogic.percentText(nil) == "—")
        #expect(SubsPaneLogic.percentText(50) == "50.0%")
        #expect(SubsPaneLogic.percentText(97.34) == "97.3%")
    }

    /// 到期 0/负=未知（demo expire '未知' 口径）；formatter 注入锚定 UTC 断言
    @Test func dateTextNilForNonPositiveUnix() {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyy-MM-dd HH:mm"
        formatter.timeZone = TimeZone(identifier: "UTC")
        formatter.locale = Locale(identifier: "en_US_POSIX")
        #expect(SubsPaneLogic.dateText(0, formatter: formatter) == nil)
        #expect(SubsPaneLogic.dateText(-5, formatter: formatter) == nil)
        #expect(SubsPaneLogic.dateText(1_798_761_600, formatter: formatter) == "2027-01-01 00:00")
    }
}

// MARK: - SubsModel 编排（load/add/delete current 重拉/busy 防抖/updateAll 过滤）

@MainActor
@Suite struct SubsModelTests {
    /// 装载完成的模型 + 记录型 fake（load 消耗首条 list 回复）
    private func loadedModel(_ replies: [FakeRunner.Reply]) async -> (SubsModel, FakeRunner) {
        let fake = FakeRunner(replies)
        let model = SubsModel(bridge: CoreBridge(runner: fake))
        await model.load()
        return (model, fake)
    }

    @Test func loadDecodesListEntries() async {
        let (model, fake) = await loadedModel([.init(output: subsListFixture, exitCode: 0)])
        #expect(model.subs.count == 2)
        #expect(model.subs[0].name == "TrojanFlare" && model.subs[0].current)
        #expect(model.subs[1].type == "file" && model.subs[1].updateInterval == nil)
        #expect(!model.loadFailed)
        #expect(fake.calls == [["list", "--json"]])
    }

    @Test func loadFailureSetsLoadFailedAndToast() async {
        let (model, _) = await loadedModel([.init(output: "not json", exitCode: 0)])
        #expect(model.loadFailed)
        #expect(model.toast != nil)
        #expect(model.subs.isEmpty)
    }

    /// URL 空校验（壳侧前置，不落 CLI）
    @Test func addRejectsBlankURLWithoutCLICall() async {
        let (model, fake) = await loadedModel([.init(output: subsListFixture, exitCode: 0)])
        let ok = await model.add(url: "   ", name: "任意")
        #expect(!ok)
        #expect(fake.calls == [["list", "--json"]]) // 仅 load 一次
        #expect(model.toast?.contains("订阅链接") == true)
    }

    @Test func addTrimsInputImportsAndReloads() async {
        let (model, fake) = await loadedModel([
            .init(output: subsListFixture, exitCode: 0), // load
            .init(output: "", exitCode: 0), // add
            .init(output: subsListAfterMutation, exitCode: 0), // 重拉
        ])
        let ok = await model.add(url: " https://example.com/sub?token=x ", name: " 云雾航线 ")
        #expect(ok)
        #expect(fake.calls == [["list", "--json"],
                               ["add", "--name", "云雾航线", "https://example.com/sub?token=x"],
                               ["list", "--json"]]) // 模型 trim 后再落 CLI
        #expect(model.subs.map(\.uid) == ["a1b2c3d4"]) // 重拉后的权威态
        #expect(model.toast == "订阅已导入")
    }

    /// 删 current：UI 二次确认强制语义——直接传 --force，成功后重拉权威态 +
    /// 裸内核态提示（对齐 Go CLI 输出文案，M3-plan §11.2-5）
    @Test func deleteCurrentSendsForceReloadsAndWarnsBareCore() async {
        let entry = SubsCardEntry(
            current: true, uid: "b89ed456", name: "TrojanFlare", type: "url",
            url: "https://sub.trojanflare.one/api/v1/client/subscribe", updated: 1,
            extra: SubsExtraTraffic(upload: 0, download: 0, total: 0, expire: 0),
            allowAutoUpdate: nil, updateInterval: nil)
        let (model, fake) = await loadedModel([
            .init(output: subsListFixture, exitCode: 0), // load
            .init(output: "", exitCode: 0), // subs delete --force
            .init(output: subsListAfterMutation, exitCode: 0), // 重拉
        ])
        await model.delete(entry)
        #expect(fake.calls[1] == ["subs", "delete", "b89ed456", "--force"])
        #expect(fake.calls[2] == ["list", "--json"]) // 删除后重拉 list
        #expect(model.subs.map(\.uid) == ["a1b2c3d4"])
        #expect(model.toast?.contains("裸内核态") == true)
        #expect(model.toast?.contains("请导入或 use 订阅") == true)
    }

    /// 删非 current：不带 --force（Go current 保护语义保持完整），普通提示
    @Test func deleteNonCurrentOmitsForce() async {
        let entry = SubsCardEntry(
            current: false, uid: "a1b2c3d4", name: "本地备份", type: "file",
            url: nil, updated: 1,
            extra: SubsExtraTraffic(upload: 0, download: 0, total: 0, expire: 0),
            allowAutoUpdate: nil, updateInterval: nil)
        let (model, fake) = await loadedModel([
            .init(output: subsListFixture, exitCode: 0),
            .init(output: "", exitCode: 0),
            .init(output: subsListAfterMutation, exitCode: 0),
        ])
        await model.delete(entry)
        #expect(fake.calls[1] == ["subs", "delete", "a1b2c3d4"])
        #expect(model.toast == "订阅已删除（本地备份）")
    }

    /// busy 防抖（SettingsModel 同款）：在途时再次提交被拒且不落第二次 CLI 调用
    @Test func busyGuardsConcurrentWrite() async {
        let (model, fake) = await loadedModel([.init(output: subsListFixture, exitCode: 0)])
        model.testHooksSetBusy(true)
        let entry = model.subs[0]
        await model.update(entry)
        await model.delete(entry)
        #expect(fake.calls == [["list", "--json"]]) // 无任何写调用
        model.testHooksSetBusy(false)
    }

    /// 全部更新仅触 remote（type=url）订阅；file 型无远端可拉跳过；整组一个
    /// busy 周期结尾统一重拉 + 汇总 toast
    @Test func updateAllTouchesOnlyRemoteEntries() async {
        let (model, fake) = await loadedModel([
            .init(output: subsListFixture, exitCode: 0), // load（两条：url+file）
            .init(output: "", exitCode: 0), // update b89ed456
            .init(output: subsListAfterMutation, exitCode: 0), // 重拉
        ])
        await model.updateAll()
        let writes = fake.calls.filter { $0.first == "update" }
        #expect(writes == [["update", "b89ed456"]]) // file 型 a1b2c3d4 不更新
        #expect(fake.calls.last == ["list", "--json"])
        #expect(model.toast == "全部订阅已更新")
    }
}
