import SwiftUI

/// ② 当前节点卡（旗帜 + 节点名/链式节点 + 延迟徽标 + 组名 + 出站模式 seg）。
/// v0.4.10 链式展示（方案 B，2026-11-19 用户拍板）：当前节点是嵌套组时沿 now 下探，
/// 完整呈现 组A [组] → Auto → 🇭🇰尾节点 + 尾节点延迟——旧嵌套组口径「Auto [组] —」
/// 末端信息缺失，废止；当前节点为普通节点时维持单节点形态（v0.4.4 口径）。
struct CurrentNodeCard: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                if let chain = nodeChain {
                    chainView(chain)
                } else if let name = currentNodeName {
                    // v0.4.4：当前节点名是组 id（嵌套组）时不渲染旗帜——组无旗帜语义
                    // （🌐 误导），名称后「组」徽标标明类型（metacubexd 同类信息位）；
                    // v0.4.10 起嵌套组态改走链式分支，此处仅承接普通节点
                    if !isNestedGroup(name, in: model.groups) {
                        Text(CountryFlag.flag(for: name))
                            .font(.system(size: 16)) // 旗帜 16pt（M3-plan §3.2）
                    }
                    Text(name)
                        .font(.system(size: 14, weight: .semibold))
                    if isNestedGroup(name, in: model.groups) {
                        BadgeView(text: "组")
                    }
                } else {
                    Text("—")
                        .font(.system(size: 14, weight: .semibold))
                }
                delayBadge
                Spacer()
                // 组名展示形态（v0.4.15 与 chips 同口径：icon+demo 中文名）；"当前组"
                // 前缀语义由位置表达（M3-plan §3.2）
                Text(currentGroupName ?? "—")
                    .font(.system(size: Theme.fontSizeSub))
                    .foregroundStyle(Theme.muted(scheme))
            }
            .lineLimit(1) // 整行截断（现口径）
            HStack {
                Text("出站模式")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.secondary(scheme))
                Spacer()
                // 自绘分段（批①.5 §3.3，demo .seg；去固定宽 168 改自然宽度，demo 无 flex）
                SegmentedPill(
                    options: [("rule", "规则"), ("global", "全局"), ("direct", "直连")],
                    selection: modeBinding
                )
                .disabled(!model.running || model.busy)
            }
        }
        .cardStyle()
    }

    /// 链式形态：首位=当前组名（组不带旗帜、带「组」徽标），中间嵌套组名以
    /// 三级「 → 」衔接（Text(" → ") tertiary），尾节点追加旗帜；延迟徽标由
    /// 行内 delayBadge 统一呈现（取 tailDelay），此处不重复。
    @ViewBuilder private func chainView(_ chain: (names: [String], tailDelay: Int?)) -> some View {
        if let head = chain.names.first {
            Text(head)
                .font(.system(size: 14, weight: .semibold))
            BadgeView(text: "组")
        }
        ForEach(chain.names.dropFirst(), id: \.self) { name in
            Text(" → ")
                .font(.system(size: Theme.fontSizeSub))
                .foregroundStyle(.tertiary)
            if name == chain.names.last, !isNestedGroup(name, in: model.groups) {
                // 尾节点：旗帜 + 名称（组名不带旗帜的口径只约束链上组名；
                // code-reviewer P3：深度截断/环致链尾落组时不渲染误导性兜底旗帜）
                Text(CountryFlag.flag(for: name))
                    .font(.system(size: 16))
            }
            Text(name)
                .font(.system(size: 14, weight: .semibold))
        }
    }

    /// 徽标延迟源：链式态取尾节点延迟（v0.4.10），单节点态取 currentNodeDelay。
    /// WHY 不用 map/?? 链：Optional.map 嵌套 Int?? 会令 nil 尾延迟误回落到
    /// currentNodeDelay，须显式分流保证链式态严格以 tailDelay 为准
    private var badgeDelay: Int? {
        guard let chain = nodeChain else { return model.currentNodeDelay }
        return chain.tailDelay
    }

    /// 延迟徽标：按延迟色阶着色（底色 16% + 字色同源，LatencyColor.scale 四档，
    /// 无记录显示"—"灰）。WHY 偏离 demo：demo .ms-chip 为固定绿，但 316ms 绿徽标
    /// 语义误导——用户 2026-11-19 拍板改延迟色阶（记验收偏差 #2，批①.5 §3.6）
    @ViewBuilder private var delayBadge: some View {
        if let delay = badgeDelay {
            let color = LatencyColor.scale(delay, scheme: scheme)
            Text("\(delay) ms")
                .font(.system(size: Theme.fontSizeSub, weight: .semibold))
                .monospacedDigit()
                .padding(.horizontal, 7)
                .padding(.vertical, 1)
                .background(color.opacity(0.16), in: RoundedRectangle(cornerRadius: 5))
                .foregroundStyle(color)
        } else {
            Text("—")
                .font(.system(size: Theme.fontSizeSub))
                .monospacedDigit()
                .foregroundStyle(Theme.muted(scheme))
        }
    }

    private var activeGroup: ProxyGroup? {
        model.groups.first { $0.id == model.activeGroupID } ?? model.groups.first
    }

    private var currentNodeName: String? { activeGroup?.currentNode }
    /// v0.4.15（审查 P2）：组名位与 demo 同口径用展示形态（"🚀 节点选择"，demo
    /// curGroup 即此形态）——chips/卡片双名不一致正是用户投诉的那类偏差；
    /// 选中/语义仍按 id（原名），映射只影响显示。
    private var currentGroupName: String? {
        guard let g = activeGroup else { return nil }
        return g.display.icon.isEmpty ? g.display.name : "\(g.display.icon) \(g.display.name)"
    }
    /// 链式解析数据路径（视图不自行下探，模型可单测）
    private var nodeChain: (names: [String], tailDelay: Int?)? { model.exitChain() }

    private var modeBinding: Binding<String> {
        Binding(
            get: { model.report?.mode ?? "rule" },
            set: { newValue in Task { await model.setMode(newValue) } }
        )
    }
}

/// ③ 开关组（系统代理可用；TUN/代理守护 M2 禁用态 + P2 徽标，demo 显式偏差已声明）。
struct ToggleGroupCard: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(spacing: 2) {
            ToggleRow(
                icon: "globe", label: "系统代理",
                note: "127.0.0.1:7897",
                isOn: Binding(
                    // v0.4.7 effective 口径：点击立即翻转（乐观值），不等 networksetup 回读
                    get: { model.effectiveSysProxy },
                    set: { newValue in Task { await model.setSysProxy(on: newValue) } }
                ),
                disabled: !model.running || model.busy
            )
            Divider().overlay(Theme.muted(scheme).opacity(0.2))
            ToggleRow(icon: "rectangle.on.rectangle", label: "TUN 模式", note: nil,
                      isOn: .constant(false), disabled: true, badge: "P2")
            Divider().overlay(Theme.muted(scheme).opacity(0.2))
            ToggleRow(icon: "shield.lefthalf.filled", label: "代理守护", note: nil,
                      isOn: .constant(false), disabled: true, badge: "P2")
        }
        .cardStyle()
    }
}

struct BadgeView: View {
    let text: String

    var body: some View {
        Text(text)
            .font(.system(size: 10, weight: .medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 1)
            .background(.quaternary, in: RoundedRectangle(cornerRadius: 4))
            .foregroundStyle(.tertiary)
    }
}

struct ToggleRow: View {
    let icon: String
    let label: String
    let note: String?
    @Binding var isOn: Bool
    let disabled: Bool
    var badge: String?

    var body: some View {
        HStack(spacing: 10) {
            Label {
                Text(label).font(.system(size: 12.5))
            } icon: {
                Image(systemName: icon)
                    .font(.system(size: 13))
                    .foregroundStyle(.secondary)
            }
            Spacer()
            if let note {
                Text(note)
                    .font(.system(size: Theme.fontSizeSub))
                    .monospacedDigit()
                    .foregroundStyle(.tertiary)
            }
            if let badge {
                BadgeView(text: badge)
            }
            // 自绘复刻 demo .switch（小号）；v0.4.3 缘由：原生 tint 在 NSPopover 首呈现
            // 竞态丢色（M3-plan §3.3.1 的"接受系统色"预案被真机证伪——macOS 26 无 tint
            // 即灰轨且首开必现，2026-11-19 用户截图实锤），自绘为确定性修复
            SwitchPill(isOn: $isOn, small: true, accessibilityLabel: label)
                .disabled(disabled)
        }
        .padding(.vertical, 7)
        .padding(.horizontal, 8)
    }
}

/// ⑤ 策略组：chips + 节点列表（延迟点三色阶 + ✓ 当前）+ 测速按钮。
struct GroupsCardView: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("策略组")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(Theme.secondary(scheme))
                Spacer()
                Button {
                    Task { await model.testLatency() }
                } label: {
                    Label("测速", systemImage: "bolt")
                        .font(.system(size: 11.5))
                }
                .controlSize(.small)
                .disabled(model.busy || model.groups.isEmpty)
            }
            if model.degraded {
                Text("面板数据不可用")
                    .font(.system(size: Theme.fontSizeSub))
                    .foregroundStyle(.tertiary)
            } else {
                GroupChips(model: model)
                NodeList(model: model)
            }
        }
        .cardStyle()
    }
}

private struct GroupChips: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 6) {
                ForEach(model.groups) { group in
                    Button {
                        model.activeGroupID = group.id
                    } label: {
                        // v0.4.15：pill 用 demo 语义展示形态（icon+中文名，如 "♻️ 自动选择"）；
                        // 选中态仍按 id（原名）判定，映射只影响显示不影响语义
                        Text(group.display.icon.isEmpty
                             ? group.display.name
                             : "\(group.display.icon) \(group.display.name)")
                            .font(.system(size: 12))
                            .padding(.horizontal, 11)
                            .padding(.vertical, 4)
                            // 选中态 brand（demo chips active 背景 --brand；M3-plan §3.3.2，
                            // 原 Color.accentColor 为系统蓝的 tokens 应用缺口）
                            .background(
                                Capsule().fill(group.id == model.activeGroupID
                                               ? AnyShapeStyle(Theme.brand(scheme))
                                               : AnyShapeStyle(.quaternary))
                            )
                            .foregroundStyle(group.id == model.activeGroupID ? Color.white : .secondary)
                    }
                    .buttonStyle(.plain)
                }
            }
        }
    }
}

/// 节点列表：延迟点三色阶（<100 ok / 100-299 warn / ≥300 bad / 未测灰，demo 设计规范）
/// + ✓ 当前（色阶口径同节点卡延迟徽标——批①.5 起两处统一按延迟分级）。
private struct NodeList: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    private var group: ProxyGroup? {
        model.groups.first { $0.id == model.activeGroupID } ?? model.groups.first
    }

    // 确定性高度（批①.5 §3.1）：≤5 行全显、>5 行固定 168 滚动——
    // maxHeight 在 NSPopover fittingSize 路径收缩为 0（M3-plan §2.2），判定逻辑见 NodeListLayout
    var body: some View {
        switch NodeListLayout.layout(forCount: group?.nodes.count ?? 0) {
        case .fitAll:
            rows
        case .fixedScroll:
            ScrollView(showsIndicators: true) {
                rows
            }
            .frame(height: NodeListLayout.scrollHeight)
        }
    }

    private var rows: some View {
        VStack(spacing: 2) {
            ForEach(group?.nodes ?? []) { node in
                NodeRow(node: node, selected: node.name == group?.currentNode) {
                    Task { await model.selectNode(node.name) }
                }
            }
        }
    }
}

private struct NodeRow: View {
    let node: ProxyGroup.Node
    let selected: Bool
    let onSelect: () -> Void
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        Button(action: onSelect) {
            HStack(spacing: 9) {
                Circle()
                    .fill(LatencyColor.scale(node.delay, scheme: scheme))
                    .frame(width: 8, height: 8)
                // WHY v0.4.4：嵌套组不渲染旗帜——CountryFlag 对组名无语义（🌐 误导），
                // 「组」徽标标明类型（metacubexd 同类信息位）；2026-11-19 拍板
                // Auto（url-test）组自身形态保留现状，本批只做组成员识别
                if !node.isGroup {
                    Text(CountryFlag.flag(for: node.name))
                        .font(.system(size: 12.5))
                        .lineLimit(1)
                }
                Text(node.name)
                    .font(.system(size: 12.5, weight: selected ? .semibold : .regular))
                    .lineLimit(1)
                    .foregroundStyle(Color.primary)
                if node.isGroup {
                    BadgeView(text: "组")
                }
                Spacer()
                Text(node.delay.map { "\($0) ms" } ?? "—")
                    .font(.system(size: 11.5))
                    .monospacedDigit()
                    .foregroundStyle(.tertiary)
                Image(systemName: "checkmark")
                    .font(.system(size: 12, weight: .bold))
                    .foregroundStyle(Theme.brand(scheme))
                    .opacity(selected ? 1 : 0)
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 7)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }
}

/// 嵌套组判定：成员名命中任一组 id 即为组（CurrentNodeCard 徽标位用；
/// NodeRow 直接读 node.isGroup 数据字段，无须 contains）
private func isNestedGroup(_ name: String, in groups: [ProxyGroup]) -> Bool {
    groups.contains { $0.id == name }
}

/// 延迟色阶（demo 设计规范面板的纯函数化）。
enum LatencyColor {
    static func scale(_ delay: Int?, scheme: ColorScheme) -> Color {
        guard let delay else { return Theme.muted(scheme) } // 未测/不可用 → 灰
        switch delay {
        case ..<100: return Theme.ok
        case ..<300: return Theme.warn
        default: return Theme.bad
        }
    }
}

/// ⑥ 底部（批①.5 §3.4，demo .pop-foot）：网页面板 / 设置… / ⏻ 退出；
/// 前两钮等宽（flex:1），⏻ 自然宽（flex:0）+ 危险红（.btn.danger）。
struct PopoverFootView: View {
    @ObservedObject var model: PanelModel

    var body: some View {
        HStack(spacing: 8) {
            FootButton(fillsWidth: true, action: { Task { await model.openPanel() } }) {
                Label("网页面板", systemImage: "globe")
            }
            FootButton(fillsWidth: true, action: {}) {
                // 设置…：功能归批②，保持禁用但视觉为正常按钮弱化态（文本 tertiary，非系统灰块）
                Text("设置…").foregroundStyle(.tertiary)
            }
            .disabled(true)
            .help("M3 交付：随原生设置窗口")
            // ⏻ 保留 SF Symbol "power"（偏差 #3：demo 文本字形 ⏻ 弃用防字体回退 tofu，观感等价）
            FootButton(horizontalPadding: 10, action: { Task { await model.stopCoreAndQuit() } }) {
                Image(systemName: "power").foregroundStyle(Theme.bad)
            }
            .help("停止内核并还原系统代理，然后退出壳")
        }
        .padding(.top, 12)
    }
}
