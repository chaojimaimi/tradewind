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
        // popover 底色（批①.5 §3.2：demo --mat-pop 合成冷白/冷黑；NSPopover 自身圆角裁切内容）
        .background(Theme.popBackground(scheme))
        // toast 取 .bottom（M3-plan §3.3.3）：原 .top 压住标题行（用户截图一实锤）；
        // 声明偏差（§1.3）：demo toast 为桌面级通知（popover 之外），壳内为面板内提示
        .overlay(alignment: .bottom) {
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
                .fill(model.effectiveRunning ? Theme.ok : Theme.muted(scheme))
                .frame(width: 9, height: 9)
                .shadow(color: model.effectiveRunning ? Theme.ok : .clear, radius: 4)
            VStack(alignment: .leading, spacing: 1) {
                Text(model.effectiveRunning ? "Tradewind 运行中" : "Tradewind 已停止")
                    .font(.system(size: 14, weight: .semibold))
                Text(subtitle)
                    .font(.system(size: Theme.fontSizeSub))
                    .foregroundStyle(Theme.muted(scheme))
            }
            Spacer()
            // 自绘复刻 demo .switch（标准尺寸）；v0.4.3 缘由：原生 tint 在 NSPopover 首呈现
            // 竞态丢色（M3-plan §3.3.1 的"接受系统色"预案被真机证伪——macOS 26 无 tint
            // 即灰轨且首开必现，2026-11-19 用户截图实锤），自绘为确定性修复
            SwitchPill(isOn: masterBinding, accessibilityLabel: "Tradewind 主开关")
                .disabled(model.busy)
        }
        .padding(.bottom, 4)
    }

    private var subtitle: String {
        guard model.effectiveRunning, let mode = model.report?.mode else {
            return "内核已退出，系统代理已还原"
        }
        let modeName = ["rule": "规则", "global": "全局", "direct": "直连"][mode] ?? mode
        // v0.4.7 effective 口径：副标题随系统代理点击立即翻转，不等 networksetup 回读
        let proxy = model.effectiveSysProxy ? "已开启" : "已关闭"
        return "\(modeName)模式 · 系统代理\(proxy)"
    }

    private var masterBinding: Binding<Bool> {
        Binding(
            get: { model.effectiveRunning },
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
            .padding(.bottom, 6) // 随 overlay .bottom 对齐（M3-plan §3.3.3）
            // WHY 提示层不参与命中测试（v0.4.2 热修，2026-11-19 真机分析发现）：
            // toast 显示 3s 期间 overlay(.bottom) 会压住底部 FootButton 命中区，致按钮"点了没反应"。
            .allowsHitTesting(false)
    }
}

extension View {
    /// 卡片容器（demo .card：mat-card 底 + inset 描边 + 圆角 10 + 上边距）。
    func cardStyle() -> some View {
        modifier(CardStyle())
    }
}

/// WHY 以 ViewModifier 承载：cardStyle 挂在 View extension 上无法直接持有 @Environment，
/// modifier 内取 colorScheme 注入 Theme.cardBackground(scheme)（批①.5 §3.2，原静态
/// quaternaryLabelColor 会被暖色 vibrancy 染色）；调用点签名不变，侵入最小。
private struct CardStyle: ViewModifier {
    @Environment(\.colorScheme) private var scheme

    func body(content: Content) -> some View {
        content
            .padding(12)
            .background(Theme.cardBackground(scheme))
            .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
            .overlay(
                RoundedRectangle(cornerRadius: Theme.radiusCard)
                    .strokeBorder(Color.primary.opacity(0.08), lineWidth: 1)
            )
            .padding(.top, 10)
    }
}
