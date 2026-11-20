import Foundation
import Testing
@testable import TradewindKit

/// ConnectionsStore：快照解码健壮性（null/缺字段）/排序与 500 上限/搜索过滤/
/// 退避重连状态机（fake socket）/start-stop 生命周期（M3-plan §11.4 c4 计划）。
@Suite @MainActor struct ConnectionsStoreTests {
    /// c4 交付 fixture（真实 mihomo 全量快照条目形态；chains[0]=实际出口节点）
    private let fixtureJSON = #"""
    {"downloadTotal":1024000,"uploadTotal":204800,"connections":[{"id":"abc","metadata":{"network":"tcp","type":"HTTP","sourceIP":"127.0.0.1","sourcePort":"51024","host":"www.apple.com","destinationIP":"17.253.144.10","destinationPort":"443"},"upload":1234,"download":567890,"start":"2026-11-20T01:02:03Z","chains":["HK-1","Auto","Proxy"],"rule":"Match"}]}
    """#

    private func decode(_ json: String) throws -> ConnectionsSnapshot {
        try JSONDecoder().decode(ConnectionsSnapshot.self, from: Data(json.utf8))
    }

    /// 测试行构造（memberwise init；字段默认值与 fixture 同形）
    private func conn(id: String, start: String, host: String = "", sniffHost: String = "",
                      chains: [String] = ["N1", "G"], rule: String = "Match") -> Connection {
        Connection(id: id,
                   metadata: ConnectionMetadata(network: "tcp", type: "HTTP", sourceIP: "127.0.0.1",
                                                sourcePort: "51024", host: host, sniffHost: sniffHost,
                                                destinationIP: "17.253.144.10", destinationPort: "443"),
                   upload: 1234, download: 567_890, start: start, chains: chains, rule: rule)
    }

    private let stampFormatter = ISO8601DateFormatter()

    private func stamp(_ secondsFromBase: Int) -> String {
        stampFormatter.string(from: Date(timeIntervalSince1970: TimeInterval(1_742_700_000 + secondsFromBase)))
    }

    /// 轮询等待 MainActor 上的异步状态就绪（fake socket 立即抛错路径；上限 2s 防挂死）
    private func waitUntil(_ condition: () -> Bool) async -> Bool {
        let deadline = Date().addingTimeInterval(2)
        while Date() < deadline {
            if condition() { return true }
            try? await Task.sleep(nanoseconds: 10_000_000)
        }
        return condition()
    }

    // MARK: 快照解码（健壮性）

    @Test func decodeFixturePreservesChainsOrderAndFields() throws {
        let snapshot = try decode(fixtureJSON)
        #expect(snapshot.downloadTotal == 1_024_000)
        #expect(snapshot.uploadTotal == 204_800)
        let connection = try #require(snapshot.connections?.first)
        #expect(connection.id == "abc")
        #expect(connection.metadata.network == "tcp")
        #expect(connection.metadata.type == "HTTP")
        #expect(connection.metadata.sourceIP == "127.0.0.1")
        #expect(connection.metadata.sourcePort == "51024")
        #expect(connection.metadata.host == "www.apple.com")
        #expect(connection.metadata.sniffHost == "") // fixture 无 sniffHost 键 → 缺省 ""
        #expect(connection.metadata.destinationIP == "17.253.144.10")
        #expect(connection.metadata.destinationPort == "443")
        #expect(connection.upload == 1234)
        #expect(connection.download == 567_890)
        // 数组序原样保留（chains[0]=实际出口节点，末位=规则命中策略组）
        #expect(connection.chains == ["HK-1", "Auto", "Proxy"])
        #expect(connection.rule == "Match")
    }

    @Test func decodeToleratesMissingFields() throws {
        // 帧级缺键：totals 归 0、connections 归 nil（≠空表语义由 ingest 层处理）
        let empty = try decode(#"{}"#)
        #expect(empty.downloadTotal == 0)
        #expect(empty.uploadTotal == 0)
        #expect(empty.connections == nil)
        // 连接级缺键：metadata 整体缺省、数值归 0、start 不可解析
        let minimal = try decode(#"{"connections":[{"id":"x"}]}"#)
        let connection = try #require(minimal.connections?.first)
        #expect(connection.metadata == ConnectionMetadata())
        #expect(connection.upload == 0 && connection.download == 0)
        #expect(connection.chains.isEmpty && connection.rule.isEmpty)
        #expect(connection.startDate == nil)
    }

    @Test func parseStartToleratesISO8601Variants() {
        #expect(Connection.parseStart("2026-11-20T01:02:03Z") != nil) // fixture 整秒 Z 形态
        #expect(Connection.parseStart("2026-11-20T09:02:03.456+08:00") != nil) // mihomo 毫秒+偏移
        #expect(Connection.parseStart("garbage") == nil)
        #expect(Connection.parseStart("") == nil)
    }

    // MARK: 入库（排序 / 上限 / 全量快照替换）

    @Test func ingestSortsByStartDescending() throws {
        let store = ConnectionsStore(bridge: nil)
        store.ingest(try decode("""
        {"connections":[
          {"id":"old","start":"2026-11-20T01:00:00Z"},
          {"id":"new","start":"2026-11-20T03:00:00Z"},
          {"id":"mid","start":"2026-11-20T02:00:00Z"}
        ]}
        """))
        #expect(store.connections.map(\.id) == ["new", "mid", "old"]) // 最新在前
    }

    @Test func ingestCapsAtMaxConnectionsKeepingNewest() {
        let store = ConnectionsStore(bridge: nil)
        let items = (0..<600).map { index in
            Connection(id: "c\(index)", start: stamp(index)) // 时间随下标递增
        }
        store.ingest(ConnectionsSnapshot(connections: items))
        #expect(store.connections.count == ConnectionsStore.maxConnections)
        #expect(store.connections.first?.id == "c599") // start desc → 最新居首
        #expect(store.connections.last?.id == "c100") // 最旧 100 条被裁
    }

    @Test func nullConnectionsClearsWholeTable() {
        let store = ConnectionsStore(bridge: nil)
        store.ingest(ConnectionsSnapshot(connections: [conn(id: "a", start: stamp(0)),
                                                       conn(id: "b", start: stamp(1))]))
        #expect(store.connections.count == 2)
        // mihomo 零连接形态（connections=null）= 全量快照整表替换，绝不留陈旧行
        store.ingest(ConnectionsSnapshot(downloadTotal: 9, uploadTotal: 9, connections: nil))
        #expect(store.connections.isEmpty)
    }

    // MARK: 搜索过滤

    @Test func filteredMatchesHostSniffChainsRuleCaseInsensitive() {
        let items = [
            conn(id: "1", start: stamp(0), host: "www.apple.com"),
            conn(id: "2", start: stamp(1), sniffHost: "www.YouTube.com", rule: "Geoip"),
            conn(id: "3", start: stamp(2), chains: ["HK-1", "Auto"]),
        ]
        #expect(ConnectionsStore.filtered(items, query: " APPLE ").count == 1) // 空白容忍+大小写不敏感
        #expect(ConnectionsStore.filtered(items, query: "youtube").map(\.id) == ["2"]) // sniffHost
        #expect(ConnectionsStore.filtered(items, query: "hk-1").map(\.id) == ["3"]) // chains
        #expect(ConnectionsStore.filtered(items, query: "match").count == 2) // rule（1/3 默认 Match）
        #expect(ConnectionsStore.filtered(items, query: "geoip").map(\.id) == ["2"]) // rule 命中 2
        #expect(ConnectionsStore.filtered(items, query: "zzz-not-found").isEmpty)
        #expect(ConnectionsStore.filtered(items, query: "   ").count == 3) // 空查询全量返回
        let store = ConnectionsStore(bridge: nil)
        store.ingest(ConnectionsSnapshot(connections: items))
        #expect(store.filtered(query: "apple").map(\.id) == ["1"]) // 实例入口同谓词
    }

    // MARK: 退避重连状态机（fake socket，绝不真连网络）

    @Test func backoffSequenceCapsAtSixty() {
        // 指数退避 1→2→4→…→60 封顶（TrafficStore 先例同值）
        var value: TimeInterval = 1
        var sequence = [value]
        while value < ConnectionsStore.backoffCap {
            value = ConnectionsStore.nextBackoff(after: value)
            sequence.append(value)
        }
        #expect(sequence == [1, 2, 4, 8, 16, 32, 60])
        #expect(ConnectionsStore.nextBackoff(after: 60) == 60) // 顶格不再翻倍
    }

    @Test func startWithUnavailableGatewaySchedulesBackoffRetry() async {
        let store = ConnectionsStore(bridge: nil) // 桥缺失降级 → 网关恒不可用
        await store.start()
        #expect(store.running)
        #expect(!store.connected)
        #expect(store.receiveTask == nil) // 未建流
        #expect(store.backoff == 2) // 首败推进（1→2）
        #expect(store.reconnectTask != nil) // 已安排退避重试（内核恢复自愈）
        store.stop()
        store.socketDidFail()
        #expect(store.backoff == 2) // stop 后状态机冻结
        #expect(!store.running)
    }

    @Test func startWithFailingSocketAdvancesBackoffAndSchedulesReconnect() async {
        let socket = FakeSocket()
        let store = ConnectionsStore(
            bridge: nil,
            gatewayProvider: { GatewayClient(panelURL: "http://127.0.0.1:1/ui/testtoken/") },
            socketFactory: { _ in socket })
        await store.start()
        #expect(await waitUntil { !store.connected && store.backoff == 2 }) // 首败已发生
        #expect(socket.resumed)
        #expect(store.reconnectTask != nil)
        // 直调推进状态机（免等退避定时）：2→4→8…封顶 60
        store.socketDidFail()
        #expect(store.backoff == 4)
        store.socketDidFail()
        #expect(store.backoff == 8)
        while store.backoff < ConnectionsStore.backoffCap {
            store.socketDidFail()
        }
        #expect(store.backoff == 60)
        store.stop()
    }

    @Test func startIsIdempotentWhileRunning() async {
        let counter = CallCounter()
        let store = ConnectionsStore(
            bridge: nil,
            gatewayProvider: { counter.increment(); return nil },
            socketFactory: { _ in FakeSocket() })
        await store.start()
        await store.start() // 重入被 running 守卫拒绝
        #expect(counter.value == 1) // 网关解析仅一次（未重置退避重来）
        store.stop()
    }

    @Test func stopCancelsSocketAndSuspendsLifecycle() async {
        let socket = FakeSocket()
        let store = ConnectionsStore(
            bridge: nil,
            gatewayProvider: { GatewayClient(panelURL: "http://127.0.0.1:1/ui/testtoken/") },
            socketFactory: { _ in socket })
        await store.start()
        _ = await waitUntil { store.backoff == 2 } // 首次断连已发生
        store.stop()
        #expect(!store.running)
        #expect(!store.connected)
        #expect(socket.cancelled) // stop 显式关闭 socket
        store.socketDidFail()
        #expect(store.backoff == 2) // 生命周期结束后状态机冻结
    }

    // MARK: WS 消息处理

    @Test func handleIgnoresMalformedFramesAndIngestsValid() throws {
        let store = ConnectionsStore(bridge: nil)
        store.handle(message: .string("not json"))
        #expect(!store.connected) // 非 JSON 帧忽略且不置建流标记
        store.handle(message: .data(Data("{}".utf8))) // data 帧忽略（mihomo 恒 text，防御面）
        #expect(!store.connected)
        store.handle(message: .string(#"{"downloadTotal":1,"uploadTotal":2}"#))
        #expect(store.connected) // 合法快照（缺 connections 键）→ 建流标记
        #expect(store.connections.isEmpty)
        store.handle(message: .string(fixtureJSON))
        #expect(store.connections.count == 1)
        #expect(store.connections.first?.id == "abc")
    }

    // MARK: 行展示逻辑（demo 口径）

    @Test func rowLogicDisplayTextsFollowDemo() throws {
        // 主机列：host 空 → sniffHost（mihomo 直连/无 SNI 常态的回退链）
        let withSniff = ConnectionMetadata(network: "tcp", type: "HTTP", sourceIP: "127.0.0.1",
                                           sourcePort: "51024", sniffHost: "cdn.example.com",
                                           destinationIP: "1.2.3.4", destinationPort: "443")
        #expect(ConnectionsRowLogic.hostText(withSniff) == "cdn.example.com")
        let direct = ConnectionMetadata(destinationIP: "1.2.3.4", destinationPort: "443")
        #expect(ConnectionsRowLogic.hostText(direct) == "1.2.3.4:443") // 全空回退 IP:port
        #expect(ConnectionsRowLogic.hostText(ConnectionMetadata()) == "—")
        #expect(ConnectionsRowLogic.nodeText(["HK-1", "Auto", "Proxy"]) == "HK-1") // chains[0]=实际出口
        #expect(ConnectionsRowLogic.nodeText([]) == "—")
        #expect(ConnectionsRowLogic.trafficText(download: 567_890, upload: 1_234_000) == "568 KB / 1.2 MB")
        let formatter = ConnectionsRowLogic.makeTimeFormatter()
        formatter.timeZone = TimeZone(identifier: "UTC") // 锚定时区（生产用本地时区）
        let date = try #require(Connection.parseStart("2026-11-20T01:02:03Z"))
        #expect(ConnectionsRowLogic.timeText(from: date, formatter: formatter) == "01:02")
        #expect(ConnectionsRowLogic.timeText(from: nil, formatter: formatter) == "—")
    }
}

// MARK: - fake socket 与计数器（文件私有）

/// 立即断开的 fake socket：receive 抛错驱动退避路径（壳层测试绝不真连网络）。
private final class FakeSocket: ConnectionsSocket {
    enum StubError: Error { case disconnected }
    private(set) var resumed = false
    private(set) var cancelled = false

    func receive() async throws -> URLSessionWebSocketTask.Message {
        throw StubError.disconnected
    }

    func resume() { resumed = true }

    func cancel(with closeCode: URLSessionWebSocketTask.CloseCode, reason: Data?) {
        cancelled = true
    }
}

/// 网关解析调用计数（start 幂等性断言；MainActor 串行访问无竞争）。
private final class CallCounter: @unchecked Sendable {
    private(set) var value = 0
    func increment() { value += 1 }
}
