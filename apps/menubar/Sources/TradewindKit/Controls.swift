import SwiftUI

// MARK: - 节点列表布局判定（批①.5 §3.1，M3-plan）

/// 节点列表高度判定（demo `.node-list { max-height:168px }` 语义的确定性复刻）：
/// 行数 ≤ 5 直接 VStack 全显（不用 ScrollView）；> 5 固定 168pt 内部滚动。
/// WHY 不用 maxHeight+适度给高：NSPopover fittingSize 适配对"上限型"约束取 0 是
/// 平台行为（M3-plan §2.2，竖向 ScrollView 收缩为 0 致整块不渲染），
/// 确定性高度是唯一稳态；矮则留白与 demo max-height 行为一致，接受。
enum NodeListLayout {
    /// fitAll：行数 ≤ 阈值，VStack 全显
    case fitAll
    /// fixedScroll：行数 > 阈值，ScrollView 固定 scrollHeight 内部滚动
    case fixedScroll

    /// 行高基准 30pt：NodeRow `.padding(.vertical, 7)`×2=14 + 12.5pt 文本行≈16；
    /// 真机终验若实测行高 ≥34（5 行超 168）则阈值降 4 并声明偏差（M3-plan §3.1）
    static let rowHeightBaseline: CGFloat = 30
    /// 滚动态固定高度（demo .node-list max-height:168px）
    static let scrollHeight: CGFloat = 168
    /// 全显阈值 = floor(168/30) = 5（5×30+4×2=158 ≤ 168 自然成立）
    static let fitThreshold = 5

    /// 纯函数判定，`@testable` 可测（View.body 语义冲突故名 layout）
    static func layout(forCount count: Int) -> NodeListLayout {
        count <= fitThreshold ? .fitAll : .fixedScroll
    }
}

// MARK: - 自绘分段控件（批①.5 §3.3，demo .seg 逐项对齐）

/// 自绘分段控件：容器 mat-card-2 + padding 2 + 圆角 8 + inset 描边 --hair；
/// 按钮 padding 3/14、圆角 6、字号 12.5；选中钮第三层底色+浮起阴影、文本 primary，
/// 未选中透明底、文本 secondary。替换原生 Picker(.segmented)（深色选中态观感不符 demo）。
/// 整层禁用由调用方 .disabled 控制（行为与原 Picker 等价，差异仅视觉）。
struct SegmentedPill: View {
    let options: [(value: String, label: String)]
    @Binding var selection: String
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        // WHY spacing 0：demo .seg 为 inline-flex 无 gap（ui-demo.html:102），按钮紧邻
        HStack(spacing: 0) {
            // WHY 下标遍历：Swift 键路径不支持元组元素，无法 id: \.value
            ForEach(options.indices, id: \.self) { index in
                pillButton(options[index])
            }
        }
        .padding(2)
        .background(
            RoundedRectangle(cornerRadius: Theme.radiusContainer)
                .fill(Theme.controlBackground(scheme))
        )
        .overlay(
            RoundedRectangle(cornerRadius: Theme.radiusContainer)
                .strokeBorder(Color.primary.opacity(0.1), lineWidth: 1) // ≈ --hair
        )
    }

    private func pillButton(_ option: (value: String, label: String)) -> some View {
        let isSelected = option.value == selection
        return Button {
            selection = option.value
        } label: {
            Text(option.label)
                .font(.system(size: 12.5))
                .padding(.vertical, 3)
                .padding(.horizontal, 14)
                .background(
                    RoundedRectangle(cornerRadius: Theme.radiusControl)
                        .fill(isSelected ? AnyShapeStyle(Theme.controlSelected(scheme))
                                         : AnyShapeStyle(.clear))
                        // 浮起阴影只压底色层（demo .seg button.on box-shadow 0 1px 3px rgba(0,0,0,.25)）
                        .shadow(color: isSelected ? Color.black.opacity(0.25) : .clear,
                                radius: 1.5, y: 1)
                )
                .foregroundStyle(isSelected ? Color.primary : Theme.secondary(scheme))
        }
        .buttonStyle(.plain)
    }
}

// MARK: - 底部按钮（批①.5 §3.4，demo .btn）

/// 底部按钮：padding 5/14、圆角 --r-ctl 6、底 mat-card-2、inset 描边 --hair。
/// 等宽（.pop-foot .btn flex:1）由调用方 fillsWidth 声明；⏻ 自然宽用
/// horizontalPadding: 10，危险红由 label 自带 foregroundStyle 表达（demo .btn.danger）。
struct FootButton<Label: View>: View {
    var horizontalPadding: CGFloat = 14
    var fillsWidth = false
    let action: () -> Void
    @ViewBuilder var label: () -> Label
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        Button(action: action) {
            label()
                .font(.system(size: 12.5))
                .frame(maxWidth: fillsWidth ? .infinity : nil)
                .padding(.vertical, 5)
                .padding(.horizontal, horizontalPadding)
                .background(
                    RoundedRectangle(cornerRadius: Theme.radiusControl)
                        .fill(Theme.controlBackground(scheme))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: Theme.radiusControl)
                        .strokeBorder(Color.primary.opacity(0.1), lineWidth: 1) // ≈ --hair
                )
        }
        .buttonStyle(.plain)
    }
}
