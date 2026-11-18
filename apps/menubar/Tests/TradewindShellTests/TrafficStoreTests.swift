import Foundation
import SwiftUI
import Testing
@testable import TradewindKit

/// TrafficStore：退避序列 / 采样环形缓冲 / 今日累计 + 午夜重置 + 持久化 / 衰减归零。
@Suite @MainActor struct TrafficStoreTests {
    private func makeStore(now: @escaping () -> Date = Date.init) -> (TrafficStore, UserDefaults, String) {
        let suite = "test-traffic-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        return (TrafficStore(defaults: defaults, now: now), defaults, suite)
    }

    @Test func backoffSequenceCapsAtSixty() {
        // 指数退避 1→2→4→…→60 封顶（M2-plan §6：重连退避序列）
        var value: TimeInterval = 1
        var sequence: [TimeInterval] = [value]
        while value < TrafficStore.backoffCap {
            value = TrafficStore.nextBackoff(after: value)
            sequence.append(value)
        }
        #expect(sequence == [1, 2, 4, 8, 16, 32, 60])
        #expect(TrafficStore.nextBackoff(after: 60) == 60) // 顶格不再翻倍
        #expect(TrafficStore.nextBackoff(after: 40) == 60)
    }

    @Test func ingestAccumulatesAndCapsHistory() {
        let (store, _, suite) = makeStore()
        for i in 1...80 {
            store.ingest(down: Double(i) * 1000, up: Double(i) * 100)
        }
        #expect(store.history.count == TrafficStore.maxSamples) // 环形 60 点
        #expect(store.history.last?.down == 80_000)
        #expect(store.todayBytes > 0)
        #expect(store.down == 80_000 && store.up == 8_000)
        UserDefaults().removePersistentDomain(forName: suite)
    }

    @Test func todayBytesPersistAcrossInstances() {
        // 持久化：同 suite 新实例读取累计（壳重启续计，M2-plan §5）
        let suite = "test-traffic-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        let today = Date()
        let first = TrafficStore(defaults: defaults, now: { today })
        first.ingest(down: 1_000, up: 500)
        first.persistNow()
        let second = TrafficStore(defaults: defaults, now: { today })
        #expect(second.todayBytes == 1_500)
        UserDefaults().removePersistentDomain(forName: suite)
    }

    @Test func midnightResetClearsAndStartsFresh() {
        // 午夜重置：日期变化 → 今日清零，新一天采样从零起算（M2-plan §5）。
        // 单键存储只保"今日"账：昨日在 rollDay 瞬间被今日键覆写（需求无昨日保留面）。
        let suite = "test-traffic-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        let day1 = date(2026, 11, 18, 12)
        let day2 = date(2026, 11, 19, 0, 5) // 跨午夜 5 分钟
        var now = day1
        let store = TrafficStore(defaults: defaults, now: { now })
        store.ingest(down: 10_000, up: 1_000)
        #expect(store.todayBytes == 11_000)
        now = day2
        store.ingest(down: 20_000, up: 2_000) // day2 首条采样：rollDay 清零后计入新一天
        #expect(store.todayBytes == 22_000)
        store.persistNow()
        // 同为 day2 的新实例读到本日累计；day1 视角读不到（键被 day2 覆写，符合"只保今日"）
        let reloadedDay2 = TrafficStore(defaults: defaults, now: { day2 })
        #expect(reloadedDay2.todayBytes == 22_000)
        let reloadedDay1 = TrafficStore(defaults: defaults, now: { day1 })
        #expect(reloadedDay1.todayBytes == 0)
        UserDefaults().removePersistentDomain(forName: suite)
    }

    @Test func decayDrivesRatesToZeroInSteps() {
        let (store, _, suite) = makeStore()
        store.ingest(down: 1_000_000, up: 500_000)
        store.decayTick()
        #expect(store.down == 450_000) // 1e6 * 0.45
        #expect(store.up == 225_000)
        for _ in 0..<20 {
            store.decayTick()
        }
        #expect(store.down == 0 && store.up == 0) // 尾部干脆归零
        UserDefaults().removePersistentDomain(forName: suite)
    }

    @Test func latencyColorScaleFollowsDemoThresholds() {
        // demo 设计规范：<100 ok / 100-299 warn / ≥300 bad / 未测灰
        let dark = ColorScheme.dark
        #expect(LatencyColor.scale(42, scheme: dark) == Theme.ok)
        #expect(LatencyColor.scale(99, scheme: dark) == Theme.ok)
        #expect(LatencyColor.scale(150, scheme: dark) == Theme.warn)
        #expect(LatencyColor.scale(299, scheme: dark) == Theme.warn)
        #expect(LatencyColor.scale(300, scheme: dark) == Theme.bad)
        #expect(LatencyColor.scale(nil, scheme: dark) == Theme.muted(dark))
    }

    private func date(_ year: Int, _ month: Int, _ day: Int, _ hour: Int, _ minute: Int = 0) -> Date {
        var components = DateComponents()
        components.year = year
        components.month = month
        components.day = day
        components.hour = hour
        components.minute = minute
        return Calendar.current.date(from: components)!
    }
}
