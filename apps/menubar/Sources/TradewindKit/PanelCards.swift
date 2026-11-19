import SwiftUI

/// ② 当前节点卡（旗帜 + 节点名 + 延迟徽标 + 组名 + 出站模式 seg）。
struct CurrentNodeCard: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                if let name = currentNodeName {
                    Text(CountryFlag.flag(for: name))
                        .font(.system(size: 16)) // 旗帜 16pt（M3-plan §3.2）
                    Text(name)
                        .font(.system(size: 14, weight: .semibold))
                        .lineLimit(1)
                } else {
                    Text("—")
                        .font(.system(size: 14, weight: .semibold))
                }
                delayBadge
                Spacer()
                // 组名原样显示（真实组名无 emoji）；"当前组"前缀语义由位置表达（M3-plan §3.2）
                Text(currentGroupName ?? "—")
                    .font(.system(size: Theme.fontSizeSub))
                    .foregroundStyle(Theme.muted(scheme))
                    .lineLimit(1)
            }
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

    /// 延迟徽标：按延迟色阶着色（底色 16% + 字色同源，LatencyColor.scale 四档，
    /// 无记录显示"—"灰）。WHY 偏离 demo：demo .ms-chip 为固定绿，但 316ms 绿徽标
    /// 语义误导——用户 2026-11-19 拍板改延迟色阶（记验收偏差 #2，批①.5 §3.6）
    @ViewBuilder private var delayBadge: some View {
        if let delay = model.currentNodeDelay {
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
    private var currentGroupName: String? { activeGroup?.name }

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
                    get: { model.report?.sysProxy.applied ?? false },
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
            // 品牌绿 on 态（demo `.switch input:checked` 背景 --ok）。
            // 取舍声明（M3-plan §3.3.1）：tint 对 macOS 13+ .switch 生效（Package.swift
            // 平台基线已核），若个别系统版本不生效则接受系统色并在验收记录中声明偏差
            // （不引入 controlAccent 全局副作用）。
            Toggle("", isOn: $isOn)
                .toggleStyle(.switch)
                .labelsHidden()
                .controlSize(.small)
                .tint(Theme.ok)
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
                        Text(group.name)
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
                // 旗帜注入（M3-plan §3.1 应用点位：每行节点名前；真实节点名无 emoji 前缀）
                Text(CountryFlag.flag(for: node.name))
                    .font(.system(size: 12.5))
                    .lineLimit(1)
                Text(node.name)
                    .font(.system(size: 12.5, weight: selected ? .semibold : .regular))
                    .lineLimit(1)
                    .foregroundStyle(Color.primary)
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
