import Foundation

/// CLI 进程执行抽象（测试注入 fake，绝不真拉 tradewind 进程）。
protocol CommandLineRunning {
    /// 执行 CLI 子命令并返回 stdout；tolerate 中的非零退出码不算失败
    /// （status 的退出码契约 0/1：1=内核异常但 JSON 仍有效，M2-plan §2.5）。
    func run(_ args: [String], tolerate: Set<Int32>) async throws -> String
    /// 加长超时变体（M3-plan §11.2-4 评审 P1-3）：settings/autostart/subs 写编排
    /// 持 mutation.lock 最长 35s + reload 失败降级重启 15s，默认 10s SIGKILL
    /// 会制造半完成态。默认实现转发常规 run（fake 无进程可杀，超时仅真实执行器
    /// 需要语义；单测以覆写本方法断言注入的超时值）。
    func runWithTimeout(_ args: [String], timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String
    /// stdin 携带内容变体（批③ c6 enhance set：内容经 stdin 传 CLI，不经 argv）。
    /// 默认实现转发 runWithTimeout 并丢弃 input（fake 无进程；真实执行器覆写）。
    func runWithInput(_ args: [String], input: String, timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String
}

extension CommandLineRunning {
    func run(_ args: [String]) async throws -> String {
        try await run(args, tolerate: [])
    }

    func runWithTimeout(_ args: [String], timeout: TimeInterval) async throws -> String {
        try await runWithTimeout(args, timeout: timeout, tolerate: [])
    }

    /// 协议默认转发：调用方不感知实现差异（真实执行器覆写为有界等待）。
    func runWithTimeout(_ args: [String], timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        try await run(args, tolerate: tolerate)
    }

    /// stdin 变体默认转发（丢弃 input；真实执行器覆写为管道写入）。
    func runWithInput(_ args: [String], input: String, timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        try await runWithTimeout(args, timeout: timeout, tolerate: tolerate)
    }

    func runWithInput(_ args: [String], input: String, timeout: TimeInterval) async throws -> String {
        try await runWithInput(args, input: input, timeout: timeout, tolerate: [])
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
        // 批⑦ §3.6：DMG 安装器落位的数据目录候选（home 与 InstallerSupport.homeDir
        // 同源，TRADEDWIND_HOME 可覆盖）。homebrew 优先于数据目录——尊重 tarball
        // 用户显式安装与 self-update 通道。
        let dataDirCandidate = InstallerSupport.homeDir(environment: environment) + "/tradewind"
        return fileExists(dataDirCandidate) ? dataDirCandidate : nil
    }
}

/// 真实 CLI 执行器：同步 Process + 有界等待（10s 超时后 SIGKILL，绝不挂死壳）。
struct CommandLineRunner: CommandLineRunning {
    let executablePath: String
    /// 控制面操作的安全上限；status/select 等本机 socket 操作毫秒级即可
    static let defaultTimeout: TimeInterval = 10

    /// async 包装：fork/exec + 忙等整体卸到后台线程（WHY v0.4.6，2026-11-19 用户反馈
    /// 点开 popover/开关 sysproxy 明显卡顿根治——调用方全在 @MainActor，同步实现曾把
    /// status 的 ~50-200ms 与 networksetup 的数百 ms 全压在主线程忙等）。
    /// struct 值类型捕获进 @Sendable 闭包安全（仅含 String 常量）。
    func run(_ args: [String], tolerate: Set<Int32>) async throws -> String {
        try await Task.detached { try self.runBlocking(args, tolerate: tolerate, timeout: Self.defaultTimeout) }.value
    }

    /// 加长超时路径（M3-plan §11.2-4）：整体等待上限由调用方传入而非 defaultTimeout。
    func runWithTimeout(_ args: [String], timeout: TimeInterval, tolerate: Set<Int32>) async throws -> String {
        try await Task.detached { try self.runBlocking(args, tolerate: tolerate, timeout: timeout) }.value
    }

    /// stdin 携带内容变体（批③ c6 enhance set：内容经 stdin 传 CLI，不经 argv——
    /// ps 可见面为零）。整体卸后台线程，与 run 同款有界等待。
    func runWithInput(_ args: [String], input: String, timeout: TimeInterval,
                      tolerate: Set<Int32>) async throws -> String {
        try await Task.detached {
            try self.runBlocking(args, tolerate: tolerate, timeout: timeout, stdin: input)
        }.value
    }

    /// 同步实现体（仅经 detached 任务在后台线程执行，非隔离）。
    /// stdin 非空时经管道写入（异步派发写侧：大内容不阻塞读侧，防管道缓冲死锁）。
    private func runBlocking(_ args: [String], tolerate: Set<Int32>, timeout: TimeInterval,
                             stdin: String? = nil) throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executablePath)
        process.arguments = args
        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr
        var inputHandle: FileHandle?
        var stdinData: Data?
        if let stdin, !stdin.isEmpty {
            let inputPipe = Pipe()
            process.standardInput = inputPipe
            stdinData = Data(stdin.utf8)
            inputHandle = inputPipe.fileHandleForWriting
        }
        do {
            try process.run()
        } catch {
            throw CoreBridgeError.cliFailed(command: args.first ?? "tradewind")
        }
        if let inputHandle, let data = stdinData {
            // 异步派发写侧：进程读侧未消费时大内容不阻塞主读循环（管道缓冲死锁防御）。
            // WHY try?：非 throwing write(_:) 遇 EPIPE（进程早退已关读侧，典型=
            // 65s 超时被 terminate）抛 NSFileHandleOperationException，GCD 闭包内
            // 未捕获 ObjC 异常直接终止 app（code-reviewer P2-3）
            DispatchQueue.global(qos: .userInitiated).async {
                try? inputHandle.write(contentsOf: data)
                inputHandle.closeFile()
            }
        }
        let output = readAll(stdout, timeout: timeout)
        let errText = readAll(stderr, timeout: timeout)
        try waitForExitOrKill(process, timeout: timeout, command: args.first ?? "tradewind")
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

    /// terminate() 发 SIGTERM（注释修正，c2 审查 P2：旧注释误写 SIGKILL——SIGTERM
    /// 对 CLI 的 mutation.lock flock 释放语义相同，进程死亡锁即解）。
    /// 超时抛 timedOut：写编排可能已部分生效（settings.yaml 先落盘后 reload），
    /// 消费方不得按普通失败回退乐观回显。
    private func waitForExitOrKill(_ process: Process, timeout: TimeInterval, command: String) throws {
        let deadline = Date().addingTimeInterval(timeout)
        while process.isRunning && Date() < deadline {
            usleep(20_000)
        }
        if process.isRunning {
            process.terminate()
            process.waitUntilExit()
            throw CoreBridgeError.timedOut(command: command)
        }
        process.waitUntilExit()
    }
}

/// 控制面桥（M2-plan §2.1）：壳全部启停/切换/模式操作走 CLI 子进程，
/// 全部机器可读输出；壳不解析人读文本。
final class CoreBridge {
    private let runner: CommandLineRunning
    /// detached start 动作注入点（审查 P3-3）：生产默认 Process spawn（见 spawnDetached），
    /// 单测替换为 no-op——"壳层单测绝不真拉 tradewind 进程"契约对纯 spawn 同样适用
    /// （真机装有 CLI 时，无缝隙的 start 测试会拉起用户真内核）。
    var startDetached: () throws -> Void

    init(runner: CommandLineRunning) {
        self.runner = runner
        self.startDetached = { try CoreBridge.spawnDetached() }
    }

    /// 默认桥：解析 CLI 路径失败显式报错（壳按"CLI 不可用"降级，控制面禁用）。
    static func live() throws -> CoreBridge {
        guard let path = CLIPath.resolve() else {
            throw CoreBridgeError.cliNotFound
        }
        return CoreBridge(runner: CommandLineRunner(executablePath: path))
    }

    /// status --json；exit 1（内核异常）不视为进程错误——stdout JSON 仍是有效状态。
    /// WHY async：CLI 子进程经 runner 卸后台，5s 周期探测不再阻塞主线程（v0.4.6）。
    func status() async throws -> StatusReport {
        let output = try await runner.run(["status", "--json"], tolerate: [1])
        return try StatusReport.decode(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// 网页面板入口 URL（panel --url 仅打印不开浏览器）。
    func panelURL() async throws -> String {
        try await runner.run(["panel", "--url"]).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// 切换节点（组名/节点名含 emoji 由 CLI 侧 PathEscape 处理，原样传参即可）。
    func select(group: String, node: String) async throws {
        _ = try await runner.run(["select", group, node])
    }

    /// 清除 URLTest/Fallback 组的手动固定节点，恢复自动测速选择（v0.4.16）。
    func unpin(group: String) async throws {
        _ = try await runner.run(["unpin", group])
    }

    /// 出站模式热切换（rule|global|direct；非法值由 CLI 校验并报错）。
    func setMode(_ mode: String) async throws {
        _ = try await runner.run(["mode", mode])
    }

    /// 系统代理开关（幂等语义由 CLI 保证；networksetup 可达数百 ms，async 化主因之一）。
    /// WHY 65s write 档（批④终审 P2）：SysProxyOn/Off 已纳 mutation.lock（最坏等锁
    /// 35s），10s 档会把正常等待误判超时抛"可能已部分生效"。
    func sysProxy(on: Bool) async throws {
        _ = try await runner.runWithTimeout(["sysproxy", on ? "on" : "off"], timeout: Self.writeTimeout)
    }

    /// 停止内核（优雅管线含系统代理精确还原）。
    func stopCore() async throws {
        _ = try await runner.run(["stop"])
    }

    /// 组延迟测试（CLI delay [group]；有界并发 ≤8 由 CLI 保证，M1-plan §9）。
    func runDelay(group: String) async throws {
        _ = try await runner.run(["delay", group])
    }

    /// 前台拉起内核（detached 子进程：壳退出不影响内核，M2-plan §2.1）。
    /// WHY 保持同步：纯 spawn 不等待子进程（毫秒级返回），无主线程阻塞问题。
    func startCoreDetached() throws {
        try startDetached()
    }

    /// spawn 实现体（生产默认注入；静态私有：无实例状态，单测不可达）。
    private static func spawnDetached() throws {
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

    // MARK: 设置窗口桥（M3-plan §11.2-10：全部经 CLI 机器可读输出，壳不写 YAML）

    /// 写编排操作的加长超时（M3-plan §11.2-4 评审 P1-3/c2 审查 P2）：最坏路径
    /// 串行段 = 等锁 35s + 探测 2s + PUT 2s + 降级重启 15s + retarget 数秒 ≈ 60s+，
    /// 取 65s 留裕量（45s 会把壳 SIGTERM 在 retarget 前——半完成态正是本设计要消灭的）。
    static let writeTimeout: TimeInterval = 65

    /// TUN 编排超时（批⑥ §4）：90s——首装含提权弹窗等待用户输入 + 编排就绪判定
    /// 窗口 45s + 失败回滚重拉，65s write 档不够（settings set 65s 先例的加长档）。
    static let tunTimeout: TimeInterval = 90

    /// TUN 模式开关（批⑥ §4）：`tun on|off` 编排（含服务首装/就绪等待/失败回滚），
    /// 绝不经 settings set tun_enabled（Go 侧双入口封死）。
    func setTUN(enabled: Bool) async throws {
        _ = try await runner.runWithTimeout(["tun", enabled ? "on" : "off"], timeout: Self.tunTimeout)
    }

    /// `settings get --json`：通用页初始值数据源（读操作，常规 10s 足够）。
    func settingsGetJSON() async throws -> SettingsSnapshot {
        let output = try await runner.run(["settings", "get", "--json"])
        return try SettingsSnapshot.decode(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// `settings set <key> <value>`：65s 加长超时（写编排含热重载/降级重启链）。
    func settingsSet(key: String, value: String) async throws {
        _ = try await runner.runWithTimeout(["settings", "set", key, value], timeout: Self.writeTimeout)
    }

    /// `settings reset`：恢复默认，走同一写编排（mixed_port 定制时含 retarget 联动）。
    func settingsReset() async throws {
        _ = try await runner.runWithTimeout(["settings", "reset"], timeout: Self.writeTimeout)
    }

    /// `subs interval <target> <mode>`（c3 订阅页同用）；65s（持 mutation.lock）。
    func subsInterval(target: String, mode: String) async throws {
        _ = try await runner.runWithTimeout(["subs", "interval", target, mode], timeout: Self.writeTimeout)
    }

    /// 当前订阅的自动更新间隔写入（通用页 interval select 的动作）。
    /// WHY 壳侧定位 current：Go resolveProfile 仅按 uid/name 精确匹配（subs.go:170），
    /// 不支持 "current" 关键字（c1 实况）——经 status --json 的 profile.uid 解析
    /// （机器可读输出契约不变）；无当前订阅显式报错，不静默写错条目。
    func subsIntervalCurrent(mode: String) async throws {
        let report = try await status()
        guard !report.profile.uid.isEmpty else {
            throw CoreBridgeError.noCurrentProfile
        }
        try await subsInterval(target: report.profile.uid, mode: mode)
    }

    /// `autostart status --json`：开机自启开关初始值数据源（读操作）。
    func autostartStatusJSON() async throws -> AutostartStatus {
        let output = try await runner.run(["autostart", "status", "--json"])
        return try AutostartStatus.decode(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// `autostart on|off`：launchctl load/unload（on 含 manual 实例迁移 stop）。
    func autostart(on: Bool) async throws {
        _ = try await runner.runWithTimeout(["autostart", on ? "on" : "off"], timeout: Self.writeTimeout)
    }

    /// `list --json`：间隔 select 初值数据源（当前条目定位）。
    /// 完整订阅卡解码模型见 subsListJSON（c3）。
    func listJSON() async throws -> [ProfileIntervalEntry] {
        let output = try await runner.run(["list", "--json"])
        return try ProfileIntervalEntry.decodeArray(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    // MARK: 订阅管理页桥（M3-plan §11.3 页 1，c3；写操作全部走 65s writeTimeout）

    /// `list --json`（订阅卡全量数据源）：与 listJSON 同 CLI 读，解码模型不同
    /// （完整订阅卡 vs 通用页间隔初值裁剪——两模型并存系 SettingsModel 不动的约束）。
    func subsListJSON() async throws -> [SubsCardEntry] {
        let output = try await runner.run(["list", "--json"])
        return try SubsCardEntry.decodeArray(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// `add <url> [--name <name>]`：URL 导入（file 型导入不做 UI，仅 URL 路径）。
    /// WHY flags 前置（cmdAdd 实况核对）：Go flag 在首个非 flag 参数处停止解析——
    /// `add <url> --name X` 的 `--name X` 会被吞为位置参数，名称静默丢失。
    /// 名称空白则省略 --name（Go 默认取 URL host）。
    func subsAdd(url: String, name: String) async throws {
        var args = ["add"]
        if !name.isEmpty { args += ["--name", name] }
        args.append(url)
        _ = try await runner.runWithTimeout(args, timeout: Self.writeTimeout)
    }

    /// `update <target>`：拉取更新 + 合成 + 热重载（target=uid|name）。
    func subsUpdate(target: String) async throws {
        _ = try await runner.runWithTimeout(["update", target], timeout: Self.writeTimeout)
    }

    /// `use <target>`：切换当前订阅 + 合成 + 热重载。
    func subsUse(target: String) async throws {
        _ = try await runner.runWithTimeout(["use", target], timeout: Self.writeTimeout)
    }

    /// `subs delete <target> [--force]`：删除订阅；current 需 force（Go 侧保护，
    /// 删除后裸内核态语义与提示文案归 SubsModel）。WHY 位置参数在前（cmdSubs
    /// 实况核对）：fs.Parse(args[2:]) 先取 target 再解析余下 flag，`--force`
    /// 置于 target 前会因多余位置参数报错。
    func subsDelete(target: String, force: Bool) async throws {
        var args = ["subs", "delete", target]
        if force { args.append("--force") }
        _ = try await runner.runWithTimeout(args, timeout: Self.writeTimeout)
    }

    /// `subs rename <target> <new_name>`：订阅卡 ✎ 改名（纯索引写，Go 不触发
    /// reload）。WHY 与 cmdSubs rename 直取形态对应——rename 无 flag 项，壳侧
    /// 全位置参数传四元素数组，subsAdd 的 flags 前置教训不适用； newName 以
    /// "-" 开头也不会被误解析（Go 侧不经 flag.FlagSet）。
    func subsRename(target: String, name: String) async throws {
        _ = try await runner.runWithTimeout(["subs", "rename", target, name], timeout: Self.writeTimeout)
    }

    // MARK: 增强链桥（M3 批③ B2：双轨制 F11——壳一切经 CLI，绝不直接写槽文件）
    // 自升级桥已退役（batch5 §3.2 终审 P2-1 删除集）：AboutPane 更新区块改走
    // Sparkle（UpdateService）；CLI `version --check`/`self-update` 命令保留为
    // 守护/CLI 场景 fallback（DESIGN #22），壳内不再消费其 --json 输出。

    /// `enhance show <slot> --json`（读档；恒退出 0 契约，exists=false 是空转态非错误）。
    func enhanceShow(slot: String) async throws -> EnhanceSlotReport {
        let output = try await runner.run(["enhance", "show", slot, "--json"])
        return try EnhanceSlotReport.decode(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// `enhance set <slot> --file - --json`（65s 写档；内容经 stdin 传 CLI——
    /// 绝不经 argv/壳直接写文件，batch3 §3.3）。失败抛 cliMessage（error 字段文案）。
    func enhanceSet(slot: String, content: String) async throws {
        let output = try await runner.runWithInput(
            ["enhance", "set", slot, "--file", "-", "--json"],
            input: content, timeout: Self.writeTimeout)
        _ = try EnhanceOKReport.decode(output.trimmingCharacters(in: .whitespacesAndNewlines),
                                       label: "enhance set")
    }

    /// `enhance clear <slot> --json`（65s 写档，契约同 set）。
    func enhanceClear(slot: String) async throws {
        let output = try await runner.runWithTimeout(
            ["enhance", "clear", slot, "--json"], timeout: Self.writeTimeout)
        _ = try EnhanceOKReport.decode(output.trimmingCharacters(in: .whitespacesAndNewlines),
                                       label: "enhance clear")
    }
}
