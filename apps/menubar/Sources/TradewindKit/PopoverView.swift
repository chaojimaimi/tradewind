import SwiftUI

/// popover 主面板（demo 主面板布局/语义/tokens 对照，M2-plan §5/§5.5）。
/// 本 commit 交付：状态行 / 当前节点卡 / 开关组（TUN+代理守护 M2 禁用态）/
/// 降级横幅 / 底部按钮组；速率卡与策略组列表由 commit 10（TrafficStore）填充。
struct ShellRootView: View {
    @StateObject private var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    init(model: PanelModel? = nil) {
        // WHY 不用默认参数直接调 .live()：MainActor 隔离方法不能在非隔离默认表达式里调用
        _model = StateObject(wrappedValue: model ?? PanelModel.live())
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            PopoverHeadView(model: model)
            if model.degraded {
                DegradedBanner()
            }
            CurrentNodeCard(model: model)
            ToggleGroupCard(model: model)
            GroupsCard(model: model)
            PopoverFootView(model: model)
        }
        .padding(14)
        .frame(width: Theme.popoverWidth)
        .overlay(alignment: .top) {
            if let toast = model.toast {
                ToastView(text: toast)
            }
        }
        .task { await model.refresh() }
    }
}

// MARK: ① 状态行（状态点 + 标题/副标题 + 主开关）

private struct PopoverHeadView: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        HStack(spacing: 10) {
            Circle()
                .fill(model.running ? Theme.ok : Theme.muted(scheme))
                .frame(width: 9, height: 9)
                .shadow(color: model.running ? Theme.ok : .clear, radius: 4)
            VStack(alignment: .leading, spacing: 1) {
                Text(model.running ? "Tradewind 运行中" : "Tradewind 已停止")
                    .font(.system(size: 14, weight: .semibold))
                Text(subtitle)
                    .font(.system(size: Theme.fontSizeSub))
                    .foregroundStyle(Theme.muted(scheme))
            }
            Spacer()
            Toggle("", isOn: masterBinding)
                .toggleStyle(.switch)
                .labelsHidden()
                .disabled(model.busy)
        }
        .padding(.bottom, 4)
    }

    private var subtitle: String {
        guard model.running, let mode = model.report?.mode else {
            return "内核已退出，系统代理已还原"
        }
        let modeName = ["rule": "规则", "global": "全局", "direct": "直连"][mode] ?? mode
        let proxy = (model.report?.sysProxy.applied ?? false) ? "已开启" : "已关闭"
        return "\(modeName)模式 · 系统代理\(proxy)"
    }

    private var masterBinding: Binding<Bool> {
        Binding(
            get: { model.running },
            set: { newValue in Task { await model.toggleMaster(on: newValue) } }
        )
    }
}

// MARK: ② 当前节点卡（节点名 + 固定绿延迟徽标 + 出站模式 seg）

private struct CurrentNodeCard: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Text(currentNodeName ?? "—")
                    .font(.system(size: 14, weight: .semibold))
                    .lineLimit(1)
                Spacer()
                Text("当前组 \(currentGroupName ?? "—")")
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

// MARK: ③ 开关组（系统代理可用；TUN/代理守护 M2 禁用态 + P2 徽标）

private struct ToggleGroupCard: View {
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

private struct ToggleRow: View {
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
            Toggle("", isOn: $isOn)
                .toggleStyle(.switch)
                .labelsHidden()
                .controlSize(.small)
                .disabled(disabled)
        }
        .padding(.vertical, 7)
        .padding(.horizontal, 8)
    }
}

// MARK: ④⑤ 速率卡 + 策略组（commit 10 填充，此为骨架占位）

private struct GroupsCard: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("策略组")
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(Theme.secondary(scheme))
            Text(model.degraded ? "面板数据不可用" : "节点列表接入中…")
                .font(.system(size: Theme.fontSizeSub))
                .foregroundStyle(.tertiary)
        }
        .cardStyle()
    }
}

// MARK: ⑥ 底部：网页面板 / 设置…(置灰 M3) / 退出（停内核+还原代理）

private struct PopoverFootView: View {
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

// MARK: 降级横幅 / toast / 小组件

/// 降级态（M2-plan §5 v1.1）：面板数据不可用（run `tradewind start`），控制面仍可用。
private struct DegradedBanner: View {
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        Text("面板数据不可用（运行 tradewind start 后自动恢复）")
            .font(.system(size: Theme.fontSizeSub))
            .foregroundStyle(Theme.warn)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 12)
            .padding(.vertical, 7)
            .background(Theme.warn.opacity(0.12))
            .cornerRadius(Theme.radiusControl)
            .padding(.bottom, 10)
    }
}

private struct ToastView: View {
    let text: String

    var body: some View {
        Text(text)
            .font(.system(size: Theme.fontSizeSub))
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            .background(.ultraThinMaterial, in: Capsule())
            .padding(.top, 6)
    }
}

private struct BadgeView: View {
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

// MARK: 卡片容器样式（demo .card：mat-card 底 + inset 描边 + 圆角 10）

extension View {
    func cardStyle() -> some View {
        self
            .padding(12)
            .background(Theme.cardBackground)
            .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
            .overlay(
                RoundedRectangle(cornerRadius: Theme.radiusCard)
                    .strokeBorder(Color.primary.opacity(0.08), lineWidth: 1)
            )
            .padding(.top, 10)
    }
}
