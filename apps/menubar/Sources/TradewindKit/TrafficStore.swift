import Combine
import Foundation

/// 迷你图单点采样（demo 60 点历史）。
struct TrafficSample: Identifiable, Equatable {
    let id: Int // 序号即时间序（ ring buffer 内自增）
    let down: Double
    let up: Double
}

/// 今日流量持久化载荷：日期键 + 累计字节（跨壳重启续计；午夜重置，M2-plan §5）。
struct TrafficDailyRecord: Codable, Equatable {
    let day: String
    let bytes: Double
}

/// WS /traffic 速率流（M2-plan §5）：指数退避重连（1→2→4→…上限 60s）；
/// 断线期间速率衰减归零（demo 排空语义）；今日流量壳内累计 + 持久化 + 午夜重置。
/// 显式限制：壳关闭期间欠计（mihomo 无聚合端点，方案已声明）。
@MainActor
final class TrafficStore: ObservableObject {
    static let maxSamples = 60
    static let backoffCap: TimeInterval = 60
    /// 断线后速率衰减系数（demo 无接入时更快排空：cur *= 1-0.55）
    static let decayFactor: Double = 0.45
    /// 衰减尾部归零阈值（demo 3e3）
    static let decayFloor: Double = 3_000

    @Published private(set) var down: Double = 0
    @Published private(set) var up: Double = 0
    @Published private(set) var history: [TrafficSample] = []
    @Published private(set) var todayBytes: Double = 0
    @Published private(set) var connected = false

    private var gateway: GatewayClient?
    private var receiveLoopTask: Task<Void, Never>?
    private var reconnectTask: Task<Void, Never>?
    private var webSocket: URLSessionWebSocketTask?
    private(set) var backoff: TimeInterval = 1
    private var sampleCounter = 0
    private var samplesSincePersist = 0
    private var currentDayKey = ""
    /// 注入点：测试用可控时钟
    private let now: () -> Date
    private let defaults: UserDefaults?

    init(defaults: UserDefaults? = .standard, now: @escaping () -> Date = Date.init) {
        self.defaults = defaults
        self.now = now
        loadPersisted()
    }

    deinit {
        receiveLoopTask?.cancel()
        reconnectTask?.cancel()
    }

    // MARK: 生命周期

    /// 网关变化（刷新周期内降级/恢复）：恢复 → 启动 WS；降级 → 断流 + 衰减归零。
    func attach(gateway: GatewayClient?) {
        self.gateway = gateway
        receiveLoopTask?.cancel()
        reconnectTask?.cancel()
        webSocket?.cancel(with: .normalClosure, reason: nil)
        connected = false
        guard gateway != nil else {
            startDecay()
            return
        }
        backoff = 1
        connect()
    }

    /// 主动断流（popover 关闭仍保持：壳常驻期间速率持续；仅壳退出时 deinit 取消）。
    func stop() {
        receiveLoopTask?.cancel()
        reconnectTask?.cancel()
        webSocket?.cancel(with: .normalClosure, reason: nil)
        connected = false
        persistNow()
    }

    // MARK: 连接与重连

    private func connect() {
        guard let gateway else { return }
        let task = gateway.webSocketTask(path: "traffic")
        webSocket = task
        task.resume()
        receiveLoopTask = Task { [weak self] in
            await self?.receiveLoop(task: task)
        }
    }

    private func scheduleReconnect() {
        guard let gateway else { return }
        let delay = backoff
        reconnectTask = Task { [weak self] in
            let nanoseconds = UInt64(delay * 1_000_000_000)
            try? await Task.sleep(nanoseconds: nanoseconds)
            guard !Task.isCancelled else { return }
            self?.connect()
        }
    }

    /// 指数退避序列（纯函数，可单测）：1→2→4→…封顶 60。
    static func nextBackoff(after current: TimeInterval) -> TimeInterval {
        min(current * 2, backoffCap)
    }

    // MARK: 接收与采样

    private func receiveLoop(task: URLSessionWebSocketTask) async {
        backoff = 1
        while !Task.isCancelled {
            do {
                let message = try await task.receive()
                handle(message: message)
            } catch {
                connected = false
                if !Task.isCancelled {
                    startDecay()
                    backoff = Self.nextBackoff(after: backoff)
                    scheduleReconnect()
                }
                return
            }
        }
    }

    /// 处理一条 WS 消息：{up, down} 每秒推送 ≈1s 字节量。
    private func handle(message: URLSessionWebSocketTask.Message) {
        guard case let .string(text) = message,
              let payload = try? JSONDecoder().decode(TrafficPayload.self, from: Data(text.utf8))
        else { return } // 非 JSON 心跳帧忽略（保持连接继续收）
        connected = true
        ingest(down: payload.down, up: payload.up)
    }

    /// 采样入库（WS 消息与测试共用）：历史环形 60 点 + 今日累计 + 午夜重置 + 节流落盘。
    func ingest(down newDown: Double, up newUp: Double) {
        down = newDown
        up = newUp
        sampleCounter += 1
        history.append(TrafficSample(id: sampleCounter, down: newDown, up: newUp))
        if history.count > Self.maxSamples {
            history.removeFirst(history.count - Self.maxSamples)
        }
        rollDayIfNeeded()
        todayBytes += newDown + newUp
        samplesSincePersist += 1
        if samplesSincePersist >= 15 {
            persistNow()
        }
    }

    /// 断线衰减一步（demo：更快排空 + 尾部干脆归零）。500ms 定时器驱动，测试直调。
    func decayTick() {
        down = decay(down)
        up = decay(up)
    }

    private func decay(_ value: Double) -> Double {
        let next = value * Self.decayFactor
        return next < Self.decayFloor ? 0 : next
    }

    /// 衰减定时器：断线时把余量排到 0（收到新样本即停）。
    private func startDecay() {
        // WHY 不用 Combine timer：500ms 轻量 Timer 足够，且 MainActor 上下文确定
        Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] timer in
            Task { @MainActor in
                guard let self else {
                    timer.invalidate()
                    return
                }
                if self.connected || (self.down == 0 && self.up == 0) {
                    timer.invalidate()
                    return
                }
                self.decayTick()
            }
        }
    }

    // MARK: 今日流量持久化（壳内累计；午夜重置）

    /// 午夜重置：日期键变化 → 清零（先落盘昨天的账，避免丢计）。
    private func rollDayIfNeeded() {
        let key = Self.dayKey(now())
        if currentDayKey.isEmpty {
            currentDayKey = key
            return // 首次加载：loadPersisted 已对齐
        }
        guard key != currentDayKey else { return }
        persistNow()
        todayBytes = 0
        currentDayKey = key
        persistNow()
    }

    static func dayKey(_ date: Date) -> String {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyy-MM-dd"
        formatter.timeZone = .current
        return formatter.string(from: date)
    }

    private func loadPersisted() {
        guard let defaults,
              let data = defaults.data(forKey: Self.storageKey),
              let record = try? JSONDecoder().decode(TrafficDailyRecord.self, from: data)
        else { return }
        currentDayKey = Self.dayKey(now())
        todayBytes = record.day == currentDayKey ? record.bytes : 0 // 跨天读取直接归零
    }

    func persistNow() {
        guard let defaults else { return }
        samplesSincePersist = 0
        let record = TrafficDailyRecord(day: currentDayKey.isEmpty ? Self.dayKey(now()) : currentDayKey,
                                        bytes: todayBytes)
        if let data = try? JSONEncoder().encode(record) {
            defaults.set(data, forKey: Self.storageKey)
        }
    }

    static let storageKey = "tradewind.shell.traffic.today"
}

/// /traffic 单条推送：{"up": bytes/s, "down": bytes/s}（mihomo 语义，行为参考 only）。
private struct TrafficPayload: Decodable {
    let up: Double
    let down: Double
}
