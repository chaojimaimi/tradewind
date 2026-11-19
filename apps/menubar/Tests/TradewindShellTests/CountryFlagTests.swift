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
}
