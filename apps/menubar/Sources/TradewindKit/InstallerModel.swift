// 批⑦ c1（docs/plans/batch7-plan.md §3.4/§3.5）：DMG 安装器七步状态机。
// @MainActor ObservableObject，纯逻辑——文件系统/子进程/验签/open 全走注入缝。
// 每步幂等可重试：失败重试从步 1 重跑；凡 DMG 落位二进制一律清 com.apple.quarantine
// （评审 P0：未签名二进制带 quarantine 首 exec 被 SIGKILL）。进度只到步粒度，
// 不展示用户路径（/Applications/Tradewind.app 公开定值豁免）。
// 执行模型（评审必修 2）：相位更新恒在主 actor，步体（FS/Process 阻塞操作）
// 派发后台任务——每个相位先渲染再跑；PATH 授权弹窗等待期主线程空闲可交互。
import AppKit
import Foundation

/// 七步安装状态机（§3.4）：目录 → 内核 → ui → CLI → app → PATH 授权（可选）→ 收尾。
@MainActor
final class InstallerModel: ObservableObject {
    // MARK: 步定义与相位

    enum Step: Int, CaseIterable {
        case directories = 1, kernel, ui, cli, app, pathAuth, finish

        var title: String {
            switch self {
            case .directories: return "准备数据目录"
            case .kernel: return "安装内核"
            case .ui: return "安装网页面板资源"
            case .cli: return "安装命令行工具"
            case .app: return "安装应用本体"
            case .pathAuth: return "授权终端命令链接"
            case .finish: return "完成"
            }
        }
    }

    enum Phase: Equatable {
        case idle
        case running(Step)
        /// 步 5：目标 app 在跑 → 界面给「退出并继续」按钮（点击即授权语义）
        case needsAppQuit
        /// 步号+原因 → 界面给「重试」（从步 1 重跑，全幂等）；PATH 步另有「跳过此步」
        case failed(Step, String)
        /// nil=干净完成；注记=步 7 open 失败的手动打开提示（公开定值路径豁免）
        case finished(String?)
    }

    // MARK: 可观察状态

    @Published private(set) var phase: Phase = .idle
    /// PATH 授权项（可选步，默认不勾选，§3.5）
    @Published var installPATH = false
    /// 用户取消授权 / 主动跳过：本步按「已跳过」计，完成态如实
    @Published private(set) var pathAuthSkipped = false

    // MARK: 注入缝（生产默认真实现；单测全替换，绝不触真实系统）
    // 缝类型全部 @Sendable：步体派发到后台任务执行（评审必修 2）的前提。

    let fs: InstallerFS
    let runner: InstallerRunner
    /// DMG 里的 app bundle（源四件锚点；生产 = Bundle.main.bundlePath）
    let bundlePath: String
    /// 数据目录（environment 注入可测；测试隔离全走 TRADEDWIND_HOME）
    let home: String
    var ownVersion: @Sendable () -> String?
    var verifySignature: @Sendable (String) -> Error?
    var appIsRunning: @Sendable () -> Bool
    var terminateRunningApp: @Sendable () -> Void
    var openInstalledApp: @Sendable () -> Bool
    /// 完成后自身退出（App 注入 NSApp.terminate；安装器不留常驻）
    var terminateSelf: @Sendable () -> Void

    /// 步 7 open 失败时的手动打开提示（公开定值路径，§3.4 进度纪律显式豁免）。
    static let manualOpenNote = "无法自动打开，请手动打开 \(InstallerSupport.installedAppPath)"

    init(bundlePath: String,
         environment: [String: String] = ProcessInfo.processInfo.environment,
         fs: InstallerFS = FileManagerInstallerFS(),
         runner: InstallerRunner = ProcessInstallerRunner()) {
        self.bundlePath = bundlePath
        self.fs = fs
        self.runner = runner
        self.home = InstallerSupport.homeDir(environment: environment)
        self.ownVersion = {
            Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        }
        self.verifySignature = { InstallerSignature.validate($0) }
        self.appIsRunning = {
            let pids = NSRunningApplication.runningApplications(withBundleIdentifier: Self.appBundleIdentifier)
                .map(\.processIdentifier)
            return !Self.foreignRunningPids(pids, currentPid: ProcessInfo.processInfo.processIdentifier).isEmpty
        }
        self.terminateRunningApp = {
            // 唯一调用点 confirmQuitAndContinue 在主 actor——assumeIsolated 收口
            // AppKit 的 MainActor 隔离面（@Sendable 缝闭包的静态可判安全性）
            MainActor.assumeIsolated {
                let currentPid = ProcessInfo.processInfo.processIdentifier
                let apps = NSRunningApplication.runningApplications(withBundleIdentifier: Self.appBundleIdentifier)
                let foreignPids = Set(Self.foreignRunningPids(apps.map(\.processIdentifier), currentPid: currentPid))
                for app in apps where foreignPids.contains(app.processIdentifier) {
                    app.terminate()
                }
            }
        }
        self.openInstalledApp = {
            NSWorkspace.shared.open(URL(fileURLWithPath: InstallerSupport.installedAppPath))
        }
        // 唯一调用点 finalize 的 main-queue asyncAfter——同款主线程保证
        self.terminateSelf = { MainActor.assumeIsolated { NSApp.terminate(nil) } }
    }

    /// 双实例并存防御：检测/退出都按本 bundle id 定位。
    /// nonisolated：@Sendable 生产闭包（appIsRunning/terminateRunningApp）与后台
    /// 步体均可直接读——不可变 Sendable 常量，无隔离需求。
    nonisolated static let appBundleIdentifier = "io.github.chaojimaimi.tradewind"

    /// 步 5 目标实例定位谓词（纯函数可测）：同 bundle id 的进程列表里**排除当前
    /// 进程**——安装器自己就是同 bundle id 的 Tradewind.app，不过滤会让全新机
    /// （目标不存在）步 5 恒 needsAppQuit 误报，且「退出并继续」会 terminate 到
    /// 安装器自身。顺序保持，供 appIsRunning/terminateRunningApp 两处共用。
    nonisolated static func foreignRunningPids(_ pids: [Int32], currentPid: Int32) -> [Int32] {
        pids.filter { $0 != currentPid }
    }

    // MARK: 驱动入口（§3.4：失败重试/授权后继续均从步 1 重跑，全幂等）
    // 相位更新恒在主 actor（状态一致性），步体派发后台任务（评审必修 2）——
    // 每个相位先渲染再跑，PATH 授权弹窗等待期主线程空闲（窗口可移动/关闭）。

    private var currentStep: Step = .directories
    /// 用户点「退出并继续」即授权语义：后续步 5 不再因运行中实例而中止
    /// （NSRunningApplication.terminate 是异步的，重入时实例可能仍在列表里）。
    private var userAuthorizedQuit = false
    /// 单飞防御：running 期「重试/退出并继续」二次点击不并发重入步序。
    private var runInFlight = false

    func startInstall() async {
        userAuthorizedQuit = false
        await runAgain()
    }

    /// 失败重试入口。
    func retry() async {
        await runAgain()
    }

    /// 用户点击「退出并继续」：终止目标实例后重跑（点击即授权语义，批⑦ §7 红线）。
    func confirmQuitAndContinue() async {
        terminateRunningApp()
        userAuthorizedQuit = true
        await runAgain()
    }

    /// PATH 授权失败后的「跳过此步」：本步按跳过计，收尾后完成态如实。
    func skipPathAuth() async {
        guard case .failed(.pathAuth, _) = phase else { return }
        pathAuthSkipped = true
        installPATH = false
        await finalize()
    }

    private func runAgain() async {
        guard !runInFlight else { return }
        runInFlight = true
        defer { runInFlight = false }
        do {
            try await runSteps()
        } catch InstallerError.needsAppQuit {
            phase = .needsAppQuit
        } catch let error as InstallerError {
            if case .stepFailure(let step, let reason) = error {
                phase = .failed(step, reason)
            }
        } catch {
            phase = .failed(currentStep, "意外错误：\(error.localizedDescription)")
        }
    }

    private func runSteps() async throws {
        try await advance(.directories) { try self.stepDirectories() }
        try await advance(.kernel) { try self.stepKernel() }
        try await advance(.ui) { try self.stepUI() }
        try await advance(.cli) { try self.stepCLI() }
        try await runAppStep()
        if installPATH {
            let userCanceled = try await advance(.pathAuth) { try self.stepPathAuth() }
            if userCanceled { pathAuthSkipped = true } // 取消授权=该步跳过（主 actor 落状态）
        }
        await finalize()
    }

    /// 相位驱动：主 actor 先落相位（running 态获得渲染机会），随后派发步体到
    /// 后台任务（FS/Process 阻塞操作不占主线程），await 回主 actor 进下一步。
    /// Result 透传步体结论（步 6 的「用户取消=跳过」信号在主 actor 落状态）。
    private func advance<Result>(_ step: Step,
                                 body: @escaping @Sendable () throws -> Result) async throws -> Result {
        currentStep = step
        phase = .running(step)
        return try await Task.detached(priority: .userInitiated) { try body() }.value
    }

    /// 收尾（步 7）：open 已装副本；open 失败=完成态如实标注手动打开提示；
    /// 干净完成时短暂展示后自身退出（安装器不留常驻，§3.2/§3.4）。
    private func finalize() async {
        currentStep = .finish
        phase = .running(.finish)
        let open = openInstalledApp
        let opened = await Task.detached(priority: .userInitiated) { open() }.value
        let note = opened ? nil : Self.manualOpenNote
        phase = .finished(note)
        if note == nil {
            DispatchQueue.main.asyncAfter(deadline: .now() + 1.0) { [weak self] in
                self?.terminateSelf()
            }
        }
    }

    // MARK: 步体（nonisolated：只读不可变 Sendable 缝 fs/runner/home/bundlePath，
    // 由 advance 派发后台执行；可变状态只在 @MainActor 驱动层触碰）

    // 步 1——目录（0700 对齐 paths.go Ensure 权限语义）
    private nonisolated func stepDirectories() throws {
        try fs.createDirectory(home, intermediate: true, posixMode: 0o700)
        try fs.createDirectory(home + "/bin", intermediate: true, posixMode: 0o700)
    }

    // 步 2——内核（跳过判据=「成对且校验通过」，绝不降级健康内核）

    private nonisolated func stepKernel() throws {
        let root = InstallerSupport.sourceRoot(bundlePath: bundlePath)
        let targetKernel = home + "/bin/mihomo"
        let targetSidecar = home + "/bin/mihomo.sha256"
        // 健康内核含 self-update --core 升级过的版本——跳过即「只填缺不降级」
        if kernelPairHealthy(kernel: targetKernel, sidecar: targetSidecar) { return }
        // 损坏态（缺任一/校验不过）允许降级修复：清旧两件 → 双临时名拷入 →
        // chmod 755 + 清 quarantine → rename 序=sidecar 先、binary 后（两 rename
        // 之间的中间态=无 binary + 新 sidecar，start/open 内核 fail-closed 安全方向）
        try removeIfExists(targetKernel)
        try removeIfExists(targetSidecar)
        let incomingKernel = home + "/bin/.mihomo.incoming"
        let incomingSidecar = home + "/bin/.mihomo.sha256.incoming"
        try fs.copyItem(at: root + "/bin/mihomo", to: incomingKernel)
        try fs.copyItem(at: root + "/bin/mihomo.sha256", to: incomingSidecar)
        try fs.setPosixMode(0o755, at: incomingKernel)
        clearQuarantine(incomingKernel)
        try fs.moveItem(at: incomingSidecar, to: targetSidecar)
        try fs.moveItem(at: incomingKernel, to: targetKernel)
    }

    /// 「成对且校验通过」：两文件都在 且 SHA256(binary) == sidecar 首段 hex
    /// （复刻 VerifyBinary 语义，现算不信任旧值；任一读取失败=不健康）。
    private nonisolated func kernelPairHealthy(kernel: String, sidecar: String) -> Bool {
        guard fs.fileExists(kernel), fs.fileExists(sidecar) else { return false }
        guard let sidecarData = try? fs.contents(sidecar),
              let expected = InstallerSupport.sidecarExpectedHex(sidecarData),
              let actual = try? fs.sha256Hex(kernel) else { return false }
        return actual == expected
    }

    // 步 3——ui（静态资源无版本：removeItem 旧 + copyItem 新，重跑=刷新）

    private nonisolated func stepUI() throws {
        let root = InstallerSupport.sourceRoot(bundlePath: bundlePath)
        let target = home + "/ui"
        try removeIfExists(target)
        try fs.copyItem(at: root + "/ui", to: target)
    }

    // 步 4——CLI（临时名 → chmod 755 → 清 quarantine → sha 双侧比对）

    private nonisolated func stepCLI() throws {
        let root = InstallerSupport.sourceRoot(bundlePath: bundlePath)
        let target = home + "/tradewind"
        let incoming = home + "/.tradewind.incoming"
        try fs.copyItem(at: root + "/tradewind", to: incoming)
        try fs.setPosixMode(0o755, at: incoming)
        clearQuarantine(incoming)
        if fs.fileExists(target), (try? fs.sha256Hex(target)) == (try? fs.sha256Hex(incoming)) {
            try fs.removeItem(at: incoming) // 与既有一致：删 incoming 跳过（不换 inode）
            return
        }
        // 异/缺 → rm 旧 + rename 新 inode（运行中守护靠旧 inode 幸存）。
        // 降级语义登记（§3.4）：重跑旧版 DMG 会把数据目录 CLI 换回旧版，
        // 恢复=self-update；本步只看 sha 无版本门槛（步 5 才有）。
        try removeIfExists(target)
        try fs.moveItem(at: incoming, to: target)
    }

    // 步 5——app（版本门槛「≥ 跳过」防降级 + 验签失败中止）。
    // 决策段（读可变缝 ownVersion/appIsRunning 与授权旗标）留主 actor——快路径；
    // 落位/验签阻塞体（appPlacement）下放后台。

    @MainActor private func runAppStep() async throws {
        let target = InstallerSupport.installedAppPath
        let needed = InstallerSupport.appInstallNeeded(
            targetVersion: fs.appShortVersion(atBundlePath: target),
            ownVersion: ownVersion())
        if !needed { return }
        // 目标 bundle id 进程在跑（排除安装器自身）：挂起等用户授权，不擅自杀进程
        if !userAuthorizedQuit && appIsRunning() { throw InstallerError.needsAppQuit }
        let verifier = verifySignature
        try await advance(.app) { try self.appPlacement(to: target, verify: verifier) }
    }

    /// app 本体落位：remove 旧 → copy → 整 bundle 递归清隔离 → 验签。
    /// 验签失败=报错中止（不 open、不拉起坏副本）。
    private nonisolated func appPlacement(to target: String,
                                          verify: @Sendable (String) -> Error?) throws {
        try removeIfExists(target)
        try fs.copyItem(at: bundlePath, to: target)
        clearQuarantine(target)
        if let failure = verify(target) {
            throw InstallerError.stepFailure(step: .app, reason: "验签失败：\(failure.localizedDescription)")
        }
    }

    // 步 6——PATH symlink 授权（可选；双层转义 + stdin 交付，§3.5）。
    // 返回值=用户取消授权弹窗（true → 主 actor 落「已跳过」状态）。

    private nonisolated func stepPathAuth() throws -> Bool {
        let binDir = InstallerSupport.pathBinDir(directoryExists: { self.fs.fileExists($0) })
        let appleScript = InstallerSupport.pathAuthAppleScript(home: home, binDir: binDir)
        // stdin 交付：argv 恰为 ["osascript"]，脚本文本绝不进 argv（tun_install.go 先例）
        let result = runner.run(argv: ["osascript"], stdin: appleScript)
        if result.exitCode == 0 { return false }
        // 用户取消授权弹窗：信号回主 actor 落「已跳过」（本步跳过，非失败，完成态如实）
        if isUserCancellation(result) {
            return true
        }
        // osascript 失败 → stderr 摘要（截断），可跳过可重试
        let summary = String(result.stderr.prefix(300))
        throw InstallerError.stepFailure(
            step: .pathAuth,
            reason: summary.isEmpty ? "osascript 退出码 \(result.exitCode)" : summary)
    }

    private nonisolated func isUserCancellation(_ result: InstallerRunResult) -> Bool {
        result.exitCode == -128
            || result.stderr.contains("User canceled")
            || result.stderr.contains("(-128)")
    }

    /// PATH 目录里既有同名「真身文件」（tarball 装的 CLI）→ 界面如实声明接管；
    /// 既有 symlink（DMG 自装的链接）不算接管。
    var willTakeOverExistingCLI: Bool {
        let binDir = InstallerSupport.pathBinDir(directoryExists: { self.fs.fileExists($0) })
        let existing = binDir + "/tradewind"
        return fs.fileExists(existing) && fs.symlinkDestination(existing) == nil
    }

    // MARK: 通用助手（步体伴生，随后台执行）

    private nonisolated func removeIfExists(_ path: String) throws {
        if fs.fileExists(path) { try fs.removeItem(at: path) }
    }

    /// 清 DMG 卷 quarantine（评审 P0）：所有 DMG 落位二进制统一调用；错误码忽略
    /// （对无 quarantine 文件无害）。argv 形态 ["xattr", "-dr", "com.apple.quarantine", 路径]。
    private nonisolated func clearQuarantine(_ path: String) {
        _ = runner.run(argv: ["xattr", "-dr", "com.apple.quarantine", path], stdin: nil)
    }
}
