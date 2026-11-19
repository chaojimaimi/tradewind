import Foundation

/// 节点名 → 区域旗帜 emoji（M3-plan §3.1）。纯函数、零依赖。
/// demo mock 节点名自带旗帜前缀（`🇭🇰 HK-01`），真实订阅节点名不带 → 壳侧补齐。
///
/// 四级匹配：
/// ① emoji 直通：已含成对区域指示符（U+1F1E6–U+1F1FF）→ 原样提取使用
/// ② 中文区名最长匹配：表驱动按别名长度降序，防"印度尼西亚"因子串"印度"误判 🇮🇳
/// ③ 英文：两字码仅整 token 匹配（不区分大小写，禁子串——防 `AUSTIN-01` 因 "US"、
///    `SINGAPORE-01` 因 "IN" 误判）+ 英文城市名精确大小写子串匹配
/// ④ 兜底 🌐
///
/// 取舍：城市名匹配取精确大小写——§3.1 反例样本 `SINGAPORE-01`→🌐 是规范性验收契约，
/// 若城市名不区分大小写该样本必命中 Singapore→🇸🇬 与样本表冲突；两字码不受影响，
/// 保持不区分大小写（`jp-tokyo-01`→🇯🇵 样本依赖）。
/// "中国"不设主条目（§3.1 声明）：`中国香港`/`中国台湾`由 香港/台湾 别名覆盖，
/// 纯"中国"节点按命中规则落 🌐。
enum CountryFlag {
    /// ④ 兜底（地球）
    private static let globe = "🌐"

    // MARK: ② 中文区名表（§3.1 全表 + 短形别名 印尼/沙特，与全形并存）

    private static let chineseRegions: [(alias: String, flag: String)] = [
        ("新西兰", "🇳🇿"), ("印度尼西亚", "🇮🇩"), ("沙特阿拉伯", "🇸🇦"), ("哈萨克斯坦", "🇰🇿"),
        ("孟加拉国", "🇧🇩"), ("斯里兰卡", "🇱🇰"), ("马来西亚", "🇲🇾"), ("澳大利亚", "🇦🇺"),
        ("阿根廷", "🇦🇷"), ("土耳其", "🇹🇷"), ("蒙古", "🇲🇳"), ("荷兰", "🇳🇱"),
        ("菲律宾", "🇵🇭"), ("柬埔寨", "🇰🇭"), ("乌克兰", "🇺🇦"), ("波兰", "🇵🇱"),
        ("瑞典", "🇸🇪"), ("西班牙", "🇪🇸"), ("意大利", "🇮🇹"), ("爱尔兰", "🇮🇪"),
        ("瑞士", "🇨🇭"), ("巴西", "🇧🇷"), ("智利", "🇨🇱"), ("墨西哥", "🇲🇽"),
        ("泰国", "🇹🇭"), ("越南", "🇻🇳"), ("缅甸", "🇲🇲"), ("老挝", "🇱🇦"),
        ("埃及", "🇪🇬"), ("南非", "🇿🇦"), ("巴基斯坦", "🇵🇰"), ("尼泊尔", "🇳🇵"),
        ("以色列", "🇮🇱"), ("卡塔尔", "🇶🇦"), ("阿联酋", "🇦🇪"), ("尼日利亚", "🇳🇬"),
        ("香港", "🇭🇰"), ("台湾", "🇹🇼"), ("日本", "🇯🇵"), ("韩国", "🇰🇷"),
        ("新加坡", "🇸🇬"), ("美国", "🇺🇸"), ("英国", "🇬🇧"), ("德国", "🇩🇪"),
        ("法国", "🇫🇷"), ("加拿大", "🇨🇦"), ("俄罗斯", "🇷🇺"), ("印度", "🇮🇳"),
        ("印尼", "🇮🇩"), ("沙特", "🇸🇦"),
    ]

    /// 长度降序快照：最长匹配优先（"新西兰/新加坡/印度尼西亚"类前缀误匹配的防线）
    private static let sortedChineseRegions = chineseRegions.sorted { $0.alias.count > $1.alias.count }

    // MARK: ③-a 英文两字码（键全大写；查找前 uppercased → 整 token 不区分大小写）

    private static let twoLetterCodes: [String: String] = [
        "HK": "🇭🇰", "TW": "🇹🇼", "JP": "🇯🇵", "KR": "🇰🇷", "SG": "🇸🇬", "US": "🇺🇸",
        "UK": "🇬🇧", "GB": "🇬🇧", "DE": "🇩🇪", "FR": "🇫🇷", "CA": "🇨🇦", "AU": "🇦🇺",
        "RU": "🇷🇺", "IN": "🇮🇳", "TR": "🇹🇷", "NL": "🇳🇱", "MY": "🇲🇾", "TH": "🇹🇭",
        "VN": "🇻🇳", "PH": "🇵🇭", "ID": "🇮🇩", "AR": "🇦🇷", "BR": "🇧🇷", "CL": "🇨🇱",
        "MX": "🇲🇽", "SE": "🇸🇪", "ES": "🇪🇸", "IT": "🇮🇹", "IE": "🇮🇪", "CH": "🇨🇭",
        "UA": "🇺🇦", "PL": "🇵🇱", "MN": "🇲🇳", "KZ": "🇰🇿", "AE": "🇦🇪", "SA": "🇸🇦",
        "QA": "🇶🇦", "IL": "🇮🇱", "EG": "🇪🇬", "ZA": "🇿🇦", "BD": "🇧🇩", "PK": "🇵🇰",
        "LK": "🇱🇰", "NP": "🇳🇵", "MM": "🇲🇲", "KH": "🇰🇭", "LA": "🇱🇦", "NZ": "🇳🇿",
    ]

    // MARK: ③-b 英文城市名（§3.1 列举 + 常见扩展/带空格变体；精确大小写子串）

    private static let englishCities: [(name: String, flag: String)] = [
        ("Tokyo", "🇯🇵"), ("Osaka", "🇯🇵"), ("Seoul", "🇰🇷"), ("Busan", "🇰🇷"),
        ("Singapore", "🇸🇬"), ("Hongkong", "🇭🇰"), ("Hong Kong", "🇭🇰"),
        ("Taipei", "🇹🇼"), ("Kaohsiung", "🇹🇼"), ("London", "🇬🇧"), ("Manchester", "🇬🇧"),
        ("Frankfurt", "🇩🇪"), ("Hamburg", "🇩🇪"), ("Amsterdam", "🇳🇱"), ("Paris", "🇫🇷"),
        ("NewYork", "🇺🇸"), ("New York", "🇺🇸"), ("LosAngeles", "🇺🇸"), ("Los Angeles", "🇺🇸"),
        ("SanJose", "🇺🇸"), ("San Jose", "🇺🇸"), ("SiliconValley", "🇺🇸"),
        ("Chicago", "🇺🇸"), ("Seattle", "🇺🇸"), ("Dallas", "🇺🇸"), ("Phoenix", "🇺🇸"),
        ("Toronto", "🇨🇦"), ("Vancouver", "🇨🇦"), ("Sydney", "🇦🇺"), ("Melbourne", "🇦🇺"),
        ("Moscow", "🇷🇺"), ("Mumbai", "🇮🇳"), ("Istanbul", "🇹🇷"),
        ("KualaLumpur", "🇲🇾"), ("Kuala Lumpur", "🇲🇾"), ("Bangkok", "🇹🇭"),
        ("Hanoi", "🇻🇳"), ("HoChiMinh", "🇻🇳"), ("Manila", "🇵🇭"), ("Jakarta", "🇮🇩"),
        ("BuenosAires", "🇦🇷"), ("Buenos Aires", "🇦🇷"), ("SaoPaulo", "🇧🇷"), ("Sao Paulo", "🇧🇷"),
        ("Santiago", "🇨🇱"), ("Stockholm", "🇸🇪"), ("Madrid", "🇪🇸"), ("Barcelona", "🇪🇸"),
        ("Milan", "🇮🇹"), ("Rome", "🇮🇹"), ("Dublin", "🇮🇪"), ("Zurich", "🇨🇭"), ("Geneva", "🇨🇭"),
        ("Kyiv", "🇺🇦"), ("Warsaw", "🇵🇱"), ("Ulaanbaatar", "🇲🇳"), ("Almaty", "🇰🇿"),
        ("Dubai", "🇦🇪"), ("AbuDhabi", "🇦🇪"), ("Riyadh", "🇸🇦"), ("Jeddah", "🇸🇦"), ("Doha", "🇶🇦"),
        ("TelAviv", "🇮🇱"), ("Tel Aviv", "🇮🇱"), ("Cairo", "🇪🇬"),
        ("Johannesburg", "🇿🇦"), ("CapeTown", "🇿🇦"), ("Dhaka", "🇧🇩"), ("Karachi", "🇵🇰"),
        ("Colombo", "🇱🇰"), ("Kathmandu", "🇳🇵"), ("Yangon", "🇲🇲"), ("PhnomPenh", "🇰🇭"),
        ("Vientiane", "🇱🇦"), ("Auckland", "🇳🇿"), ("Wellington", "🇳🇿"),
    ]

    /// 长度降序快照：多词/复合名先于更短名比对，保证确定性
    private static let sortedEnglishCities = englishCities.sorted { $0.name.count > $1.name.count }

    /// 非字母数字段分隔（中文字符归入 alnum 段，但中文段不可能等于两字码，无害）
    private static let nonTokenCharacters = CharacterSet.alphanumerics.inverted

    // MARK: 四级匹配入口

    static func flag(for nodeName: String) -> String {
        if let flag = embeddedFlag(in: nodeName) { return flag }
        if let flag = chineseFlag(in: nodeName) { return flag }
        if let flag = englishFlag(in: nodeName) { return flag }
        return globe
    }

    /// ① emoji 直通：首个成对区域指示符（两连续 U+1F1E6–U+1F1FF 标量）
    private static func embeddedFlag(in nodeName: String) -> String? {
        var previous: Unicode.Scalar?
        for scalar in nodeName.unicodeScalars {
            if let prev = previous, isRegionalIndicator(prev), isRegionalIndicator(scalar) {
                return String(prev) + String(scalar)
            }
            previous = scalar
        }
        return nil
    }

    private static func isRegionalIndicator(_ scalar: Unicode.Scalar) -> Bool {
        (0x1F1E6...0x1F1FF).contains(scalar.value)
    }

    /// ② 中文区名最长匹配：别名长度降序遍历，首个被包含即命中
    private static func chineseFlag(in nodeName: String) -> String? {
        sortedChineseRegions.first { nodeName.contains($0.alias) }?.flag
    }

    /// ③ 英文：两字码整 token（禁子串）→ 城市名精确大小写子串（取舍见类型注释）
    private static func englishFlag(in nodeName: String) -> String? {
        // 两字码按"非字母数字"切段后整段比对：AUSTIN-01 切段为 AUSTIN/01，均非两字码
        for token in nodeName.uppercased().components(separatedBy: nonTokenCharacters) {
            if let flag = twoLetterCodes[token] { return flag }
        }
        return sortedEnglishCities.first { nodeName.contains($0.name) }?.flag
    }
}
