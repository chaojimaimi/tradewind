import Foundation
import Testing
@testable import TradewindKit

/// Sparkle 集成装配面（batch5 §3.2/§5）：Info.plist 两键锁点 + UpdateService 装配 +
/// AboutPane 按钮接线 + 删除集零死代码审计。
/// WHY 锁点在生成脚本而非运行时 Info.plist：单测环境无 app bundle，键由 build.sh
/// heredoc 注入——锁生成面即锁产物面。Sparkle 网络/UI 行为不进单测（F11）。
@Suite struct SparkleIntegrationTests {
    /// 仓库根（测试文件位于 <root>/apps/menubar/Tests/TradewindShellTests/）
    private static let repoRoot = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent() // Tests/TradewindShellTests
        .deletingLastPathComponent() // Tests
        .deletingLastPathComponent() // apps/menubar
        .deletingLastPathComponent() // apps
        .deletingLastPathComponent() // repo root

    // MARK: Info.plist 两键锁点（batch5 §3.1 build.sh 注入面）

    @Test func buildScriptInjectsSparkleInfoPlistKeys() throws {
        let buildSh = try String(contentsOf: Self.repoRoot
            .appendingPathComponent("apps/menubar/build.sh"), encoding: .utf8)
        // SUFeedURL：F7 稳定 feed URL（latest/download，公开仓匿名可达）
        #expect(buildSh.contains("<key>SUFeedURL</key>"))
        #expect(buildSh.contains(
            "https://github.com/chaojimaimi/tradewind/releases/latest/download/appcast.xml"))
        // SUPublicEDKey：从 sparkle-eddsa-pub.txt 注入（缺失→警告跳过，构建不失败）
        #expect(buildSh.contains("<key>SUPublicEDKey</key>"))
        #expect(buildSh.contains("sparkle-eddsa-pub.txt"))
    }

    // MARK: UpdateService 装配（startsUpdater=false：不触网、不启动自动检查，F11）

    @MainActor
    @Test func updateServiceAssemblesWithoutStarting() {
        let service = UpdateService(startsUpdater: false)
        // 未启动 → canCheckForUpdates 恒 false（按钮禁用降级面）
        #expect(!service.canCheckForUpdates)
        // 未就绪时 checkForUpdates no-op：无崩溃、无 Sparkle 弹窗（装配面防御）
        service.checkForUpdates()
    }

    @MainActor
    @Test func aboutPaneAcceptsInjectedUpdateService() {
        // 按钮接线锁点：AboutPane 接受注入的 UpdateService（构造不崩）；
        // 生产路径走默认 init 自管装配（updater 启动），视觉接线由目验清单覆盖
        //（batch5 §7-3：点按走 Sparkle 标准窗）。
        let pane = AboutPane(model: SettingsModel(bridge: nil),
                             updateService: UpdateService(startsUpdater: false))
        _ = pane // 构造成功即装配面成立
    }

    // MARK: 删除集零死代码审计（终审 P2-1：UpdateModel/CLI 三态区块退役）

    @Test func retiredUpdateModelLeavesNoDeadCode() throws {
        let fm = FileManager.default
        let menubar = Self.repoRoot.appendingPathComponent("apps/menubar")
        // 删除集文件不存在
        #expect(!fm.fileExists(atPath: menubar
            .appendingPathComponent("Sources/TradewindKit/UpdateModel.swift").path))
        #expect(!fm.fileExists(atPath: menubar
            .appendingPathComponent("Tests/TradewindShellTests/UpdateModelTests.swift").path))
        // CoreBridge 两函数与专用超时档删除（漏删即编译断链或无消费面死代码）
        let coreBridge = try String(contentsOf: menubar
            .appendingPathComponent("Sources/TradewindKit/CoreBridge.swift"), encoding: .utf8)
        for token in ["versionCheckJSON", "selfUpdate", "updateTimeout"] {
            #expect(!coreBridge.contains(token), "CoreBridge retains \(token)")
        }
        // 壳源码零残留（UpdateCheckReport/SelfUpdateReport 定义在 UpdateModel.swift
        // 内，类型与消费面须一并退役）
        let enumerator = fm.enumerator(at: menubar.appendingPathComponent("Sources"),
                                       includingPropertiesForKeys: nil)
        while let file = enumerator?.nextObject() as? URL {
            guard file.pathExtension == "swift" else { continue }
            let content = try String(contentsOf: file, encoding: .utf8)
            for token in ["UpdateModel", "UpdateCheckReport", "SelfUpdateReport",
                          "UpdateCheckState"] {
                #expect(!content.contains(token),
                        "\(file.lastPathComponent) retains \(token)")
            }
        }
    }
}
