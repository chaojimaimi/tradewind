import Testing
@testable import TradewindKit

/// CountryFlag 四级匹配（M3-plan §3.1）：12 个规范样本 + 长度优先边界 + emoji 直通 + 兜底。
@Suite struct CountryFlagTests {
    /// §3.1 样本表逐条断言（真实订阅名 + 边界反例）
    @Test func mapsAllSpecSamples() {
        let samples: [(name: String, expected: String)] = [
            ("日本-TY-4-HY2-流量倍率:0.6", "🇯🇵"),    // 真实订阅名（中文区名）
            ("中国香港-IP LC-HK-4-流量倍率:1", "🇭🇰"),  // 真实订阅名（中文别名 + HK 两字码并存）
            ("🇭🇰 HK-01", "🇭🇰"),                      // emoji 直通（demo mock 形态）
            ("美国 GIGA-01", "🇺🇸"),                   // 中文区名
            ("US-01", "🇺🇸"),                          // 两字码整 token
            ("新西兰-01", "🇳🇿"),                       // 最长匹配优先（防"新"类前缀误匹配）
            ("jp-tokyo-01", "🇯🇵"),                    // 两字码不区分大小写整 token
            ("Tokyo-01", "🇯🇵"),                       // 英文城市名
            ("AUSTIN-01", "🌐"),                      // 反例：两字码禁子串（"US"）
            ("SINGAPORE-01", "🌐"),                   // 反例：两字码禁子串（"IN"）+ 城市名精确大小写
            ("印尼-01", "🇮🇩"),                         // 中文短形别名
            ("未知节点", "🌐"),                         // 兜底
        ]
        for sample in samples {
            #expect(CountryFlag.flag(for: sample.name) == sample.expected, "样本: \(sample.name)")
        }
    }

    @Test func chineseLongestMatchBeatsShorterAlias() {
        // "印度尼西亚" 含子串 "印度"——长度降序保证 →🇮🇩 而非 🇮🇳（§4 长度优先边界）
        #expect(CountryFlag.flag(for: "印度尼西亚-巴厘-01") == "🇮🇩")
        #expect(CountryFlag.flag(for: "印度-孟买-01") == "🇮🇳")
    }

    @Test func emojiPassthroughExtractsFlagAnywhereInName() {
        #expect(CountryFlag.flag(for: "节点 🇯🇵 东京 01") == "🇯🇵")
        #expect(CountryFlag.flag(for: "🇰🇷 Seoul-01") == "🇰🇷")
    }

    // MARK: v0.4.2 热修（2026-11-19 真机反馈）：流量单位两字码不再误判为国家码

    /// 真机样本："GB" 紧随含数字 token，属"数值+单位"语境 → 兜底 🌐 而非误判 🇬🇧
    @Test func trafficUnitAfterNumberYieldsGlobe() {
        #expect(CountryFlag.flag(for: "有效期2029-07-02, 剩余 96.17 GB") == "🌐")
    }

    /// 纯英文场景同口径：数字 + GB 单位 → 兜底 🌐
    @Test func trafficUnitAfterNumberYieldsGlobeInPureEnglish() {
        #expect(CountryFlag.flag(for: "Node 96.17 GB") == "🌐")
    }

    /// 单位表不误伤行首国家码：首 token 的 GB 无前驱（无"数字+码"语境）→ 仍正常命中
    @Test func leadingCountryCodeUnaffectedByUnitTable() {
        #expect(CountryFlag.flag(for: "GB-London-01") == "🇬🇧")
    }

    /// 跳过单位后城市名兜底仍生效（code-reviewer P3 固化口径）：GB 被跳过、London 城市名
    /// 精确大小写子串命中 → 🇬🇧；对照 "96 GB-01"（数字前驱+无城市名）落 🌐 系启发式固有代价
    @Test func skippedUnitStillFallsBackToCityName() {
        #expect(CountryFlag.flag(for: "96 GB-London-01") == "🇬🇧")
    }

    /// 既有行为回归：非单位两字码不受单位表影响
    @Test func ordinaryTwoLetterCodeStillMatches() {
        #expect(CountryFlag.flag(for: "US-01") == "🇺🇸")
    }

    // MARK: v0.4.7 旗帜映射扩充（2026-11-19 用户订阅实锤：欧洲中小国家区名/两字码）

    /// 真机订阅样本：「爱沙尼亚-EE-1」此前落兜底 🌐；扩充后中文区名与 EE 两字码双路径命中
    @Test func estoniaRealSubscriptionSampleResolves() {
        #expect(CountryFlag.flag(for: "爱沙尼亚-EE-1-流量倍率:1") == "🇪🇪")
        #expect(CountryFlag.flag(for: "EE-01") == "🇪🇪")
    }

    /// 中文区名优先于两字码：芬兰 + HK 并存时命中 🇫🇮（② 中文匹配先于 ③ 英文）
    @Test func chineseRegionBeatsTwoLetterCode() {
        #expect(CountryFlag.flag(for: "芬兰-HK-01") == "🇫🇮")
    }

    /// 既有样本回归：表格扩充不得改变既有命中（真实订阅名/两字码/反例/流量单位防误判）
    @Test func expandedTableKeepsExistingSamples() {
        #expect(CountryFlag.flag(for: "日本-TY-4-HY2-流量倍率:0.6") == "🇯🇵")
        #expect(CountryFlag.flag(for: "中国香港-IP LC-HK-4-流量倍率:1") == "🇭🇰")
        #expect(CountryFlag.flag(for: "US-01") == "🇺🇸")
        #expect(CountryFlag.flag(for: "AUSTIN-01") == "🌐")
        #expect(CountryFlag.flag(for: "有效期2029-07-02, 剩余 96.17 GB") == "🌐")
    }
}
