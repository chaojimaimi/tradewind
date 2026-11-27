import Foundation
import Testing
@testable import TradewindKit

// MARK: - 注入缝 fake（内存 FS + 记录型 runner；绝不触真实 /Applications/PATH/数据目录）
// @unchecked Sendable：步体经后台任务执行（评审必修 2），缝对象需可跨线程；
// 两 fake 只被串行驱动（每步 await 完成才进下一步），无真实并发写。

/// 内存文件系统：记录操作序列（rename 序/双临时名/幂等断言），不落盘。
private final class MemoryInstallerFS: InstallerFS, @unchecked Sendable {
    enum Entry: Equatable {
        case file(Data)
        case directory
        case symlink(String)
    }

    struct Op: Equatable {
        let kind: String
        let at: String
        let to: String?
    }

    private(set) var ops: [Op] = []
    var entries: [String: Entry] = [:]
    /// bundle 路径 → CFBundleShortVersionString（步 5 版本门槛数据源）
    var versions: [String: String] = [:]
    /// 脚本化失败：copyItem 到该目标时抛错（步 4 重试用例）
    var failCopyTo: String?

    func fileExists(_ path: String) -> Bool { entries[path] != nil }

    func createDirectory(_ path: String, intermediate: Bool, posixMode: Int) throws {
        entries[path] = .directory
        ops.append(Op(kind: "mkdir \(posixMode)", at: path, to: nil))
    }

    /// 目录子树（对齐 FileManager 目录操作递归语义——拷贝 ui/ 必须带走 index.html）。
    private func subtree(of path: String) -> [String: Entry] {
        let prefix = path + "/"
        return entries.filter { $0.key.hasPrefix(prefix) }
    }

    func copyItem(at: String, to: String) throws {
        if let target = failCopyTo, target == to {
            failCopyTo = nil
            throw CocoaError(.fileWriteUnknown)
        }
        guard let source = entries[at] else { throw CocoaError(.fileReadNoSuchFile) }
        for key in subtree(of: to).keys { entries.removeValue(forKey: key) } // 覆盖语义
        entries[to] = source
        let prefix = at + "/"
        for (key, value) in subtree(of: at) {
            entries[to + "/" + key.dropFirst(prefix.count)] = value
        }
        ops.append(Op(kind: "copy", at: at, to: to))
    }

    func moveItem(at: String, to: String) throws {
        guard let source = entries[at] else { throw CocoaError(.fileReadNoSuchFile) }
        entries[to] = source
        entries.removeValue(forKey: at)
        let prefix = at + "/"
        for (key, value) in subtree(of: at) {
            entries[to + "/" + key.dropFirst(prefix.count)] = value
            entries.removeValue(forKey: key)
        }
        ops.append(Op(kind: "move", at: at, to: to))
    }

    func removeItem(at: String) throws {
        entries.removeValue(forKey: at)
        for key in subtree(of: at).keys { entries.removeValue(forKey: key) }
        ops.append(Op(kind: "remove", at: at, to: nil))
    }

    func contents(_ path: String) throws -> Data {
        if case .file(let data) = entries[path] { return data }
        throw CocoaError(.fileReadNoSuchFile)
    }

    func sha256Hex(_ path: String) throws -> String {
        InstallerSupport.sha256Hex(try contents(path))
    }

    func setPosixMode(_ mode: Int, at path: String) throws {
        ops.append(Op(kind: "chmod \(mode)", at: path, to: nil))
    }

    func symlinkDestination(_ path: String) -> String? {
        if case .symlink(let destination) = entries[path] { return destination }
        return nil
    }

    func appShortVersion(atBundlePath path: String) -> String? { versions[path] }

    func write(_ path: String, _ data: Data) { entries[path] = .file(data) }
    func link(_ path: String, to destination: String) { entries[path] = .symlink(destination) }
    func data(at path: String) -> Data {
        if case .file(let data) = entries[path] { return data }
        return Data()
    }
}

/// 记录型 runner：xattr 恒成功（错误码忽略语义）；osascript 结果可脚本化。
private final class RecordingInstallerRunner: InstallerRunner, @unchecked Sendable {
    struct Call: Equatable {
        let argv: [String]
        let stdin: String?
    }

    private(set) var calls: [Call] = []
    /// osascript 调用的脚本化结果（nil=默认成功）
    var osascriptResult: InstallerRunResult?

    func run(argv: [String], stdin: String?) -> InstallerRunResult {
        calls.append(Call(argv: argv, stdin: stdin))
        if argv.first == "osascript", let result = osascriptResult { return result }
        return InstallerRunResult(exitCode: 0, stdout: "", stderr: "")
    }

    var osascriptCalls: [Call] { calls.filter { $0.argv.first == "osascript" } }
    /// 清 quarantine 调用的目标路径序列（DMG 落位二进制断言用）
    var quarantineTargets: [String] {
        calls.filter { $0.argv.first == "xattr" }.compactMap { $0.argv.last }
    }
}

/// 测试计数盒：@Sendable 缝闭包内可变捕获的容器（直捕局部 var 在 v5 模式告警）。
private final class CounterBox: @unchecked Sendable {
    var count = 0
}

// MARK: - §3.9 #2/#3 步骤序、幂等与内核原子性；#5/#6 模型级门槛与重试

@MainActor @Suite struct InstallerModelStepsTests {
    static let bundlePath = "/Volumes/Tradewind/Tradewind.app"
    static let home = "/tmp/tw-inst-fixture/home"
    static let appTarget = InstallerSupport.installedAppPath
    static let kernelData = Data("mihomo-binary-bytes".utf8)
    static let uiData = Data("<html>metacubexd</html>".utf8)
    static let cliData = Data("tradewind-cli-bytes".utf8)

    private func makeFS() -> MemoryInstallerFS { MemoryInstallerFS() }

    /// 装配带 DMG 源四件的模型（全内存 fixture；home 经 TRADEDWIND_HOME 注入）。
    private func makeModel(fs: MemoryInstallerFS,
                           runner: RecordingInstallerRunner,
                           ownVersion: String? = "v0.5.0-m3f3",
                           targetVersion: String? = nil,
                           running: Bool = false,
                           signatureError: (any Error)? = nil) -> InstallerModel {
        let model = InstallerModel(bundlePath: Self.bundlePath,
                                   environment: [InstallerSupport.homeEnvKey: Self.home],
                                   fs: fs, runner: runner)
        model.ownVersion = { ownVersion }
        model.verifySignature = { _ in signatureError }
        model.appIsRunning = { running }
        model.terminateRunningApp = {}
        model.openInstalledApp = { true }
        model.terminateSelf = {}
        if let targetVersion { fs.versions[Self.appTarget] = targetVersion }
        let root = InstallerSupport.sourceRoot(bundlePath: Self.bundlePath)
        let kernelHex = InstallerSupport.sha256Hex(Self.kernelData)
        fs.write(root + "/bin/mihomo", Self.kernelData)
        fs.write(root + "/bin/mihomo.sha256", Data((kernelHex + "  mihomo\n").utf8))
        fs.write(root + "/ui/index.html", Self.uiData)
        fs.entries[root + "/ui"] = .directory
        fs.write(root + "/tradewind", Self.cliData)
        fs.entries[Self.bundlePath] = .directory
        return model
    }

    private func writeHealthyKernelPair(_ fs: MemoryInstallerFS, kernel: Data) {
        try? fs.createDirectory(Self.home + "/bin", intermediate: true, posixMode: 0o700)
        fs.write(Self.home + "/bin/mihomo", kernel)
        let hex = InstallerSupport.sha256Hex(kernel)
        fs.write(Self.home + "/bin/mihomo.sha256", Data((hex + "  mihomo\n").utf8))
    }

    private func kernelOps(_ fs: MemoryInstallerFS) -> [MemoryInstallerFS.Op] {
        fs.ops.filter { op in
            let all = op.at + (op.to ?? "")
            return all.contains("mihomo") || all.contains("\(Self.home)/bin")
        }
    }

    // MARK: 全新机全流程（步序落位四件 + 目录 0700）

    @Test func freshInstallLandsKernelSidecarUIAndCLIIdempotently() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        let model = makeModel(fs: fs, runner: runner, targetVersion: "v0.5.0-m3e2")
        await model.startInstall()
        #expect(model.phase == .finished(nil))
        #expect(fs.data(at: Self.home + "/bin/mihomo") == Self.kernelData)
        #expect(fs.data(at: Self.home + "/bin/mihomo.sha256")
            == Data((InstallerSupport.sha256Hex(Self.kernelData) + "  mihomo\n").utf8))
        #expect(fs.data(at: Self.home + "/ui/index.html") == Self.uiData)
        #expect(fs.data(at: Self.home + "/tradewind") == Self.cliData)
        // 步 1 权限语义：home 与 bin/ 显式 0700（对齐 paths.go Ensure）
        #expect(fs.ops.contains { $0.kind == "mkdir 448" && $0.at == Self.home })
        #expect(fs.ops.contains { $0.kind == "mkdir 448" && $0.at == Self.home + "/bin" })
        // 幂等：重跑无 app 重复落位、无 .incoming 残留
        await model.startInstall()
        #expect(model.phase == .finished(nil))
        #expect(fs.entries.keys.filter { $0.contains(".incoming") }.isEmpty)
    }

    // MARK: §3.4 步 2——「成对且校验通过」跳过 / 损坏态整套替换

    @Test func healthyKernelPairSkipsStep2EvenWhenDmgKernelDiffers() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        writeHealthyKernelPair(fs, kernel: Data("self-update-upgraded-kernel".utf8)) // 与 DMG 件不同
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        // 「只填缺不降级」：健康内核零操作（copy/move/chmod/remove 均不触碰）
        #expect(kernelOps(fs).filter { $0.kind != "mkdir 448" }.isEmpty)
        #expect(fs.data(at: Self.home + "/bin/mihomo") == Data("self-update-upgraded-kernel".utf8))
    }

    @Test func corruptSidecarReplacesWholePairWithDmgBytes() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        writeHealthyKernelPair(fs, kernel: Self.kernelData)
        fs.write(Self.home + "/bin/mihomo.sha256", Data(("deadbeef" + String(repeating: "0", count: 56)).utf8))
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        #expect(fs.data(at: Self.home + "/bin/mihomo") == Self.kernelData) // 损坏态降级修复
        #expect(kernelOps(fs).contains { $0.kind == "remove" && $0.at == Self.home + "/bin/mihomo" })
    }

    @Test func missingSidecarReplacesWholePair() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        try? fs.createDirectory(Self.home + "/bin", intermediate: true, posixMode: 0o700)
        fs.write(Self.home + "/bin/mihomo", Self.kernelData) // sidecar 缺
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        #expect(fs.data(at: Self.home + "/bin/mihomo.sha256")
            == Data((InstallerSupport.sha256Hex(Self.kernelData) + "  mihomo\n").utf8))
    }

    /// §3.9 #3：双临时名 + 「sidecar 先、binary 后」rename 序。
    @Test func kernelReplaceUsesDoubleTempNamesWithSidecarFirstRenameOrder() async throws {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        writeHealthyKernelPair(fs, kernel: Self.kernelData)
        fs.write(Self.home + "/bin/mihomo.sha256", Data("corrupted".utf8))
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        let root = InstallerSupport.sourceRoot(bundlePath: Self.bundlePath)
        let incomingKernel = "\(Self.home)/bin/.mihomo.incoming"
        let incomingSidecar = "\(Self.home)/bin/.mihomo.sha256.incoming"
        let finalKernel = Self.home + "/bin/mihomo"
        let finalSidecar = Self.home + "/bin/mihomo.sha256"
        let kernelOpsOnly = kernelOps(fs)
        func index(of kind: String, at: String, to: String? = nil) -> Int? {
            kernelOpsOnly.firstIndex {
                $0.kind == kind && $0.at == at && $0.to == to
            }
        }
        let copyKernel = try #require(index(of: "copy", at: root + "/bin/mihomo", to: incomingKernel))
        let copySidecar = try #require(index(of: "copy", at: root + "/bin/mihomo.sha256", to: incomingSidecar))
        let chmod = try #require(index(of: "chmod 493", at: incomingKernel)) // 0o755
        let removeOld = try #require(index(of: "remove", at: finalKernel))
        let moveSidecar = try #require(index(of: "move", at: incomingSidecar, to: finalSidecar))
        let moveKernel = try #require(index(of: "move", at: incomingKernel, to: finalKernel))
        // 全序列：清旧 → 双临时拷入 → chmod → sidecar rename 先于 binary rename
        #expect(removeOld < copyKernel && copyKernel < copySidecar && copySidecar < chmod)
        #expect(chmod < moveSidecar && moveSidecar < moveKernel)
        #expect(fs.data(at: finalKernel) == Self.kernelData)
    }

    @Test func repairedKernelIsHealthyOnRerun() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        writeHealthyKernelPair(fs, kernel: Self.kernelData)
        fs.write(Self.home + "/bin/mihomo.sha256", Data("corrupted".utf8))
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        // 步 1 每次重跑都有 mkdir（非内核语义），排除后比对内核操作数
        let coreKernelOps = { kernelOps(fs).filter { $0.kind != "mkdir 448" } }
        let kernelOpCount = coreKernelOps().count
        await model.startInstall() // 重跑：修复后的对=健康 → 步 2 跳过
        #expect(coreKernelOps().count == kernelOpCount)
    }

    // MARK: §3.4 步 3/4——ui 覆盖刷新；CLI sha 双侧比对

    @Test func uiRerunOverwritesTarget() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        let uiOpCount = fs.ops.filter { ($0.at + ($0.to ?? "")).contains("\(Self.home)/ui") }.count
        await model.startInstall()
        let secondRun = Array(fs.ops.dropFirst(uiOpCount))
            .filter { ($0.at + ($0.to ?? "")).contains("\(Self.home)/ui") }
        // 重跑=刷新：removeItem 旧 + copyItem 新 再来一遍
        #expect(secondRun.contains { $0.kind == "remove" && $0.at == Self.home + "/ui" })
        #expect(secondRun.contains { $0.kind == "copy" && $0.to == Self.home + "/ui" })
        #expect(fs.data(at: Self.home + "/ui/index.html") == Self.uiData)
    }

    @Test func cliShaIdenticalSkipsInodeSwap() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        writeHealthyKernelPair(fs, kernel: Self.kernelData) // 内核对健康：步 2 跳过
        try? fs.createDirectory(Self.home, intermediate: true, posixMode: 0o700)
        fs.write(Self.home + "/tradewind", Self.cliData) // 与 DMG 件 sha 一致
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        // 同→删 incoming 跳过：无 rm 旧、无 rename 新 inode
        #expect(kernelOps(fs).allSatisfy { $0.kind == "mkdir 448" }) // 内核对健康：无内核操作
        #expect(fs.ops.contains { $0.kind == "remove" && $0.at == Self.home + "/.tradewind.incoming" })
        #expect(!fs.ops.contains { $0.kind == "move" && $0.to == Self.home + "/tradewind" })
        #expect(!fs.ops.contains { $0.kind == "remove" && $0.at == Self.home + "/tradewind" })
    }

    @Test func cliShaDiffersReplacesViaRemoveAndRename() async throws {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        try? fs.createDirectory(Self.home, intermediate: true, posixMode: 0o700)
        fs.write(Self.home + "/tradewind", Data("old-cli".utf8))
        let model = makeModel(fs: fs, runner: runner)
        await model.startInstall()
        let removeOld = try #require(fs.ops.firstIndex { $0.kind == "remove" && $0.at == Self.home + "/tradewind" })
        let moveNew = try #require(fs.ops.firstIndex { $0.kind == "move" && $0.to == Self.home + "/tradewind" })
        #expect(removeOld < moveNew) // rm 旧 + rename 新 inode（运行中守护靠旧 inode 幸存）
        #expect(fs.data(at: Self.home + "/tradewind") == Self.cliData)
    }

    /// 评审 P0：凡 DMG 落位二进制一律清 com.apple.quarantine（kernel/CLI/app 三处）。
    @Test func quarantineClearedOnEveryDmgPlacedBinary() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        let model = makeModel(fs: fs, runner: runner, targetVersion: "v0.5.0-m3e2")
        await model.startInstall()
        #expect(runner.quarantineTargets == [
            "\(Self.home)/bin/.mihomo.incoming",
            "\(Self.home)/.tradewind.incoming",
            Self.appTarget,
        ])
        #expect(runner.osascriptCalls.isEmpty) // 未勾选 PATH 项：零 osascript
    }

    // MARK: §3.9 #5 步 5 版本门槛（模型级）

    @Test func appStepSkipsWhenTargetNotOlder() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        let model = makeModel(fs: fs, runner: runner, targetVersion: "v0.5.0-m3f3") // 等于自身
        await model.startInstall()
        #expect(model.phase == .finished(nil))
        #expect(!fs.ops.contains { ($0.at + ($0.to ?? "")).contains(Self.appTarget) })
    }

    @Test func appStepInstallsWhenTargetOlderOrUnparsable() async {
        for target in ["v0.5.0-m3e2", "not-a-version", nil] {
            let fs = makeFS()
            let runner = RecordingInstallerRunner()
            let model = makeModel(fs: fs, runner: runner, targetVersion: target)
            await model.startInstall()
            #expect(model.phase == .finished(nil))
            #expect(fs.entries[Self.appTarget] == .directory) // app 本体已落位
            #expect(runner.quarantineTargets.contains(Self.appTarget))
        }
    }

    @Test func runningTargetWaitsForUserAuthorizationThenContinues() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        let model = makeModel(fs: fs, runner: runner, targetVersion: "v0.5.0-m3e2", running: true)
        await model.startInstall()
        #expect(model.phase == .needsAppQuit) // 不擅自杀进程：等「退出并继续」
        let terminated = CounterBox()
        model.terminateRunningApp = { terminated.count += 1 }
        await model.confirmQuitAndContinue() // 用户点击即授权语义
        #expect(terminated.count == 1)
        #expect(model.phase == .finished(nil))
        #expect(fs.entries[Self.appTarget] == .directory) // appIsRunning 仍真：授权后不再挂起
    }

    // MARK: §3.9 #6 失败重试与验签中止

    @Test func cliStepFailureThenRetryCompletesIdempotently() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        writeHealthyKernelPair(fs, kernel: Data("installed-healthy-kernel".utf8))
        fs.entries[Self.home + "/ui"] = .directory // ui 已装：让首个失败点落在步 4
        fs.write(Self.home + "/ui/index.html", Self.uiData)
        let model = makeModel(fs: fs, runner: runner)
        fs.failCopyTo = Self.home + "/.tradewind.incoming"
        await model.startInstall()
        guard case .failed(.cli, _) = model.phase else {
            Issue.record("期望步 4 失败，实际 \(model.phase)")
            return
        }
        fs.failCopyTo = nil
        await model.retry() // 从步 1 重跑（全幂等）
        #expect(model.phase == .finished(nil))
        #expect(fs.data(at: Self.home + "/tradewind") == Self.cliData)
        #expect(fs.data(at: Self.home + "/bin/mihomo") == Data("installed-healthy-kernel".utf8)) // 内核未动
    }

    @Test func signatureFailureAbortsAndNeverOpens() async {
        let fs = makeFS()
        let runner = RecordingInstallerRunner()
        let model = makeModel(fs: fs, runner: runner, targetVersion: "v0.5.0-m3e2",
                              signatureError: CocoaError(.fileReadUnknown))
        let opened = CounterBox()
        model.openInstalledApp = { opened.count += 1; return true }
        await model.startInstall()
        guard case .failed(.app, let message) = model.phase else {
            Issue.record("期望步 5 验签失败，实际 \(String(describing: model.phase))")
            return
        }
        #expect(message.contains("验签失败"))
        #expect(opened.count == 0) // 验签失败=中止，不 open 不拉起坏副本
    }
}

// MARK: - 评审必修 1：目标实例定位谓词（排除安装器自身进程）

@Suite struct InstallerForeignProcessPredicateTests {
    /// 列表含自身 pid：自身被剔除——只剩安装器自己（全新机的真实形态）时
    /// 过滤结果为空 → 不算「目标在跑」，步 5 不再恒 needsAppQuit（P0 误报场景）。
    @Test func listContainingOwnPidIsNotRunning() {
        #expect(InstallerModel.foreignRunningPids([4242], currentPid: 4242).isEmpty)
        #expect(InstallerModel.foreignRunningPids([4242, 7], currentPid: 4242) == [7]) // 自身剔除、他者保留
    }

    /// 列表只有他者 pid → running（定位谓词的正面语义）。
    @Test func foreignOnlyPidCountsAsRunning() {
        #expect(InstallerModel.foreignRunningPids([7], currentPid: 4242) == [7])
    }
}

// MARK: - §3.9 #4 PATH 授权步（stdin 交付 / 取消跳过 / 失败可跳可重试）

@MainActor @Suite struct InstallerPathAuthTests {
    @Test func pathAuthDeliversScriptViaStdinWithBareArgv() async throws {
        let fs = MemoryInstallerFS()
        let runner = RecordingInstallerRunner()
        let model = Self.makeModel(fs: fs, runner: runner, optIn: true)
        await model.startInstall()
        let call = try #require(runner.osascriptCalls.first)
        // argv 恰为 ["osascript"]：脚本文本经 stdin、不进 argv（tun_install.go 先例）
        #expect(call.argv == ["osascript"])
        #expect(call.stdin?.contains("do shell script") == true)
        #expect(call.stdin?.contains("ln -sf") == true)
        #expect(model.phase == .finished(nil))
        #expect(!model.pathAuthSkipped)
    }

    @Test func canceledAuthorizationMarksSkippedAndFinishesHonestly() async {
        let fs = MemoryInstallerFS()
        let runner = RecordingInstallerRunner()
        runner.osascriptResult = InstallerRunResult(exitCode: 1, stdout: "",
                                                    stderr: "execution error: User canceled. (-128)")
        let model = Self.makeModel(fs: fs, runner: runner, optIn: true)
        await model.startInstall()
        // 取消授权=该步跳过、其余步骤已完成、完成态如实
        #expect(model.phase == .finished(nil))
        #expect(model.pathAuthSkipped)
    }

    @Test func osascriptFailureSurfacesStderrSummaryThenSkippable() async {
        let fs = MemoryInstallerFS()
        let runner = RecordingInstallerRunner()
        runner.osascriptResult = InstallerRunResult(exitCode: 2, stdout: "",
                                                    stderr: "mkdir: /usr/local/bin: permission denied")
        let model = Self.makeModel(fs: fs, runner: runner, optIn: true)
        await model.startInstall()
        guard case .failed(.pathAuth, let message) = model.phase else {
            Issue.record("期望步 6 失败，实际 \(String(describing: model.phase))")
            return
        }
        #expect(message.contains("permission denied"))
        await model.skipPathAuth() // 可跳过：其余步骤已完成
        #expect(model.phase == .finished(nil))
        #expect(model.pathAuthSkipped)
    }

    @Test func pathBinDirChoiceFeedsLnTargetViaSeam() async throws {
        let fs = MemoryInstallerFS()
        fs.entries["/opt/homebrew"] = .directory
        let runner = RecordingInstallerRunner()
        let model = Self.makeModel(fs: fs, runner: runner, optIn: true)
        await model.startInstall()
        let script = try #require(runner.osascriptCalls.first?.stdin)
        #expect(script.contains("/opt/homebrew/bin"))
        #expect(!script.contains("/usr/local/bin"))
    }

    @Test func takeoverNoticeForRealFileNotSymlink() {
        let fs = MemoryInstallerFS()
        let runner = RecordingInstallerRunner()
        let model = Self.makeModel(fs: fs, runner: runner, optIn: true)
        #expect(!model.willTakeOverExistingCLI) // 无既有命令
        fs.link("/usr/local/bin/tradewind", to: "/tmp/tw-inst-fixture/home/tradewind")
        #expect(!model.willTakeOverExistingCLI) // 既有 symlink：非接管
        fs.entries["/usr/local/bin/tradewind"] = .file(Data("tarball-cli".utf8))
        #expect(model.willTakeOverExistingCLI) // 既有真身：界面如实声明接管
        _ = runner
    }

    /// fixture：预置健康内核对（步 2 跳过）与 ui，聚焦 PATH 步行为。
    private static func makeModel(fs: MemoryInstallerFS, runner: RecordingInstallerRunner,
                                  optIn: Bool) -> InstallerModel {
        try? fs.createDirectory(home, intermediate: true, posixMode: 0o700)
        try? fs.createDirectory(home + "/bin", intermediate: true, posixMode: 0o700)
        let kernel = Data("healthy-kernel".utf8)
        fs.write(home + "/bin/mihomo", kernel)
        fs.write(home + "/bin/mihomo.sha256",
                 Data((InstallerSupport.sha256Hex(kernel) + "  mihomo\n").utf8))
        fs.entries[home + "/ui"] = .directory
        fs.write(home + "/ui/index.html", Data("<html></html>".utf8))
        let root = InstallerSupport.sourceRoot(bundlePath: bundlePath)
        fs.write(root + "/bin/mihomo", kernel)
        fs.write(root + "/bin/mihomo.sha256",
                 Data((InstallerSupport.sha256Hex(kernel) + "  mihomo\n").utf8))
        fs.entries[root + "/ui"] = .directory
        fs.write(root + "/ui/index.html", Data("<html></html>".utf8))
        fs.write(root + "/tradewind", Data("cli".utf8))
        fs.entries[bundlePath] = .directory
        fs.versions[InstallerSupport.installedAppPath] = "v0.5.0-m3f3"
        let model = InstallerModel(bundlePath: bundlePath,
                                   environment: [InstallerSupport.homeEnvKey: home],
                                   fs: fs, runner: runner)
        model.ownVersion = { "v0.5.0-m3f3" }
        model.verifySignature = { _ in nil }
        model.appIsRunning = { false }
        model.terminateRunningApp = {}
        model.openInstalledApp = { true }
        model.terminateSelf = {}
        model.installPATH = optIn
        return model
    }

    private static let bundlePath = "/Volumes/Tradewind/Tradewind.app"
    private static let home = "/tmp/tw-inst-fixture/home"
}
