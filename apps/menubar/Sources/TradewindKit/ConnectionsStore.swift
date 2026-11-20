import Combine
import Foundation

// MARK: - WS 任务最小面（M3-plan §11.2-3；CommandLineRunning 同款测试缝）

/// WS 任务抽象：生产 = GatewayClient.webSocketTask（显式 Cookie 头），
/// 测试 = fake socket 驱动退避/生命周期状态机（壳层测试绝不真连网络）。
protocol ConnectionsSocket: AnyObject {
    func receive() async throws -> URLSessionWebSocketTask.Message
    func resume()
    func cancel(with closeCode: URLSessionWebSocketTask.CloseCode, reason: Data?)
}

extension URLSessionWebSocketTask: ConnectionsSocket {}

// MARK: - /connections 全量快照解码模型（mihomo WS 每秒推全量；M3-plan §11.2-9）

/// 单连接元数据：字段全缺省容忍（decodeIfPresent ?? ""）——mihomo 直连 IP 时
/// host/sniffHost 常为空串或缺键，解码健壮性优先（c4 fixture 为唯一形态锚点）。
struct ConnectionMetadata: Decodable, Equatable {
    let network: String
    let type: String
    let sourceIP: String
    let sourcePort: String
    let host: String
    let sniffHost: String
    let destinationIP: String
    let destinationPort: String

    init(network: String = "", type: String = "", sourceIP: String = "", sourcePort: String = "",
         host: String = "", sniffHost: String = "", destinationIP: String = "",
         destinationPort: String = "") {
        self.network = network
        self.type = type
        self.sourceIP = sourceIP
        self.sourcePort = sourcePort
        self.host = host
        self.sniffHost = sniffHost
        self.destinationIP = destinationIP
        self.destinationPort = destinationPort
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            network: try container.decodeIfPresent(String.self, forKey: .network) ?? "",
            type: try container.decodeIfPresent(String.self, forKey: .type) ?? "",
            sourceIP: try container.decodeIfPresent(String.self, forKey: .sourceIP) ?? "",
            sourcePort: try container.decodeIfPresent(String.self, forKey: .sourcePort) ?? "",
            host: try container.decodeIfPresent(String.self, forKey: .host) ?? "",
            sniffHost: try container.decodeIfPresent(String.self, forKey: .sniffHost) ?? "",
            destinationIP: try container.decodeIfPresent(String.self, forKey: .destinationIP) ?? "",
            destinationPort: try container.decodeIfPresent(String.self, forKey: .destinationPort) ?? ""
        )
    }

    enum CodingKeys: String, CodingKey {
        case network, type, sourceIP, sourcePort, host, sniffHost, destinationIP, destinationPort
    }
}

/// 单连接（mihomo 全量快照条目；chains 原样保留——展示取首位的映射在
/// ConnectionsRowLogic，模型不掺展示语义）。
struct Connection: Decodable, Equatable, Identifiable {
    let id: String
    let metadata: ConnectionMetadata
    let upload: Double
    let download: Double
    /// mihomo ISO8601 原串（毫秒/时区偏移变体见 parseStart）
    let start: String
    let chains: [String]
    let rule: String
    /// start 解析结果（构造时算一次，免逐秒重复解析 500 行；nil=不可解析）
    let startDate: Date?

    init(id: String, metadata: ConnectionMetadata = ConnectionMetadata(),
         upload: Double = 0, download: Double = 0, start: String = "",
         chains: [String] = [], rule: String = "") {
        self.id = id
        self.metadata = metadata
        self.upload = upload
        self.download = download
        self.start = start
        self.chains = chains
        self.rule = rule
        self.startDate = Self.parseStart(start)
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            id: try container.decodeIfPresent(String.self, forKey: .id) ?? "",
            metadata: try container.decodeIfPresent(ConnectionMetadata.self, forKey: .metadata) ?? ConnectionMetadata(),
            upload: try container.decodeIfPresent(Double.self, forKey: .upload) ?? 0,
            download: try container.decodeIfPresent(Double.self, forKey: .download) ?? 0,
            start: try container.decodeIfPresent(String.self, forKey: .start) ?? "",
            chains: try container.decodeIfPresent([String].self, forKey: .chains) ?? [],
            rule: try container.decodeIfPresent(String.self, forKey: .rule) ?? ""
        )
    }

    enum CodingKeys: String, CodingKey {
        case id, metadata, upload, download, start, chains, rule
    }

    // MARK: start 解析

    /// ISO8601 容错解析：mihomo 实际输出带毫秒+时区偏移，c4 fixture 为整秒 Z 形态
    /// ——两种 formatter 依序尝试，均失败返回 nil（排序沉底、时间列显示 "—"）。
    /// WHY 静态共享：formatter 构造昂贵，逐连接逐秒新建不可接受；store 为
    /// MainActor 串行访问，无竞争。
    private static let fractionalFormatter: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter
    }()
    private static let plainFormatter = ISO8601DateFormatter()

    static func parseStart(_ raw: String) -> Date? {
        fractionalFormatter.date(from: raw) ?? plainFormatter.date(from: raw)
    }
}

/// /connections WS 帧整体。connections=null = 当前零连接（全量快照无差量语义，
/// 消费方必须整表替换，绝不保留上一秒的陈旧行）。
struct ConnectionsSnapshot: Decodable, Equatable {
    let downloadTotal: Double
    let uploadTotal: Double
    let connections: [Connection]?

    init(downloadTotal: Double = 0, uploadTotal: Double = 0, connections: [Connection]? = nil) {
        self.downloadTotal = downloadTotal
        self.uploadTotal = uploadTotal
        self.connections = connections
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            downloadTotal: try container.decodeIfPresent(Double.self, forKey: .downloadTotal) ?? 0,
            uploadTotal: try container.decodeIfPresent(Double.self, forKey: .uploadTotal) ?? 0,
            connections: try container.decodeIfPresent([Connection].self, forKey: .connections)
        )
    }

    enum CodingKeys: String, CodingKey {
        case downloadTotal, uploadTotal, connections
    }
}

// MARK: - 连接页数据中枢（M3-plan §11.2-3/9；TrafficStore 先例的平行实现）

/// WS /connections → @Published 快照（start desc 排序 + 500 上限）+ connected。
/// WHY 平行实现不抽公共基类：TrafficStore 挂壳常驻（衰减/今日累计/持久化），本
/// store 只随设置窗口可见期存活（start/stop 生命周期），职责面窄一档——复用仅限
/// 模式形态（退避序列/消息循环），不共享状态。两 WS 并行无冲突（网关按路径反代）。
@MainActor
final class ConnectionsStore: ObservableObject {
    /// 行上限（M3-plan §11.3 页 2；demo 表 26 行系演示数据量，真实上限按方案）
    static let maxConnections = 500
    static let backoffCap: TimeInterval = 60
    /// WS 路径（网关反代 + FlushInterval=-1；显式 Cookie 由 GatewayClient 注入）
    static let wsPath = "connections"

    /// 当前连接表（start desc；每秒全量快照整表替换，展示上限 500）
    @Published private(set) var connections: [Connection] = []
    /// 真实活跃连接数（截断前原始计数；统计卡语义，审查 P3-2）
    @Published private(set) var activeCount = 0
    @Published private(set) var connected = false
    /// 生命周期代际（审查 P2：stop/start 各 +1——connect 在 provider 挂起窗口内
    /// 经历 stop→start 时，旧 connect 的 `guard running` 会见到新 start 的 true 而
    /// 复活建流（双 socket 其一引用丢失不可取消）；代际比对使陈旧 connect 失效）
    private var generation = 0

    /// 网关解析入口（生产：settingsBridge.panelURL → GatewayClient；测试注入）
    typealias GatewayProvider = () async -> GatewayClient?
    /// WS 构造入口（生产：GatewayClient.webSocketTask；测试：fake socket）
    typealias SocketFactory = (GatewayClient) -> any ConnectionsSocket

    private let gatewayProvider: GatewayProvider
    private let socketFactory: SocketFactory
    private var socket: (any ConnectionsSocket)?
    private(set) var receiveTask: Task<Void, Never>?
    private(set) var reconnectTask: Task<Void, Never>?
    /// 生命周期开关（start/stop 幂等守卫；stop 后所有在途路径自检退出）
    private(set) var running = false
    /// 当前退避值（断连推进 1→2→4…60；成功建流重置 1）
    private(set) var backoff: TimeInterval = 1

    /// 生产构造：桥解析 panelURL（CLI status 唯一来源，PanelModel 同口径）→
    /// GatewayClient。桥 nil（CLI 缺失降级）时网关恒不可用，页面呈未连接态；
    /// gatewayProvider/socketFactory 仅供单测覆写（fake session 驱动状态机）。
    init(bridge: CoreBridge?, gatewayProvider: GatewayProvider? = nil,
         socketFactory: SocketFactory? = nil) {
        self.gatewayProvider = gatewayProvider ?? Self.bridgeProvider(bridge)
        self.socketFactory = socketFactory ?? { $0.webSocketTask(path: Self.wsPath) }
    }

    private static func bridgeProvider(_ bridge: CoreBridge?) -> GatewayProvider {
        guard let bridge else { return { nil } }
        return {
            guard let url = try? await bridge.panelURL() else { return nil }
            return GatewayClient(panelURL: url)
        }
    }

    deinit {
        receiveTask?.cancel()
        reconnectTask?.cancel()
    }

    // MARK: 生命周期（窗口层 showSettings/orderOut + 连接页 onAppear/onDisappear 双挂）

    /// 启动连接（幂等）：running 先置位，重入 start 直接返回；解析网关后建流。
    /// WHY async：panelURL 需 CLI 探测（数十至数百 ms），由调用方 Task 挂起。
    func start() async {
        guard !running else { return }
        generation += 1
        running = true
        backoff = 1
        await connect()
    }

    /// 主动断流（幂等）：取消任务与 socket；不清 connections（复开 1s 内被新快照替换）。
    func stop() {
        generation += 1
        running = false
        receiveTask?.cancel()
        reconnectTask?.cancel()
        socket?.cancel(with: .normalClosure, reason: nil)
        socket = nil
        connected = false
        // 审查 P3-1：断流即清表——内核已停时退避最长 60s 循环会让陈旧连接行与
        // "活跃 N / 0 B/s"矛盾呈现；复开 1s 内被新快照回填
        connections = []
    }

    // MARK: 连接与重连（TrafficStore.swift:96-130 先例同形）

    private func connect() async {
        guard running else { return }
        let capturedGeneration = generation
        let gateway = await gatewayProvider()
        // 代际比对（审查 P2）：provider 挂起窗口内经历 stop→start 的陈旧 connect
        // 直接失效（仅查 running 会见新 start 的 true 而复活建流）
        guard running, generation == capturedGeneration else { return }
        guard let gateway else {
            socketDidFail() // 网关不可用（内核停/CLI 缺失）：退避重试，恢复后自愈
            return
        }
        let socket = socketFactory(gateway)
        self.socket = socket
        socket.resume()
        receiveTask = Task { [weak self] in
            await self?.receiveLoop(socket: socket)
        }
    }

    private func scheduleReconnect() {
        guard running else { return }
        reconnectTask?.cancel() // 显式不变式：单活重连任务（审查 P3-3）
        let delay = backoff
        reconnectTask = Task { [weak self] in
            let nanoseconds = UInt64(delay * 1_000_000_000)
            try? await Task.sleep(nanoseconds: nanoseconds)
            guard !Task.isCancelled else { return }
            await self?.connect()
        }
    }

    /// 指数退避序列（纯函数，可单测）：1→2→4→…封顶 60（TrafficStore 同值同形）。
    static func nextBackoff(after current: TimeInterval) -> TimeInterval {
        min(current * 2, backoffCap)
    }

    /// 断连一步（接收循环 catch 与网关解析失败共用；单测直调驱动状态机）：
    /// connected=false + 退避推进 + 安排重连；stop 后静默退出（生命周期冻结）。
    func socketDidFail() {
        connected = false
        guard running else { return }
        backoff = Self.nextBackoff(after: backoff)
        scheduleReconnect()
    }

    // MARK: 接收与入库

    private func receiveLoop(socket: any ConnectionsSocket) async {
        backoff = 1 // 成功建流即重置退避（TrafficStore.receiveLoop 同款）
        while !Task.isCancelled {
            do {
                let message = try await socket.receive()
                handle(message: message)
            } catch {
                if !Task.isCancelled {
                    socketDidFail()
                }
                return
            }
        }
    }

    /// 处理一条 WS 消息：全量快照解码入库；非 JSON/解码失败帧忽略（保持连接继续收，
    /// TrafficStore.handle 同款容错）。
    func handle(message: URLSessionWebSocketTask.Message) {
        guard case let .string(text) = message,
              let payload = try? JSONDecoder().decode(ConnectionsSnapshot.self, from: Data(text.utf8))
        else { return }
        connected = true
        ingest(payload)
    }

    /// 快照入库（接收循环与单测共用）：start desc 排序 + 500 上限；
    /// connections=null 整表清空（全量快照语义）。500 为展示约束——
    /// activeCount 保留截断前原始计数（统计卡语义=真实活跃数，审查 P3-2）。
    func ingest(_ snapshot: ConnectionsSnapshot) {
        let sorted = Self.sorted(snapshot.connections ?? [])
        activeCount = sorted.count
        connections = Array(sorted.prefix(Self.maxConnections))
    }

    /// start 降序（最新在前，demo 表时间列语义）；解析失败沉底（distantPast）。
    static func sorted(_ items: [Connection]) -> [Connection] {
        items.sorted { ($0.startDate ?? .distantPast) > ($1.startDate ?? .distantPast) }
    }

    // MARK: 搜索

    /// 搜索过滤（纯函数，可单测）：host/sniffHost/chains/rule 大小写不敏感包含匹配；
    /// 空白查询全量返回。
    static func filtered(_ items: [Connection], query: String) -> [Connection] {
        let needle = query.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard !needle.isEmpty else { return items }
        return items.filter { $0.matches(needle: needle) }
    }

    /// 实例便捷入口（页面渲染读当前表）。
    func filtered(query: String) -> [Connection] {
        Self.filtered(connections, query: query)
    }
}

// MARK: - 单连接搜索匹配（ConnectionsStore.filtered 的谓词）

extension Connection {
    /// host/sniffHost/chains/rule 大小写不敏感包含匹配（needle 已小写化）。
    func matches(needle: String) -> Bool {
        metadata.host.lowercased().contains(needle)
            || metadata.sniffHost.lowercased().contains(needle)
            || chains.contains { $0.lowercased().contains(needle) }
            || rule.lowercased().contains(needle)
    }
}
