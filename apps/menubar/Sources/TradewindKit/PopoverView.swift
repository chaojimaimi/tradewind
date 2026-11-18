import SwiftUI

/// popover 主面板组合根（demo 主面板布局顺序，M2-plan §5/§5.5）：
/// 状态行 → 当前节点卡 → 开关组 → 速率卡 → 策略组 → 底部按钮组；
/// 降级横幅置于状态行下（控制面仍可用）。卡片视图实现见 PanelCards.swift。
struct ShellRootView: View {
    @ObservedObject var model: PanelModel
    @Environment(\.colorScheme) private var scheme

    /// WHY 可选注入：PanelModel.live() 是 MainActor 隔离方法，不能作默认参数求值。
    init(model: PanelModel) {
        self.model = model
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            PopoverHeadView(model: model)
            if model.degraded {
                DegradedBanner()
            }
            CurrentNodeCard(model: model)
            ToggleGroupCard(model: model)
            SpeedCardView(traffic: model.traffic)
            GroupsCardView(model: model)
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
        .onReceive(model.popoverDidShow) { _ in
            Task { await model.refresh() }
        }
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

// MARK: 降级横幅 / toast / 小组件 / 卡片容器

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

extension View {
    /// 卡片容器（demo .card：mat-card 底 + inset 描边 + 圆角 10 + 上边距）。
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
