import Foundation

/// CLI 进程执行抽象（测试注入 fake，绝不真拉 tradewind 进程）。
protocol CommandLineRunning {
    /// 执行 CLI 子命令并返回 stdout；tolerate 中的非零退出码不算失败
    /// （status 的退出码契约 0/1：1=内核异常但 JSON 仍有效，M2-plan §2.5）。
    func run(_ args: [String], tolerate: Set<Int32>) throws -> String
}

extension CommandLineRunning {
    func run(_ args: [String]) throws -> String {
        try run(args, tolerate: [])
    }
}

/// tradewind CLI 的路径解析：环境变量显式指定优先，其次探测常见安装位置
/// （os.Executable 语义对齐：发版位置不定，M2-plan §2.3 同理）。
enum CLIPath {
    static let envKey = "TRADEDWIND_CLI"

    static func resolve(environment: [String: String] = ProcessInfo.processInfo.environment,
                        fileExists: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }) -> String? {
        if let explicit = environment[envKey], !explicit.isEmpty, fileExists(explicit) {
            return explicit
        }
        for candidate in ["/opt/homebrew/bin/tradewind", "/usr/local/bin/tradewind"] where fileExists(candidate) {
            return candidate
        }
        return nil
    }
}

/// 真实 CLI 执行器：同步 Process + 有界等待（10s 超时后 SIGKILL，绝不挂死壳）。
struct CommandLineRunner: CommandLineRunning {
    let executablePath: String
    /// 控制面操作的安全上限；status/select 等本机 socket 操作毫秒级即可
    static let defaultTimeout: TimeInterval = 10

    func run(_ args: [String], tolerate: Set<Int32>) throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executablePath)
        process.arguments = args
        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr
        do {
            try process.run()
        } catch {
            throw CoreBridgeError.cliFailed(command: args.first ?? "tradewind")
        }
        let output = readAll(stdout, timeout: Self.defaultTimeout)
        let errText = readAll(stderr, timeout: Self.defaultTimeout)
        waitForExitOrKill(process, timeout: Self.defaultTimeout)
        if process.terminationStatus != 0 && !tolerate.contains(process.terminationStatus) {
            throw CoreBridgeError.cliFailed(command: args.first ?? "tradewind")
        }
        _ = errText // 错误信息不进壳日志/弹窗：可能含本机路径（错误信息不泄露内部细节）
        return output
    }

    // MARK: 进程 IO 与生命周期（私有，函数 ≤50 行）

    /// 有界读管道：到点返回已读内容（status 输出为单行 JSON，量小即时）。
    private func readAll(_ pipe: Pipe, timeout: TimeInterval) -> String {
        let handle = pipe.fileHandleForReading
        let deadline = Date().addingTimeInterval(timeout)
        var data = Data()
        while Date() < deadline {
            let chunk = handle.availableData
            if chunk.isEmpty { break } // EOF
            data.append(chunk)
            if data.count > 1 << 20 { break } // 控制面输出不应超 1MB，防御异常输出
        }
        return String(data: data, encoding: .utf8) ?? ""
    }

    private func waitForExitOrKill(_ process: Process, timeout: TimeInterval) {
        let deadline = Date().addingTimeInterval(timeout)
        while process.isRunning && Date() < deadline {
            usleep(20_000)
        }
        if process.isRunning {
            process.terminate()
        }
        process.waitUntilExit()
    }
}

/// 控制面桥（M2-plan §2.1）：壳全部启停/切换/模式操作走 CLI 子进程，
/// 全部机器可读输出；壳不解析人读文本。
final class CoreBridge {
    private let runner: CommandLineRunning

    init(runner: CommandLineRunning) {
        self.runner = runner
    }

    /// 默认桥：解析 CLI 路径失败显式报错（壳按"CLI 不可用"降级，控制面禁用）。
    static func live() throws -> CoreBridge {
        guard let path = CLIPath.resolve() else {
            throw CoreBridgeError.cliNotFound
        }
        return CoreBridge(runner: CommandLineRunner(executablePath: path))
    }

    /// status --json；exit 1（内核异常）不视为进程错误——stdout JSON 仍是有效状态。
    func status() throws -> StatusReport {
        let output = try runner.run(["status", "--json"], tolerate: [1])
        return try StatusReport.decode(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// 网页面板入口 URL（panel --url 仅打印不开浏览器）。
    func panelURL() throws -> String {
        try runner.run(["panel", "--url"]).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// 切换节点（组名/节点名含 emoji 由 CLI 侧 PathEscape 处理，原样传参即可）。
    func select(group: String, node: String) throws {
        try runner.run(["select", group, node])
    }

    /// 出站模式热切换（rule|global|direct；非法值由 CLI 校验并报错）。
    func setMode(_ mode: String) throws {
        try runner.run(["mode", mode])
    }

    /// 系统代理开关（幂等语义由 CLI 保证）。
    func sysProxy(on: Bool) throws {
        try runner.run(["sysproxy", on ? "on" : "off"])
    }

    /// 停止内核（优雅管线含系统代理精确还原）。
    func stopCore() throws {
        try runner.run(["stop"])
    }

    /// 前台拉起内核（detached 子进程：壳退出不影响内核，M2-plan §2.1）。
    func startCoreDetached() throws {
        let process = Process()
        guard let path = CLIPath.resolve() else {
            throw CoreBridgeError.cliNotFound
        }
        process.executableURL = URL(fileURLWithPath: path)
        process.arguments = ["start"]
        // WHY detach + discard stdout：start 前台阻塞，壳是观察者不能等它
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        try process.run()
    }
}
