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

/// 关于页：品牌 mark 用 SF "wind" 白字压 brand 渐变圆角方块（demo .about-icon 结构；
/// Bundle 无 SVG 渲染能力，菜单栏 PNG 资产为 16/32 点阵放大发虚，故取矢量字形）。
struct AboutPane: View {
    @ObservedObject var model: SettingsModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(spacing: 0) {
            // demo .about-icon：84×84 圆角 20，brand-deep→brand 渐变 + 白色 46pt 字形
            RoundedRectangle(cornerRadius: 20)
                .fill(LinearGradient(
                    colors: [Theme.brandDeep(scheme), Theme.brand(scheme)],
                    startPoint: .topLeading, endPoint: .bottomTrailing))
                .frame(width: 84, height: 84)
                .overlay {
                    Image(systemName: "wind")
                        .font(.system(size: 40))
                        .foregroundStyle(.white)
                }
                .padding(.bottom, 14)
            Text("Tradewind 信风")
                .font(.system(size: 19, weight: .bold))
            Text("v\(shellVersion)")
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
