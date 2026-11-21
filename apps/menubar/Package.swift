// swift-tools-version: 6.0
// Tradewind 菜单栏壳（M2-plan §10）：SPM。零第三方依赖（SwiftUI/AppKit 系统框架）
// 的显式例外（DESIGN #22 预定的分发基建 + batch5 用户拍板）：Sparkle（app 自动更新，
// binaryTarget XCFramework，macOS 10.13+——壳 13.0 无碍）。
// WHY tools 6.0 + v5 语言模式：Swift Testing 随 tools 6.0 工具链原生可用；
// 语言模式保持 v5（方案 "Swift 5.9" 的语义基线），回避严格并发对 AppKit 回调的改造。
// WHY 库 target + 薄可执行：可执行 target 无法被测试 target 可靠导入（SPM 显式
// 模块构建限制），全部逻辑放 TradewindKit 库，TradewindShell 仅 3 行入口。
import PackageDescription

let package = Package(
    name: "TradewindShell",
    platforms: [
        .macOS(.v13)
    ],
    dependencies: [
        // 零依赖例外登记（F5）：Sparkle 是 DESIGN #22 预定的分发基建例外（batch5
        // §3.2 用户拍板）。exact: 钉死 2.9.1（评审 P3：2.7.3 含本地提权修复、
        // 2.9 增 feed 签名；exact: 非 exactVersion:——后者非法 API，终审 P3）。
        .package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.9.1"),
    ],
    targets: [
        .target(
            name: "TradewindKit",
            dependencies: ["Sparkle"],
            path: "Sources/TradewindKit",
            // 菜单栏品牌图标（v0.4.10 logo 资产接入，2026-11-19 拍板）：16=@1x、32=@2x
            // 关于页品牌图标（M3-plan §12 c1，2026-11-20）：icns icon_128x128@2x 派生 256px
            resources: [
                .copy("Resources/tradewind-menubar-16.png"),
                .copy("Resources/tradewind-menubar-32.png"),
                .copy("Resources/tradewind-about-icon.png"),
            ],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        .executableTarget(
            name: "TradewindShell",
            dependencies: ["TradewindKit"],
            path: "Sources/TradewindShell",
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        .testTarget(
            name: "TradewindShellTests",
            dependencies: ["TradewindKit"],
            path: "Tests/TradewindShellTests",
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
