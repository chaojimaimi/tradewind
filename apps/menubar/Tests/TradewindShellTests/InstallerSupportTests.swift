import Foundation
import Testing
@testable import TradewindKit

// MARK: - §3.9 #1 检测矩阵（双判据缺一不可，纯函数）

@Suite struct InstallerDetectTests {
    private func exists(_ paths: Set<String>) -> (String) -> Bool {
        { paths.contains($0) }
    }

    private func sourcePaths(root: String) -> Set<String> {
        [root + "/bin/mihomo", root + "/ui/index.html", root + "/tradewind"]
    }

    @Test func volumesWithAllThreeSourcesDetectsInstallerMode() {
        let root = "/Volumes/Tradewind"
        #expect(InstallerSupport.detect(bundlePath: root + "/Tradewind.app",
                                        fileExists: exists(sourcePaths(root: root))))
    }

    @Test func missingAnySourceFallsBackToShellMode() {
        let root = "/Volumes/Tradewind"
        let full = sourcePaths(root: root)
        for missing in full {
            let remaining = full.subtracting([missing])
            #expect(!InstallerSupport.detect(bundlePath: root + "/Tradewind.app",
                                             fileExists: exists(remaining)),
                    "缺 \(missing) 仍判安装器模式——双判据失效")
        }
    }

    @Test func nonVolumesPathNeverEntersInstallerMode() {
        let root = "/Users/someone/Downloads"
        #expect(!InstallerSupport.detect(bundlePath: root + "/Tradewind.app",
                                         fileExists: exists(sourcePaths(root: root))))
    }

    @Test func duplicateMountPointWithSpaceStillDetects() {
        let root = "/Volumes/Tradewind 1"
        #expect(InstallerSupport.detect(bundlePath: root + "/Tradewind.app",
                                        fileExists: exists(sourcePaths(root: root))))
    }

    @Test func tradewindNamedMountWithoutSourcesStaysShellMode() {
        // 挂载点名含 "Tradewind" 的干扰卷：仍以源件存在性为准
        let root = "/Volumes/TradewindStuff"
        #expect(!InstallerSupport.detect(bundlePath: root + "/Something.app",
                                         fileExists: exists([])))
    }
}

// MARK: - §3.3/§3.5 数据目录、PATH 目录与双层转义

@Suite struct InstallerPathAndEscapingTests {
    @Test func homeDirPrefersTradedwindHomeEnvElseUserDir() {
        #expect(InstallerSupport.homeDir(environment: ["TRADEDWIND_HOME": "/tmp/tw-x"]) == "/tmp/tw-x")
        let fallback = NSHomeDirectory() + "/Library/Application Support/tradewind"
        #expect(InstallerSupport.homeDir(environment: ["TRADEDWIND_HOME": ""]) == fallback)
        #expect(InstallerSupport.homeDir(environment: [:]) == fallback)
    }

    @Test func pathBinDirTwoBranches() {
        #expect(InstallerSupport.pathBinDir(directoryExists: { $0 == "/opt/homebrew" }) == "/opt/homebrew/bin")
        #expect(InstallerSupport.pathBinDir(directoryExists: { _ in false }) == "/usr/local/bin")
    }

    /// shell 层单引号转义（语义对齐 internal/app/tun_script.go:132）。
    @Test func shQuoteMatchesGoPrecedent() {
        #expect(InstallerSupport.shQuote("/tmp/plain") == "'/tmp/plain'")
        #expect(InstallerSupport.shQuote("/a b/c") == "'/a b/c'")
        #expect(InstallerSupport.shQuote("a'b") == "'a'\\''b'")
    }

    /// AppleScript 字符串字面量转义（语义对齐 internal/app/tun_install.go:209-229）。
    @Test func appleScriptLiteralMatchesGoPrecedent() {
        #expect(InstallerSupport.appleScriptLiteral("plain") == "\"plain\"")
        #expect(InstallerSupport.appleScriptLiteral("a\"b\\c") == "\"a\\\"b\\\\c\"")
        #expect(InstallerSupport.appleScriptLiteral("l1\nl2\r") == "\"l1\\nl2\\r\"")
    }

    /// symlink（非拷贝）语义：ln -sf 在位、无 cp（root 属主目录拷贝件会切断 self-update）。
    @Test func pathAuthScriptCreatesSymlinkNotCopy() {
        let script = InstallerSupport.pathAuthAppleScript(home: "/tmp/tw h", binDir: "/opt/homebrew/bin")
        #expect(script.contains("ln -sf"))
        #expect(!script.contains(" cp "))
        #expect(script.hasPrefix("do shell script "))
        #expect(script.hasSuffix(" with administrator privileges"))
    }

    /// 双层转义断言清单（评审上轮正是 `"` 与 `\` 逃逸 AppleScript 层）：
    /// `'` / `;` / `$()` / 空格 / `"` / `\` 六类逐项在位。
    @Test func pathAuthScriptEscapesAllHostileCharactersThroughBothLayers() {
        let hostile = "/tmp/tw 'x\";$(boom)\\y dir"
        let script = InstallerSupport.pathAuthAppleScript(home: hostile, binDir: "/usr/local/bin")
        #expect(script.contains("/tmp/tw "))     // 空格：整体在单引号路径内
        #expect(script.contains("'\\\\''"))      // shell 层 ' → '\''（经 AppleScript 层 \ 翻倍）
        #expect(script.contains("\\\""))         // AppleScript 层 " → \"
        #expect(script.contains("\\\\y"))        // AppleScript 层 \ → \\
        #expect(script.contains(";"))            // ;：两层引号内惰性
        #expect(script.contains("$("))           // $()：单引号内惰性
        // 属性断言：除首尾定界符外，字面量内不存在未转义 "（含 \\ 后的 " 不算未转义）
        var unescapedQuotes = 0
        var escaped = false
        for character in script {
            if escaped { escaped = false; continue }
            if character == "\\" { escaped = true; continue }
            if character == "\"" { unescapedQuotes += 1 }
        }
        #expect(unescapedQuotes == 2)
    }

    /// 注入毒串：home 携带第二条 do shell script——只能以字面量文本存在，
    /// 不可能成为可执行语句（同一授权会话 root 执行的评审 P1 场景）。
    @Test func pathAuthScriptInjectionPoisonStaysInsideLiteral() {
        let poison = "x\"; do shell script \"touch /tmp/pwned\""
        let script = InstallerSupport.pathAuthAppleScript(home: poison, binDir: "/usr/local/bin")
        var unescapedQuotes = 0
        var escaped = false
        for character in script {
            if escaped { escaped = false; continue }
            if character == "\\" { escaped = true; continue }
            if character == "\"" { unescapedQuotes += 1 }
        }
        #expect(unescapedQuotes == 2)
        #expect(script.contains("\\\"; do shell script"))
    }
}

// MARK: - §3.9 #5 app 版本门槛（全序元组：semver/里程碑/批次字母/N）

@Suite struct InstallerVersionOrderTests {
    @Test func crossBatchLetterAdvanceWinsOverN() throws {
        let installed = try #require(InstallerSupport.versionKey("v0.5.0-m3e2"))
        let dmg = try #require(InstallerSupport.versionKey("v0.5.0-m3f1"))
        // 跨批次字母：m3e2 < m3f1（仅比 N 会误判跳过——评审 P2 场景）
        #expect(dmg > installed)
    }

    @Test func sameBatchComparesN() {
        #expect(InstallerSupport.versionKey("v0.5.0-m3f1")! < InstallerSupport.versionKey("v0.5.0-m3f2")!)
        #expect(InstallerSupport.versionKey("v0.5.0-m3f3")! == InstallerSupport.versionKey("0.5.0-m3f3")!)
    }

    @Test func milestoneThenSemverThenBatchFullOrder() {
        #expect(InstallerSupport.versionKey("v0.5.0-m3z9")! < InstallerSupport.versionKey("v0.6.0-m4a1")!)
        #expect(InstallerSupport.versionKey("v0.4.0-m3a1")! < InstallerSupport.versionKey("v0.5.0-m3a1")!)
        #expect(InstallerSupport.versionKey("v0.5.0-m3f1")! < InstallerSupport.versionKey("v0.5.0-m3f10")!)
    }

    @Test func unparsableVersionsReturnNil() {
        let cases: [String?] = ["", "abc", "1.2", "1.2.3", "1.2.3-rc1", "v1.2.3-", nil]
        for garbage in cases {
            #expect(InstallerSupport.versionKey(garbage) == nil, "\(String(describing: garbage)) 不应解析成功")
        }
    }

    /// 「≥ 跳过」防降级 + 解析失败视为不同（→ 安装）。
    @Test func appGateSkipsWhenTargetNotOlderElseInstalls() {
        #expect(!InstallerSupport.appInstallNeeded(targetVersion: "v0.5.0-m3f3", ownVersion: "v0.5.0-m3f3"))
        #expect(!InstallerSupport.appInstallNeeded(targetVersion: "v0.5.0-m3f4", ownVersion: "v0.5.0-m3f3"))
        #expect(InstallerSupport.appInstallNeeded(targetVersion: "v0.5.0-m3e2", ownVersion: "v0.5.0-m3f1"))
        #expect(InstallerSupport.appInstallNeeded(targetVersion: nil, ownVersion: "v0.5.0-m3f1"))
        #expect(InstallerSupport.appInstallNeeded(targetVersion: "v0.5.0-m3f1", ownVersion: nil))
        #expect(InstallerSupport.appInstallNeeded(targetVersion: "garbage", ownVersion: "v0.5.0-m3f1"))
    }
}

// MARK: - 生产 FS 实现回路（仅 /tmp 短路径，结束即清理）

@Suite struct FileManagerInstallerFSTests {
    @Test func roundTripOperationsUnderTmpPath() throws {
        let fm = FileManager.default
        let root = NSTemporaryDirectory() + "tw-inst-" + UUID().uuidString.prefix(8)
        defer { try? fm.removeItem(atPath: root) }
        let fs = FileManagerInstallerFS()
        try fs.createDirectory(root, intermediate: true, posixMode: 0o700)
        try fs.createDirectory(root + "/bin", intermediate: true, posixMode: 0o700)
        let source = root + "/src.bin"
        try Data("hello-tradewind".utf8).write(to: URL(fileURLWithPath: source))
        let copied = root + "/bin/copy.bin"
        try fs.copyItem(at: source, to: copied)
        try fs.setPosixMode(0o755, at: copied)
        let moved = root + "/bin/moved.bin"
        try fs.moveItem(at: copied, to: moved)
        #expect(try fs.sha256Hex(moved) == InstallerSupport.sha256Hex(Data("hello-tradewind".utf8)))
        #expect(fs.fileExists(moved) && !fs.fileExists(copied))
        try fm.createSymbolicLink(atPath: root + "/link", withDestinationPath: moved)
        #expect(fs.symlinkDestination(root + "/link") == moved)
        #expect(fs.symlinkDestination(moved) == nil)
        try fs.removeItem(at: moved)
        #expect(!fs.fileExists(moved))
        let attributes = try fm.attributesOfItem(atPath: root)
        #expect((attributes[.posixPermissions] as? NSNumber)?.int16Value == 0o700)
    }
}

// MARK: - §3.9 #8 壳寻址链（四候选顺序钉死）

@Suite struct CLIPathResolveChainTests {
    @Test func resolveOrderEnvExplicitHomebrewThenDataDir() {
        let home = "/tmp/tw-resolve/home"
        // env 显式最高
        #expect(CLIPath.resolve(
            environment: [CLIPath.envKey: "/x/tw", InstallerSupport.homeEnvKey: home],
            fileExists: { _ in true }) == "/x/tw")
        // homebrew 优先于 /usr/local/bin（尊重 tarball 用户显式安装与 self-update 通道）
        #expect(CLIPath.resolve(
            environment: [InstallerSupport.homeEnvKey: home],
            fileExists: { $0 == "/opt/homebrew/bin/tradewind" || $0 == home + "/tradewind" })
            == "/opt/homebrew/bin/tradewind")
        #expect(CLIPath.resolve(
            environment: [InstallerSupport.homeEnvKey: home],
            fileExists: { $0 == "/usr/local/bin/tradewind" || $0 == home + "/tradewind" })
            == "/usr/local/bin/tradewind")
        // 数据目录候选（home 由 env 的 TRADEDWIND_HOME 计算，§3.3 同源）
        #expect(CLIPath.resolve(
            environment: [InstallerSupport.homeEnvKey: home],
            fileExists: { $0 == home + "/tradewind" }) == home + "/tradewind")
        // 全缺 → nil
        #expect(CLIPath.resolve(
            environment: [InstallerSupport.homeEnvKey: home],
            fileExists: { _ in false }) == nil)
    }

    @Test func resolveDataDirCandidateDerivedFromTradedwindHome() {
        let a = "/tmp/tw-resolve/a"
        let b = "/tmp/tw-resolve/b"
        #expect(CLIPath.resolve(environment: [InstallerSupport.homeEnvKey: a],
                                fileExists: { $0 == a + "/tradewind" }) == a + "/tradewind")
        #expect(CLIPath.resolve(environment: [InstallerSupport.homeEnvKey: b],
                                fileExists: { $0 == a + "/tradewind" }) == nil)
    }
}

// MARK: - §3.9 #9 缺件引导文案（全新机器签名 → DMG 引导；其余旧文案不变）

@MainActor @Suite struct PanelGuidanceToastTests {
    private static let newToast = "未完成安装：请打开 DMG 里的 Tradewind 完成安装，或下载整包 tarball（见 README）"
    private static let legacyToast = "tradewind CLI 未找到（设 TRADEDWIND_CLI 或安装）"

    @Test func freshMachineSignatureShowsDmgGuidance() {
        let home = "/tmp/tw-toast/fresh"
        #expect(PanelModel.missingCLIToast(environment: [InstallerSupport.homeEnvKey: home],
                                           fileExists: { _ in false }) == Self.newToast)
    }

    @Test func uiInstalledKeepsLegacyToast() {
        // 仅 CLI 缺（装过 ui）：bin/mihomo 缺但 ui/index.html 在 → 旧文案
        let home = "/tmp/tw-toast/ui-only"
        #expect(PanelModel.missingCLIToast(
            environment: [InstallerSupport.homeEnvKey: home],
            fileExists: { $0 == home + "/ui/index.html" }) == Self.legacyToast)
    }

    @Test func kernelInstalledKeepsLegacyToast() {
        // 装过内核的老机器同样旧文案
        let home = "/tmp/tw-toast/kernel-only"
        #expect(PanelModel.missingCLIToast(
            environment: [InstallerSupport.homeEnvKey: home],
            fileExists: { $0 == home + "/bin/mihomo" }) == Self.legacyToast)
    }
}
