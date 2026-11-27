import AppKit
import Foundation
import Testing
@testable import TradewindKit

// MARK: - §3.9 #7 启动分支（早返回三守卫；断言面=状态项未装配 + lazy model 零构建）

@MainActor @Suite struct InstallerLaunchBranchTests {
    /// 检测命中：didFinishLaunching 首语句分支早返回——状态项/observeModel 等
    /// 余下装配全部不执行（lazy model 全程零构建是「静默启动异步块不执行」的
    /// 行为证据：该块若执行必经 openSettings 触发模型构建）。
    @Test func didFinishLaunchingInInstallerModeReturnsBeforeShellAssembly() {
        let delegate = AppDelegate()
        delegate.installerDetection = { true }
        delegate.applicationDidFinishLaunching(
            Notification(name: NSApplication.didFinishLaunchingNotification))
        #expect(delegate.installerMode)
        #expect(delegate.installerWindowShown) // 安装器窗已前置
        #expect(delegate.statusItem == nil)    // 状态项未装配（早返回证据）
        #expect(delegate.modelBuildCount == 0) // lazy model 全程未构建
    }

    /// 检测未命中：正常壳装配分支（不经 didFinishLaunching 全量，直测分支返回值）。
    @Test func detectionMissReturnsFalseWithoutInstallerAssembly() {
        let delegate = AppDelegate()
        delegate.installerDetection = { false }
        #expect(!delegate.launchInstallerIfNeeded())
        #expect(!delegate.installerMode)
        #expect(!delegate.installerWindowShown)
        #expect(delegate.modelBuildCount == 0)
    }

    /// reopen 守卫：前置安装器窗、绝不 openSettings（开设置窗必触 lazy model 构建）。
    @Test func reopenInInstallerModeFrontsInstallerWithoutSettings() {
        let delegate = AppDelegate()
        delegate.installerMode = true
        let handled = delegate.applicationShouldHandleReopen(NSApplication.shared, hasVisibleWindows: true)
        #expect(!handled) // 返回 false 声明已处理
        #expect(delegate.installerWindowShown)
        #expect(delegate.modelBuildCount == 0)
    }

    /// terminate 守卫：安装器退出不触碰 lazy model（不建面板栈）。
    @Test func terminateInInstallerModeDoesNotTouchLazyModel() {
        let delegate = AppDelegate()
        delegate.installerMode = true
        delegate.applicationWillTerminate(
            Notification(name: NSApplication.willTerminateNotification))
        #expect(delegate.modelBuildCount == 0)
    }

    /// 对照组：壳模式 terminate 照旧构建模型并 persistNow（defaults 注入 nil，
    /// 落盘 no-op——不触真实偏好域）。
    @Test func terminateInShellModeStillBuildsModelAndPersists() {
        let delegate = AppDelegate()
        delegate.modelBuilder = { PanelModel(bridge: nil, traffic: TrafficStore(defaults: nil)) }
        delegate.applicationWillTerminate(
            Notification(name: NSApplication.willTerminateNotification))
        #expect(delegate.modelBuildCount == 1)
    }
}
