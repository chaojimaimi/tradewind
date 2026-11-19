// swift-tools-version: 6.0
// Tradewind 菜单栏壳（M2-plan §10）：SPM，零第三方依赖（SwiftUI/AppKit 系统框架）。
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
    targets: [
        .target(
            name: "TradewindKit",
            path: "Sources/TradewindKit",
            // 菜单栏品牌图标（v0.4.10 logo 资产接入，2026-11-19 拍板）：16=@1x、32=@2x
            resources: [
                .copy("Resources/tradewind-menubar-16.png"),
                .copy("Resources/tradewind-menubar-32.png"),
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
