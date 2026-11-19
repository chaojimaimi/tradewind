import SwiftUI

/// ② 当前节点卡（旗帜 + 节点名 + 固定绿延迟徽标 + 组名 + 出站模式 seg；demo .ms-chip 单色事实）。
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
                Picker("", selection: modeBinding) {
                    Text("规则").tag("rule")
                    Text("全局").tag("global")
                    Text("直连").tag("direct")
                }
                .pickerStyle(.segmented)
                .frame(width: 168)
                .disabled(!model.running || model.busy)
            }
        }
        .cardStyle()
    }

    /// 延迟徽标：固定绿（demo .ms-chip：ok 色底 16% + ok 字色，圆角 5，11px mono），
    /// 不按延迟色阶分级（三色阶已由节点行色点表达）；无记录显示"—"（灰）（M3-plan §3.2）
    @ViewBuilder private var delayBadge: some View {
        if let delay = model.currentNodeDelay {
            Text("\(delay) ms")
                .font(.system(size: Theme.fontSizeSub, weight: .semibold))
                .monospacedDigit()
                .padding(.horizontal, 7)
                .padding(.vertical, 1)
                .background(Theme.ok.opacity(0.16), in: RoundedRectangle(cornerRadius: 5))
                .foregroundStyle(Theme.ok)
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

/// ④ 速率卡：▼下行 ▲上行 + 今日流量 + WS 实时迷你图（下行面积 brand / 上行线 warn）。
struct SpeedCardView: View {
    @ObservedObject var traffic: TrafficStore
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 14) {
                SpeedLabel(symbol: "arrow.down", color: Theme.brand(scheme), value: traffic.down)
                SpeedLabel(symbol: "arrow.up", color: Theme.warn, value: traffic.up)
                Spacer()
                Text("今日 \(RateText.bytes(traffic.todayBytes))")
                    .font(.system(size: Theme.fontSizeSub))
                    .monospacedDigit()
                    .foregroundStyle(Theme.muted(scheme))
            }
            SparkChart(samples: traffic.history)
                .frame(height: 56)
        }
        .cardStyle()
    }
}

private struct SpeedLabel: View {
    let symbol: String
    let color: Color
    let value: Double

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: symbol)
                .font(.system(size: 10))
                .foregroundStyle(color)
            Text(RateText.bytes(value) + "/s")
                .font(.system(size: 14, weight: .semibold))
                .monospacedDigit()
        }
    }
}

/// 迷你图（demo drawChart 形态近似：下行渐变面积 + 末点标记；上行细线）。
struct SparkChart: View {
    let samples: [TrafficSample]
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        Canvas { context, size in
            guard samples.count > 1 else { return }
            let maxValue = max(samples.map(\.down).max() ?? 0, 1_000_000) * 1.15
            let stepX = size.width / CGFloat(samples.count - 1)
            let y: (Double) -> CGFloat = { size.height - 6 - CGFloat($0 / maxValue) * (size.height - 14) }

            var area = Path()
            area.move(to: CGPoint(x: 0, y: size.height))
            for (index, sample) in samples.enumerated() {
                area.addLine(to: CGPoint(x: CGFloat(index) * stepX, y: y(sample.down)))
            }
            area.addLine(to: CGPoint(x: size.width, y: size.height))
            area.closeSubpath()
            let gradient = GraphicsContext.Shading.linearGradient(
                Gradient(colors: [Theme.brand(scheme).opacity(0.34), Theme.brand(scheme).opacity(0)]),
                startPoint: CGPoint(x: 0, y: 0),
                endPoint: CGPoint(x: 0, y: size.height)
            )
            context.fill(area, with: gradient)

            var downLine = Path()
            var upLine = Path()
            for (index, sample) in samples.enumerated() {
                let point = CGPoint(x: CGFloat(index) * stepX, y: y(sample.down))
                let upPoint = CGPoint(x: CGFloat(index) * stepX, y: y(sample.up))
                if index == 0 {
                    downLine.move(to: point)
                    upLine.move(to: upPoint)
                } else {
                    downLine.addLine(to: point)
                    upLine.addLine(to: upPoint)
                }
            }
            context.stroke(downLine, with: .color(Theme.brand(scheme)), lineWidth: 2)
            context.stroke(upLine, with: .color(Theme.warn.opacity(0.85)), lineWidth: 1.3)

            if let last = samples.last {
                let dot = CGPoint(x: size.width - 2, y: y(last.down))
                context.fill(Path(ellipseIn: CGRect(x: dot.x - 3, y: dot.y - 3, width: 6, height: 6)),
                             with: .color(Theme.brand(scheme)))
            }
        }
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
/// + ✓ 当前（固定绿延迟徽标仅用于节点卡——v1.1 更正）。
private struct NodeList: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    private var group: ProxyGroup? {
        model.groups.first { $0.id == model.activeGroupID } ?? model.groups.first
    }

    var body: some View {
        ScrollView(showsIndicators: true) {
            VStack(spacing: 2) {
                ForEach(group?.nodes ?? []) { node in
                    NodeRow(node: node, selected: node.name == group?.currentNode) {
                        Task { await model.selectNode(node.name) }
                    }
                }
            }
        }
        .frame(maxHeight: 168)
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

/// ⑥ 底部：网页面板 / 设置…(置灰 M3) / 退出（停内核+还原代理）。
struct PopoverFootView: View {
    @ObservedObject var model: PanelModel

    var body: some View {
        HStack(spacing: 8) {
            Button {
                Task { await model.openPanel() }
            } label: {
                Label("网页面板", systemImage: "globe")
            }
            Button {
            } label: {
                Text("设置…")
            }
            .disabled(true)
            .help("M3 交付：随原生设置窗口")
            Button(role: .destructive) {
                Task { await model.stopCoreAndQuit() }
            } label: {
                Image(systemName: "power")
            }
            .help("停止内核并还原系统代理，然后退出壳")
        }
        .buttonStyle(.bordered)
        .controlSize(.small)
        .padding(.top, 12)
    }
}
