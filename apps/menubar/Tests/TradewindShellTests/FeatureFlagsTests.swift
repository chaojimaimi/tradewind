import SwiftUI
import Testing
@testable import TradewindKit

/// 功能开关的单一事实源（M3 batch6 release-gate）：v0.5.0-m3f1 以"特权服务保持关闭"
/// 发版——R16 提权边界闭环前，特权 TUN 的用户入口（popover 开关行、设置页 TUN 行）
/// 不得出现；逃生口与模型/桥接层保留，加固后置 true 即重新启用。
@Suite(.serialized) @MainActor struct FeatureFlagsTests {
    // MARK: - 开关本身

    /// 发版闸：开关默认关闭。置 true 之前须闭环 R16。
    @Test func privilegedTUNStaysOffUntilR16IsClosed() {
        #expect(!FeatureFlags.privilegedTUN,
                "置 true 之前须闭环 R16（docs/plans/batch6-plan.md §9 第六轮）：root 内核以用户可写目录为工作目录的提权边界")
    }

    // MARK: - popover 开关组：行可见性（纯函数）

    @Test func toggleGroupOmitsTUNRowWhenOff() {
        let rows = ToggleGroupLayout.rows(includeTUN: false)
        #expect(rows == [.systemProxy, .guardPlaceholder])
        #expect(!rows.contains(.tun))
    }

    @Test func toggleGroupIncludesTUNRowWhenOn() {
        let rows = ToggleGroupLayout.rows(includeTUN: true)
        #expect(rows == [.systemProxy, .tun, .guardPlaceholder])
        #expect(rows.contains(.tun))
    }

    /// 隐藏后的纵向结构回到"系统代理 + 分隔 + 代理守护"：系统代理行保留，分隔恰在
    /// 两行之间一条——首行之前、末行之后都没有（不留空位或多余分隔）。
    @Test func toggleGroupHiddenShapeIsSystemProxyDividerGuard() {
        #expect(ToggleGroupLayout.items(includeTUN: false)
                == [.row(.systemProxy), .divider, .row(.guardPlaceholder)])
        #expect(ToggleGroupLayout.items(includeTUN: true)
                == [.row(.systemProxy), .divider, .row(.tun), .divider, .row(.guardPlaceholder)])
    }

    // MARK: - 设置窗口「网络代理」区：行可见性（纯函数）

    @Test func proxySectionOmitsTUNRowWhenOff() {
        let rows = ProxySectionLayout.rows(includeTUN: false)
        #expect(rows == [.pac, .bypass])
        #expect(!rows.contains(.tun))
    }

    @Test func proxySectionIncludesTUNRowWhenOn() {
        let rows = ProxySectionLayout.rows(includeTUN: true)
        #expect(rows == [.tun, .pac, .bypass])
        #expect(rows.contains(.tun))
    }

    @Test func proxySectionHiddenShapeIsPacDividerBypass() {
        #expect(ProxySectionLayout.items(includeTUN: false) == [.row(.pac), .divider, .row(.bypass)])
        #expect(ProxySectionLayout.items(includeTUN: true)
                == [.row(.tun), .divider, .row(.pac), .divider, .row(.bypass)])
    }

    // MARK: - 分隔装配的不变量

    /// 分隔恒为"行数 − 1"条，只出现在相邻两行之间；空列表与单行都没有分隔。
    @Test func dividersOnlyBetweenAdjacentRows() {
        #expect(RowListLayout.items([Int]()) == [])
        #expect(RowListLayout.items([7]) == [.row(7)])
        #expect(RowListLayout.items([1, 2, 3]) == [.row(1), .divider, .row(2), .divider, .row(3)])
        for count in 0...6 {
            let items = RowListLayout.items(Array(0..<count))
            #expect(items.filter { $0 == .divider }.count == max(count - 1, 0))
            #expect(items.first != .divider && items.last != .divider)
        }
    }

    // MARK: - 生产装配走的就是这个开关

    /// 两个视图的生产默认值都取功能开关；按开关取到的行列表里，TUN 行的有无与开关
    /// 一致（断言与开关取值无关——开关置 true 后仍然成立）。
    @Test func productionViewsDefaultToTheFlag() {
        let flag = FeatureFlags.privilegedTUN
        #expect(ToggleGroupCard(model: PanelModel(bridge: nil)).includeTUN == flag)
        #expect(ProxySectionContent(proxyModeModel: ProxyModeModel(bridge: nil)).includeTUN == flag)
        #expect(ToggleGroupLayout.rows(includeTUN: flag).contains(.tun) == flag)
        #expect(ProxySectionLayout.rows(includeTUN: flag).contains(.tun) == flag)
    }

    // MARK: - 渲染层结构对比（无法目验时的替代证据）

    /// 真实卡片的布局高度必须等于"TUN 行不参与"与"TUN 行参与"二者中与开关一致的
    /// 那一个——即开关确实决定了 TUN 行是否被渲染，而不只是行列表里有没有它。
    @Test func toggleGroupCardRendersAccordingToTheFlag() throws {
        let model = PanelModel(bridge: nil)
        let hidden = try fittedHeight(of: ToggleGroupCard(model: model, includeTUN: false))
        let shown = try fittedHeight(of: ToggleGroupCard(model: model, includeTUN: true))
        let production = try fittedHeight(of: ToggleGroupCard(model: model))

        #expect(hidden != shown, "两种形态必须不等高——相等说明高度对比失效，本用例无意义")
        #expect(production == (FeatureFlags.privilegedTUN ? shown : hidden))
        // TUN 行参与时更高（行在最上面加了一行，而不是把别的行挤掉）
        #expect(shown > hidden)
    }

    @Test func proxySectionRendersAccordingToTheFlag() throws {
        let section = { (includeTUN: Bool) in
            SettingsSection(title: "网络代理") {
                ProxySectionContent(proxyModeModel: ProxyModeModel(bridge: nil), includeTUN: includeTUN)
            }
        }
        let hidden = try fittedHeight(of: section(false))
        let shown = try fittedHeight(of: section(true))
        let production = try fittedHeight(of: section(FeatureFlags.privilegedTUN))

        #expect(hidden != shown, "两种形态必须不等高——相等说明高度对比失效，本用例无意义")
        #expect(production == (FeatureFlags.privilegedTUN ? shown : hidden))
        #expect(shown > hidden)
    }

    /// 设置页整体在开关关闭时仍能完成布局（「网络代理」区经 ProxySectionContent
    /// 装配后并入 GeneralPane，不产生零尺寸或不崩溃）。
    @Test func generalPaneLaysOutWithTheToggleHidden() async throws {
        let model = SettingsModel(bridge: CoreBridge(runner: SettingsSnapshotRunner()))
        await model.load()
        try #require(model.settings != nil) // 「网络代理」区只在快照就绪后渲染

        let height = try fittedHeight(of: GeneralPane(model: model, subsModel: SubsModel(bridge: nil),
                                                      proxyModeModel: ProxyModeModel(bridge: nil)))
        #expect(height > 0)
    }

    // MARK: - intake 不受开关约束

    /// 开关只隐藏用户入口，不改"本机是否实际在走 TUN"的呈现：经 CLI 显式确认开启的
    /// 会话，status --json 报 tun.enabled=true → 菜单栏图标仍显示有接入。
    @Test func intakeStillCountsTUNReportedByTheCLI() throws {
        let report = try StatusReport.decode(#"{"core":{"running":true,"pid":1,"version":"v"},"mode":"rule","sys_proxy":{"applied":false,"services":0},"tun":{"enabled":true,"running":true},"profile":{"uid":"","name":""},"panel_url":""}"#)
        let model = PanelModel(bridge: nil)
        model.report = report
        #expect(model.effectiveTUN)
        #expect(model.intake, "TUN 会话在跑时图标必须显示有接入，无论功能开关取值")
        #expect(model.iconState == .active)
    }

    // MARK: - 辅助

    /// 把视图挂进 NSHostingView 取布局高度（离屏；同一视图树确定性布局）。
    private func fittedHeight<V: View>(of view: V) throws -> CGFloat {
        let host = NSHostingView(rootView: view)
        let height = host.fittingSize.height
        try #require(height > 0, "视图必须产生正的布局高度（0 说明整块未渲染）")
        return height
    }
}

/// 设置快照读取桩：按子命令回放合法 JSON（settings / autostart / list / status）。
/// 绝不拉真实 CLI 进程。
private final class SettingsSnapshotRunner: CommandLineRunning, @unchecked Sendable {
    func run(_ args: [String], tolerate: Set<Int32>) async throws -> String {
        switch args.first {
        case "settings":
            return #"{"mixed_port":7897,"gateway_port":39097,"panel_enabled":true,"allow_lan":false,"mode":"rule","log_level":"info","log_retention_days":14}"#
        case "autostart":
            return #"{"installed":false,"loaded":false}"#
        case "list":
            return "[]"
        case "status":
            return #"{"core":{"running":false,"pid":0,"version":""},"mode":"rule","sys_proxy":{"applied":false,"services":0},"profile":{"uid":"","name":""},"panel_url":""}"#
        default:
            return ""
        }
    }
}
