import AppKit
import SwiftUI

// MARK: - 页 4 增强链（M3-plan §11.3：只读 P2 占位，demo :475-489 结构照录）

/// 增强链页：五槽位结构只读展示（全局 Merge.yaml/Script.js + 当前订阅
/// rules/proxies/groups），全部 P2 徽标；无任何写路径。
struct EnhancePane: View {
    /// 当前订阅名（status --json；节标题括号内展示，无订阅退化为通用标题）
    let profileName: String?
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            VStack(alignment: .leading, spacing: 4) {
                Text("增强链").font(.system(size: 17, weight: .bold))
                Text("五槽位增强：订阅文件只读，所有个性化覆盖在合成前叠加——订阅更新永不冲掉自定义。P2 交付，当前只读展示结构。")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.muted(scheme))
            }
            SettingsSection(title: "全局增强（作用于所有订阅）") {
                slot("Merge.yaml", "YAML 键级合并，最常用于追加公共分流")
                Divider().overlay(Theme.muted(scheme).opacity(0.2))
                slot("Script.js", "JS 函数 main(config) 任意变换")
            }
            SettingsSection(title: currentSectionTitle) {
                slot("rules", "规则追加 / 覆盖")
                Divider().overlay(Theme.muted(scheme).opacity(0.2))
                slot("proxies", "节点追加 / 覆盖")
                Divider().overlay(Theme.muted(scheme).opacity(0.2))
                slot("groups", "策略组追加 / 覆盖")
            }
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 20)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var currentSectionTitle: String {
        guard let profileName, !profileName.isEmpty else { return "当前订阅独立增强" }
        return "当前订阅独立增强（\(profileName)）"
    }

    /// 链槽位行（.chain-slot：粗体名+副说明 + P2 徽标右置）
    private func slot(_ name: String, _ desc: String) -> some View {
        SettingsRow(label: name, subtitle: desc, boldLabel: true) {
            BadgeView(text: "P2")
        }
    }
}

// MARK: - 页 5 关于（M3-plan §11.3：品牌 mark + 版本 + 内核 + Bundle ID + 仓库 + 许可 + 数据目录）

/// 关于页：品牌位图取 icns 128pt@2x 派生（256×256，与 Dock 图标同构；§12 c1 取代
/// v0.4.13 前"SF wind 压渐变方块"的早期近似——用户拍板品牌资产=深色底板+青渐变标形）。
/// WHY 显式 NSImage 加载（m3b2 返修实锤）：SwiftUI `Image(name, bundle:)` 只认
/// asset catalog 图像，对资源 bundle 里的散装 PNG 查不到（v0.5.0-m3b2 真机图标整块
/// 丢失）；Bundle.module.url + NSImage(contentsOf:) 是确定路径（AboutBrandTests 锁点）。
struct AboutPane: View {
    @ObservedObject var model: SettingsModel
    @Environment(\.colorScheme) private var scheme

    /// 品牌位图（进程级单次加载；资源缺失时 nil 走兜底块，绝不静默空渲染）
    private static let brandIcon: NSImage? = {
        guard let url = Bundle.module.url(forResource: "tradewind-about-icon",
                                          withExtension: "png") else { return nil }
        return NSImage(contentsOf: url)
    }()

    var body: some View {
        VStack(spacing: 0) {
            // 品牌 mark（demo .about-icon 84×84 圆角 20 观感）：真品牌位图源=icns
            // 128pt@2x 派生，与 Dock 图标同构。100pt = icns 画布自带 ~10% 留白的补偿
            // （alpha 包围盒实测 25-230/256），底板实高 ≈80pt ≈ demo 84pt 观感；shadow
            // 对照 demo .about-icon box-shadow 0 12px 30px（CSS blur 30 → radius 15、
            // y 偏移 12 直取）。兜底块=早期近似（资源不可能缺失，防御性可辨降级）。
            if let icon = Self.brandIcon {
                Image(nsImage: icon)
                    .resizable()
                    .frame(width: 100, height: 100)
                    .shadow(color: Theme.brand(scheme).opacity(0.35), radius: 15, x: 0, y: 12)
                    .padding(.bottom, 14)
            } else {
                RoundedRectangle(cornerRadius: 20)
                    .fill(LinearGradient(colors: [Theme.brandDeep(scheme), Theme.brand(scheme)],
                                         startPoint: .topLeading, endPoint: .bottomTrailing))
                    .frame(width: 100, height: 100)
                    .overlay { Image(systemName: "wind").font(.system(size: 40)).foregroundStyle(.white) }
                    .padding(.bottom, 14)
            }
            Text("Tradewind 信风")
                .font(.system(size: 19, weight: .bold))
            Text("v\(ShellVersion.display(shellVersion))")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
                .padding(.top, 2)
            VStack(spacing: 0) {
                kv(label: "代理内核", value: "mihomo \(model.coreVersion ?? "—")（外挂 · 锁版本）")
                kv(label: "Bundle ID", value: bundleID, monospaced: true)
                repoRow
                kv(label: "开源许可", value: "MIT")
                kv(label: "数据目录", value: "~/Library/Application Support/tradewind/", small: true)
            }
            .background(Theme.cardBackground(scheme))
            .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
            .overlay(
                RoundedRectangle(cornerRadius: Theme.radiusCard)
                    .strokeBorder(Color.primary.opacity(0.08), lineWidth: 1)
            )
            .padding(.top, 18)
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 26)
        .frame(maxWidth: .infinity)
    }

    // MARK: 数据源

    /// 壳版本（发版注入 CFBundleShortVersionString；swift build 开发态无值落 dev）
    private var shellVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    }

    /// Bundle ID（真机 app bundle 由 Info.plist 提供；开发态落安装标识兜底）
    private var bundleID: String {
        Bundle.main.bundleIdentifier ?? "io.github.chaojimaimi.tradewind"
    }

    /// 源码仓库行（demo 品牌色 + 可点击外链）
    private var repoRow: some View {
        HStack {
            Text("源码仓库")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
            Spacer()
            // 字面常量 URL，构造失败仅会因输入非法（此处不可能）——强制解包安全
            Link("github.com/chaojimaimi/tradewind",
                 destination: URL(string: "https://github.com/chaojimaimi/tradewind")!)
                .font(.system(size: 12.5, weight: .medium))
                .foregroundStyle(Theme.brand(scheme))
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .overlay(Divider().padding(.leading, 12), alignment: .top)
        .overlay(Divider().padding(.leading, 12), alignment: .bottom)
    }

    /// 键值行（demo .kv：label muted 左置 + 值右置 550 字重；数据目录 11px 等宽）
    private func kv(label: String, value: String, monospaced: Bool = false, small: Bool = false) -> some View {
        HStack {
            Text(label)
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
            Spacer()
            Text(value)
                .font(monospaced || small ? .system(size: small ? 11 : 12.5).monospaced()
                                          : .system(size: 12.5, weight: .medium))
                .minimumScaleFactor(0.7)
                .lineLimit(1)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .overlay(Divider().padding(.leading, 12), alignment: .top)
    }
}
