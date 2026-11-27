// 批⑦ c1（docs/plans/batch7-plan.md §3.1/§3.3/§3.5/§3.6/§3.8）：DMG 安装器支撑层。
// 检测判据/数据目录计算/PATH 目录选择/双层转义/sha256/版本全序 —— 全部纯函数可测；
// InstallerFS/InstallerRunner 是文件系统与子进程的注入缝（单测全走 fake，绝不触真实
// /Applications、PATH 目录与数据目录）。osascript 双层转义与 stdin 交付语义对齐 Go
// 先例：internal/app/tun_script.go:132（shQuote）与 internal/app/tun_install.go:209-239
// （appleScriptLiteral + argv 恰为 ["osascript"] 的 stdin 交付）。
import CryptoKit
import Foundation
import Security

/// 安装器支撑层（批⑦ c1）：检测、寻址、转义、版本序与路径常量。
enum InstallerSupport {
    // MARK: 路径常量（方案 §2.1 DMG 四件并排 / §3.4 数据目录落位）

    /// 数据目录环境变量（与 Go internal/paths/paths.go EnvHome 同名同义）。
    static let homeEnvKey = "TRADEDWIND_HOME"
    /// 安装目标（公开定值——进度/完成态文案唯一豁免的路径，§3.4 纪律）。
    static let installedAppPath = "/Applications/Tradewind.app"

    // MARK: 检测判据（§3.1，纯函数）

    /// 安装器模式检测（双判据缺一不可）：bundlePath 含 `/Volumes/`（UDZO 只读卷
    /// 无 App Translocation，重名挂载 "/Volumes/Tradewind 1" 仍命中）且 bundle
    /// 同目录存在三个源件 bin/mihomo、ui/index.html、tradewind。挂载点名含
    /// "Tradewind" 的干扰路径同样以源件存在性为准。
    static func detect(bundlePath: String,
                       fileExists: (String) -> Bool = { FileManager.default.fileExists(atPath: $0) }) -> Bool {
        guard bundlePath.contains("/Volumes/") else { return false }
        let root = sourceRoot(bundlePath: bundlePath)
        return fileExists(root + "/bin/mihomo")
            && fileExists(root + "/ui/index.html")
            && fileExists(root + "/tradewind")
    }

    /// DMG 卷内源件目录（bundle 同目录，四件锚点）。
    static func sourceRoot(bundlePath: String) -> String {
        (bundlePath as NSString).deletingLastPathComponent
    }

    // MARK: 数据目录（§3.3，与 Go paths.Default() 逐条对齐）

    /// TRADEDWIND_HOME 非空 → 用之；否则 ~/Library/Application Support/tradewind。
    /// 测试隔离一律经 environment 注入（绝不触真实数据目录）。
    static func homeDir(environment: [String: String] = ProcessInfo.processInfo.environment) -> String {
        if let home = environment[homeEnvKey], !home.isEmpty { return home }
        return NSHomeDirectory() + "/Library/Application Support/tradewind"
    }

    // MARK: PATH 目录选择（§3.5）

    /// `/opt/homebrew` 存在 → `/opt/homebrew/bin`；否则 `/usr/local/bin`。
    static func pathBinDir(directoryExists: (String) -> Bool = { FileManager.default.fileExists(atPath: $0) }) -> String {
        directoryExists("/opt/homebrew") ? "/opt/homebrew/bin" : "/usr/local/bin"
    }

    // MARK: 双层转义（§3.5，评审 P1）

    /// shell 层单引号转义：'…' 包裹，串内 `'` → `'\''`（语义对齐 tun_script.go:132）。
    static func shQuote(_ text: String) -> String {
        "'" + text.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    /// AppleScript 字符串字面量转义：`\` → `\\`、`"` → `\"`、LF → `\n`、CR → `\r`
    /// （语义对齐 tun_install.go:209-229）。缺此层，含 `"`/`\` 的 home 会逃逸出
    /// `do shell script` 字符串、注入的第二条 shell 命令在同一授权会话内不经二次
    /// 弹窗以 root 执行。
    static func appleScriptLiteral(_ text: String) -> String {
        var literal = "\""
        for character in text {
            switch character {
            case "\\": literal += "\\\\"
            case "\"": literal += "\\\""
            case "\n": literal += "\\n"
            case "\r": literal += "\\r"
            default: literal.append(character)
            }
        }
        return literal + "\""
    }

    /// PATH 授权 AppleScript（纯函数）：mkdir -p + ln -sf（**symlink 非拷贝**——
    /// root 属主目录里的拷贝件会切断 self-update，评审 P1）；shell 层与 AppleScript
    /// 层转义齐备后经 stdin 喂 osascript。生产编排见 InstallerModel.stepPathAuth。
    static func pathAuthAppleScript(home: String, binDir: String) -> String {
        let shell = "mkdir -p \(shQuote(binDir)) && ln -sf \(shQuote(home + "/tradewind")) \(shQuote(binDir + "/tradewind"))"
        return "do shell script \(appleScriptLiteral(shell)) with administrator privileges"
    }

    // MARK: 版本全序（§3.4 步 5，评审 P2 跨批次字母）

    /// 版本比较键：全序元组（semver 三段、里程碑数、批次字母字典序、N 计数）。
    /// 发版形如 v0.5.0-m3f3；跨批次字母推进（已装 m3e2 vs DMG m3f1）必须判
    /// "DMG 更新"——仅比 N 会误判跳过。
    struct VersionKey: Equatable, Comparable {
        let major: Int, minor: Int, patch: Int
        let milestone: Int
        let batch: String
        let n: Int

        static func < (lhs: VersionKey, rhs: VersionKey) -> Bool {
            let left = (lhs.major, lhs.minor, lhs.patch, lhs.milestone, lhs.batch, lhs.n)
            let right = (rhs.major, rhs.minor, rhs.patch, rhs.milestone, rhs.batch, rhs.n)
            return left < right
        }
    }

    /// 解析 CFBundleShortVersionString（可选 v 前缀 + `-m<里程碑><批次字母><N>`）。
    /// 解析失败返回 nil——调用方按方案"版本串解析失败视为不同"处理。
    static func versionKey(_ raw: String?) -> VersionKey? {
        guard let raw, !raw.isEmpty else { return nil }
        var core = raw
        if core.hasPrefix("v") { core.removeFirst() }
        let parts = core.split(separator: "-", maxSplits: 1, omittingEmptySubsequences: false)
        guard parts.count == 2 else { return nil }
        let semver = parts[0].split(separator: ".")
        guard semver.count == 3,
              let major = Int(semver[0]), let minor = Int(semver[1]), let patch = Int(semver[2])
        else { return nil }
        // 预发布段 m<里程碑><批次字母><N>，如 m3f3
        let suffix = String(parts[1])
        guard suffix.hasPrefix("m") else { return nil }
        let digits = suffix.dropFirst().prefix(while: \.isNumber)
        guard let milestone = Int(digits) else { return nil }
        let remainder = suffix.dropFirst().dropFirst(digits.count)
        let batch = remainder.prefix(while: { !$0.isNumber })
        guard !batch.isEmpty, let n = Int(remainder.dropFirst(batch.count)) else { return nil }
        return VersionKey(major: major, minor: minor, patch: patch,
                          milestone: milestone, batch: String(batch), n: n)
    }

    /// 步 5 门槛（纯函数）：目标 ≥ 自身 → false（跳过，防 Sparkle 升级后被旧 DMG
    /// 回滚）；目标 < 自身或任一侧解析失败（视为不同）→ true（安装）。
    static func appInstallNeeded(targetVersion: String?, ownVersion: String?) -> Bool {
        switch (versionKey(targetVersion), versionKey(ownVersion)) {
        case (let target?, let own?): return target < own
        default: return true
        }
    }

    // MARK: 摘要助手（复刻 internal/core/binary.go VerifyBinary 口径）

    /// sha256 hex（小写）。
    static func sha256Hex(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    /// sidecar 期望摘要：首行 trim 后取前 64 字符小写（兼容 `shasum -a 256` 输出
    /// 格式；过短返回 nil=无效 sidecar）。
    static func sidecarExpectedHex(_ sidecarData: Data) -> String? {
        guard let text = String(data: sidecarData, encoding: .utf8) else { return nil }
        let firstLine = text.split(separator: "\n", omittingEmptySubsequences: false).first
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) } ?? ""
        guard firstLine.count >= 64 else { return nil }
        return String(firstLine.prefix(64)).lowercased()
    }
}

// MARK: - 注入缝协议（§3.8：FS/Process 全走缝，单测绝不触真实目录、绝不真跑外命令）
// Sendable 约束：步体在后台任务执行（评审必修 2），缝对象须可跨线程派发。

/// 文件系统缝：生产 = FileManagerInstallerFS；单测 = 内存 fake（记录操作序列供
/// rename 序/双临时名断言）。
protocol InstallerFS: AnyObject, Sendable {
    func fileExists(_ path: String) -> Bool
    func createDirectory(_ path: String, intermediate: Bool, posixMode: Int) throws
    func copyItem(at: String, to: String) throws
    func moveItem(at: String, to: String) throws
    func removeItem(at: String) throws
    func contents(_ path: String) throws -> Data
    func sha256Hex(_ path: String) throws -> String
    func setPosixMode(_ mode: Int, at path: String) throws
    /// symlink 目标；非 symlink 或不存在 → nil（PATH 项接管声明判据用）。
    func symlinkDestination(_ path: String) -> String?
    /// bundle 的 CFBundleShortVersionString（缺失/损坏 → nil → 版本视为不同）。
    func appShortVersion(atBundlePath: String) -> String?
}

/// 生产 FS 实现（FileManager 直映射）。copy/move 先清既有目标：Foundation 二者
/// 均不覆盖同名目标，而安装流程的落位语义是覆盖（重跑=幂等刷新）。
final class FileManagerInstallerFS: InstallerFS {
    func fileExists(_ path: String) -> Bool {
        FileManager.default.fileExists(atPath: path)
    }

    func createDirectory(_ path: String, intermediate: Bool, posixMode: Int) throws {
        try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: intermediate,
                                                attributes: [.posixPermissions: posixMode])
    }

    func copyItem(at: String, to: String) throws {
        if fileExists(to) { try FileManager.default.removeItem(atPath: to) }
        try FileManager.default.copyItem(atPath: at, toPath: to)
    }

    func moveItem(at: String, to: String) throws {
        if fileExists(to) { try FileManager.default.removeItem(atPath: to) }
        try FileManager.default.moveItem(atPath: at, toPath: to)
    }

    func removeItem(at: String) throws {
        try FileManager.default.removeItem(atPath: at)
    }

    func contents(_ path: String) throws -> Data {
        try Data(contentsOf: URL(fileURLWithPath: path))
    }

    func sha256Hex(_ path: String) throws -> String {
        InstallerSupport.sha256Hex(try contents(path))
    }

    func setPosixMode(_ mode: Int, at path: String) throws {
        try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: path)
    }

    func symlinkDestination(_ path: String) -> String? {
        try? FileManager.default.destinationOfSymbolicLink(atPath: path)
    }

    func appShortVersion(atBundlePath path: String) -> String? {
        Bundle(url: URL(fileURLWithPath: path))?
            .object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
    }
}

/// 子进程执行结果（exit code/stdout/stderr 原文；失败诊断需要 stderr 摘要）。
struct InstallerRunResult {
    let exitCode: Int32
    let stdout: String
    let stderr: String
}

/// 子进程缝：生产起真进程（osascript/xattr）；单测 fake 只记录 argv/stdin，
/// 绝不真跑 osascript/xattr/codesign/hdiutil（只测脚本串构造与 argv 形态）。
protocol InstallerRunner: AnyObject, Sendable {
    /// osascript 契约：argv 恰为 ["osascript"]，脚本文本经 stdin 交付。
    func run(argv: [String], stdin: String?) -> InstallerRunResult
}

/// 生产实现（同步等待；osascript 授权弹窗等待时长由用户交互决定，安装器实例
/// 短命不设超时上限——与 tun_install.go 先例同款权衡）。工具名取 argv[0]
/// （osascript/xattr 均驻 /usr/bin）。步体在后台任务调用（评审必修 2），不占主线程。
final class ProcessInstallerRunner: InstallerRunner {
    func run(argv: [String], stdin: String?) -> InstallerRunResult {
        guard let tool = argv.first else {
            return InstallerRunResult(exitCode: 127, stdout: "", stderr: "argv 为空")
        }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/" + tool)
        process.arguments = Array(argv.dropFirst())
        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe
        do {
            if let stdin, !stdin.isEmpty {
                let inputPipe = Pipe()
                process.standardInput = inputPipe
                try process.run()
                // 脚本文本写完即关写侧（stdin 交付：不落盘、不进 argv；量小于管道
                // 缓冲，同步写不会死锁）
                try? inputPipe.fileHandleForWriting.write(contentsOf: Data(stdin.utf8))
                inputPipe.fileHandleForWriting.closeFile()
            } else {
                process.standardInput = FileHandle.nullDevice
                try process.run()
            }
        } catch {
            return InstallerRunResult(exitCode: 127, stdout: "", stderr: "进程启动失败：\(tool)")
        }
        // 先读尽管道再等退出（评审顺手修 1）：waitUntilExit 在前时，子进程写满
        // 管道缓冲会与本次读取互等死锁；readDataToEndOfFile 阻塞到 EOF——EOF 由
        // 进程退出自然到达（osascript/xattr 输出量小，顺序读尽两管道无风险）。
        let stdoutData = stdoutPipe.fileHandleForReading.readDataToEndOfFile()
        let stderrData = stderrPipe.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        return InstallerRunResult(
            exitCode: process.terminationStatus,
            stdout: String(data: stdoutData, encoding: .utf8) ?? "",
            stderr: String(data: stderrData, encoding: .utf8) ?? "")
    }
}

/// 步 5 验签生产实现：SecStaticCodeCheckValidity 基本校验（自签有效即过——
/// 本项目不做 notarization，DESIGN §7.7）；失败返回错误供状态机中止（不拉起坏副本）。
enum InstallerSignature {
    static func validate(_ path: String) -> Error? {
        var code: SecStaticCode?
        let createStatus = SecStaticCodeCreateWithPath(URL(fileURLWithPath: path) as CFURL,
                                                       SecCSFlags(), &code)
        guard createStatus == errSecSuccess, let staticCode = code else {
            return InstallerError.stepFailure(step: .app, reason: "无法创建静态码对象（\(createStatus)）")
        }
        let status = SecStaticCodeCheckValidity(staticCode, SecCSFlags(), nil)
        return status == errSecSuccess
            ? nil
            : InstallerError.stepFailure(step: .app, reason: "签名校验失败（SecStaticCodeCheckValidity \(status)）")
    }
}

/// 安装器错误：needsAppQuit=步 5 目标 app 在跑（等用户「退出并继续」点击授权）；
/// stepFailure=步号+用户可读原因（重试从步 1 重跑，全幂等）。
enum InstallerError: Error {
    case needsAppQuit
    case stepFailure(step: InstallerModel.Step, reason: String)
}
