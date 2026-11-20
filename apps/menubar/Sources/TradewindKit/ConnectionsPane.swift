import SwiftUI

// MARK: - 页 2 连接与流量（M3-plan §11.3 页 2，demo :427-442 + :806-828）
//
// 结构：统计卡×4（活跃连接/下行/上行/今日↓）+ 大图表（TrafficStore.history 复用
// SparkChart 放大）+ 搜索框 + 连接表（时间/主机/规则/节点/↓↑ 5 列，等宽 11px，
// 行上限 500）。§14 R3 增强：host/rule/node 列头右缘把手可拖宽 + 整表横滚 +
// 单元格 tooltip（demo 固定列宽为本批用户反馈显式增强，偏差登记）。
// 数据口径（交付批注）：↓↑ 速率与今日流量统一以 TrafficStore 为准显示（规避
// snapshot 差值异源数字不一致），ConnectionsStore 只管连接表 + 活跃数。
// WS 生命周期：页面 onAppear/onDisappear + 窗口层 showSettings/orderOut 双挂
// （orderOut 不触发 SwiftUI onDisappear，窗口钩子兜底 §11.2-3；start/stop 幂等）。
// WHY 不自建整页滚动：宿主 ShellSettingsView.paneContent 已提供（GeneralPane 同款）；
// 连接表自带固定高度内层 ScrollView——确定性高度，上限型约束在滚动容器不可靠
// （NodeListLayout §3.1 同因）。

// MARK: - 连接表布局算术（M3-plan §14 R3：列宽可调 + 横滚；纯数据可测，NodeListLayout 同构先例）

/// 连接表槽位/宽度算术（评审 P1-1 统一口径：槽位=文本宽+16px padding）。
/// time/traffic 固定槽；host/rule/node 文本宽为 @AppStorage 偏好（ShellPrefs
/// conn_col_* 键，列头把手拖动实时 clamp）。host 槽 = max(偏好槽, 可视富余)——
/// 富余填满不横滚、偏好槽超出富余生效拖宽横滚；tableWidth 恒 ≥ 可视宽。
enum ConnectionTableLayout {
    /// 时间列固定槽（demo 72 文本 + 16 padding）
    static let timeSlot: CGFloat = 88
    /// ↓↑ 列固定槽（demo 132 文本 + 16 padding）
    static let trafficSlot: CGFloat = 148

    /// host 槽位：偏好槽（hostPref+16）与可视富余（available-othersTotal）取大
    static func hostSlot(hostPref: CGFloat, othersTotal: CGFloat, available: CGFloat) -> CGFloat {
        max(hostPref + 16, available - othersTotal)
    }

    /// 表总宽：不小于可视宽（填充分支），拖宽溢出时为内容宽（横滚出口）
    static func tableWidth(othersTotal: CGFloat, hostSlot: CGFloat, available: CGFloat) -> CGFloat {
        max(available, othersTotal + hostSlot)
    }

    /// 文本宽 clamp（把手拖动实时收敛）
    static func clamped(_ value: CGFloat, to range: ClosedRange<CGFloat>) -> CGFloat {
        min(max(value, range.lowerBound), range.upperBound)
    }

    /// 三列缺省文本宽（host 150 / rule 118 / node 104——118/104 承 demo 列宽；
    /// 缺省总槽 88+166+134+120+148=656 > 可视 538，默认即横滚，§14.4 目验 3）。
    /// WHY 显式元组类型：@AppStorage 初值取元组成员时泛型重载解析失败
    /// （"no exact matches in call to initializer"），显式 CGFloat 消歧
    static let defaults: (host: CGFloat, rule: CGFloat, node: CGFloat) = (150, 118, 104)
    /// 三列文本宽 clamp 范围
    static let hostRange: ClosedRange<CGFloat> = 100...600
    static let ruleRange: ClosedRange<CGFloat> = 80...300
    static let nodeRange: ClosedRange<CGFloat> = 80...260
}

struct ConnectionsPane: View {
    @ObservedObject var store: ConnectionsStore
    @ObservedObject var traffic: TrafficStore
    @State private var query = ""
    @State private var timeFormatter = ConnectionsRowLogic.makeTimeFormatter()
    @Environment(\.colorScheme) private var scheme

    /// 连接表固定高度：窗口 540 内统计卡+大图约占 2/3，168 令统计卡常驻可见；
    /// 行 ≈25px 即 6 行视口，500 行上限由内层滚动消化
    static let tableHeight: CGFloat = 168
    /// 表头高度（11pt semibold 行高 ≈14 + 上下 padding 12）：GeometryReader 贪占
    /// 纵向空间须以固定总高收敛（§14.3），card 总高 = 表头 + 行视口
    static let headerHeight: CGFloat = 26

    /// 三列文本宽偏好（§14 R3：ShellPrefs 集中键 + ConnectionTableLayout 缺省，
    /// 列头把手拖动实时 clamp 写入；键生命周期限于连接页，不走通知链）。
    /// WHY Double 而非 CGFloat：本 SDK @AppStorage 无 CGFloat 重载（候选仅
    /// Bool/Data/Double/Int/String/URL/RawRepresentable），布局用 CGFloat 视图换算
    @AppStorage(ShellPrefs.connColHostKey) private var hostPrefStored: Double = Double(ConnectionTableLayout.defaults.host)
    @AppStorage(ShellPrefs.connColRuleKey) private var rulePrefStored: Double = Double(ConnectionTableLayout.defaults.rule)
    @AppStorage(ShellPrefs.connColNodeKey) private var nodePrefStored: Double = Double(ConnectionTableLayout.defaults.node)
    /// 布局用 CGFloat 视图（UserDefaults 存 Double，见上 WHY）
    private var hostPref: CGFloat { CGFloat(hostPrefStored) }
    private var rulePref: CGFloat { CGFloat(rulePrefStored) }
    private var nodePref: CGFloat { CGFloat(nodePrefStored) }
    /// 在途拖动基线（DragGesture translation 为相对手势起点的量，需绝对基线换算；
    /// 同时仅一根把手在途，onEnded 清空）
    @State private var dragStartWidth: CGFloat?
    /// hover 高亮的把手列（nil=无）
    @State private var hoveredHandle: ResizableColumn?

    /// 可拖宽列（time/traffic 固定槽无把手）
    private enum ResizableColumn { case host, rule, node }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            header
            statGrid
            bigChart
            searchField
            connectionTable
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 20)
        .frame(maxWidth: .infinity, alignment: .leading)
        .onAppear { Task { await store.start() } }
        .onDisappear { store.stop() }
    }

    /// 今日流量卡数值/单位分离（审查 P3-4 demo 排版对齐）
    private var todayParts: (valueText: String, unitText: String) {
        SubsPaneLogic.todayBytesParts(traffic.todayBytes)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("连接与流量").font(.system(size: 17, weight: .bold))
            Text("活跃连接实时视图（数据来自 mihomo /connections 与 /traffic）。")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
        }
    }

    // MARK: 统计卡×4（demo .stat-grid gap 10；值均等宽数字防跳动）

    private var statGrid: some View {
        HStack(spacing: 10) {
            StatCard(label: "活跃连接", value: "\(store.activeCount)", unit: "")
            StatCard(label: "下行速率", value: RateText.bytes(traffic.down), unit: "/s")
            StatCard(label: "上行速率", value: RateText.bytes(traffic.up), unit: "/s")
            StatCard(label: "今日流量", value: todayParts.valueText, unit: todayParts.unitText) // demo :433 数值+单位小字
        }
    }

    // MARK: 大图表（demo canvas.big-chart 高 170：SparkChart 零改动复用放大）

    private var bigChart: some View {
        SparkChart(samples: traffic.history)
            .frame(height: 170)
            .padding(.horizontal, 6)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 4)
            .background(Theme.cardBackground(scheme))
            .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
            .overlay(cardStroke)
    }

    private var cardStroke: some View {
        RoundedRectangle(cornerRadius: Theme.radiusCard)
            .strokeBorder(Color.primary.opacity(0.08), lineWidth: 1)
    }

    private var searchField: some View {
        TextField("搜索主机名 / 规则 / 节点…", text: $query)
            .textFieldStyle(.roundedBorder)
            .font(.system(size: 12))
            .accessibilityLabel("搜索连接")
    }

    // MARK: 连接表（demo table.conns 列宽 72/自适应/118/104/132 的可调版，§14 R3：
    // host/rule/node 列头右缘把手拖宽 + 整表横滚 + 单元格 tooltip，行 6px 纵向 padding）

    private var connectionTable: some View {
        // WHY 外层 GeometryReader 取可视宽 + 固定总高：GeometryReader 贪占纵向
        // 空间撑破 168 容器（§14.3），固定高收敛后内层表卡填满可视宽
        GeometryReader { geo in
            tableCard(available: geo.size.width)
        }
        .frame(height: Self.tableHeight + Self.headerHeight)
    }

    /// 表卡：横滚视口（card 铬按可视宽绘制）包 header + 行内容（同宽同步横移，
    /// 槽位算术全由 ConnectionTableLayout 提供）
    private func tableCard(available: CGFloat) -> some View {
        let othersTotal = ConnectionTableLayout.timeSlot
            + rulePref + 16 + nodePref + 16 + ConnectionTableLayout.trafficSlot
        let hostSlot = ConnectionTableLayout.hostSlot(hostPref: hostPref,
                                                      othersTotal: othersTotal,
                                                      available: available)
        let tableWidth = ConnectionTableLayout.tableWidth(othersTotal: othersTotal,
                                                          hostSlot: hostSlot,
                                                          available: available)
        return ScrollView(.horizontal, showsIndicators: true) {
            VStack(spacing: 0) {
                tableHeader(hostSlot: hostSlot)
                rowsArea(hostSlot: hostSlot, tableWidth: tableWidth)
            }
            .frame(width: tableWidth, alignment: .leading)
        }
        .background(Theme.cardBackground(scheme))
        .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
        .overlay(cardStroke)
    }

    /// 行区：空态按可视宽居中；有行时行内容宽 = tableWidth（垂滚内嵌横滚两轴正交）
    private func rowsArea(hostSlot: CGFloat, tableWidth: CGFloat) -> some View {
        ScrollView(showsIndicators: true) {
            let rows = store.filtered(query: query)
            if rows.isEmpty {
                emptyState
            } else {
                LazyVStack(spacing: 0) {
                    ForEach(rows) { connection in
                        ConnectionRow(connection: connection, formatter: timeFormatter,
                                      hostTextWidth: hostSlot - 16,
                                      ruleTextWidth: rulePref,
                                      nodeTextWidth: nodePref)
                    }
                }
                .frame(width: tableWidth, alignment: .leading)
            }
        }
        .frame(height: Self.tableHeight)
    }

    private func tableHeader(hostSlot: CGFloat) -> some View {
        HStack(spacing: 0) {
            headerCell("时间", textWidth: ConnectionTableLayout.timeSlot - 16)
            resizableHeaderCell("主机", textWidth: hostSlot - 16,
                                column: .host, width: $hostPrefStored,
                                range: ConnectionTableLayout.hostRange)
            resizableHeaderCell("规则", textWidth: rulePref,
                                column: .rule, width: $rulePrefStored,
                                range: ConnectionTableLayout.ruleRange)
            resizableHeaderCell("节点", textWidth: nodePref,
                                column: .node, width: $nodePrefStored,
                                range: ConnectionTableLayout.nodeRange)
            headerCell("↓ / ↑", textWidth: ConnectionTableLayout.trafficSlot - 16)
        }
        .padding(.vertical, 6)
        .background(alignment: .bottom) {
            Rectangle().fill(Theme.muted(scheme).opacity(0.2)).frame(height: 0.5)
        }
    }

    private func headerCell(_ title: String, textWidth: CGFloat) -> some View {
        Text(title)
            .frame(width: textWidth, alignment: .leading)
            .font(.system(size: 11, weight: .semibold))
            .foregroundStyle(Theme.muted(scheme))
            .padding(.horizontal, 8)
    }

    /// 列头单元格 + 右缘拖宽把手（8pt 热区叠 1pt 分隔线，hover brand 高亮）
    private func resizableHeaderCell(_ title: String, textWidth: CGFloat,
                                     column: ResizableColumn, width: Binding<Double>,
                                     range: ClosedRange<CGFloat>) -> some View {
        headerCell(title, textWidth: textWidth)
            .overlay(alignment: .trailing) {
                dragHandle(column: column, width: width, range: range)
            }
    }

    /// 把手：DragGesture onChanged 实时 clamp 写 @AppStorage（写开销可忽略 §14.3；
    /// 掉帧预案 onChanged 写 @State/onEnded 落盘——不预做）。clamp 算术走
    /// ConnectionTableLayout（CGFloat），落盘换算 Double（AppStorage 重载实况）
    private func dragHandle(column: ResizableColumn, width: Binding<Double>,
                            range: ClosedRange<CGFloat>) -> some View {
        Rectangle()
            .fill(Color.clear)
            .frame(width: 8)
            .contentShape(Rectangle())
            .overlay(alignment: .trailing) {
                Rectangle()
                    .fill(hoveredHandle == column
                            ? AnyShapeStyle(Theme.brand(scheme))
                            : AnyShapeStyle(Theme.muted(scheme).opacity(0.2)))
                    .frame(width: 1)
            }
            .onHover { hovering in hoveredHandle = hovering ? column : nil }
            .gesture(DragGesture(minimumDistance: 0)
                .onChanged { value in
                    let base = dragStartWidth ?? CGFloat(width.wrappedValue)
                    dragStartWidth = base
                    width.wrappedValue = Double(ConnectionTableLayout.clamped(
                        base + value.translation.width, to: range))
                }
                .onEnded { _ in dragStartWidth = nil })
    }

    private var emptyState: some View {
        Text(store.connected ? "暂无匹配连接" : "等待连接数据推送…")
            .font(.system(size: 12))
            .foregroundStyle(Theme.muted(scheme))
            .frame(maxWidth: .infinity)
            .padding(.vertical, 24)
    }
}

// MARK: - 统计卡（demo .stat：mat-card 底 + hair 描边 + padding 10/12，k 11px v 17px bold）

private struct StatCard: View {
    let label: String
    let value: String
    let unit: String
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label)
                .font(.system(size: 11))
                .foregroundStyle(Theme.muted(scheme))
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                Text(value)
                    .font(.system(size: 17, weight: .bold).monospacedDigit())
                Text(unit)
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(Theme.muted(scheme))
            }
        }
        .padding(.vertical, 10)
        .padding(.horizontal, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Theme.cardBackground(scheme))
        .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
        .overlay(
            RoundedRectangle(cornerRadius: Theme.radiusCard)
                .strokeBorder(Color.primary.opacity(0.08), lineWidth: 1)
        )
    }
}

// MARK: - 连接表行（demo td：6/8 padding、hair 上分隔、host 加重；全表等宽 11px）

private struct ConnectionRow: View {
    let connection: Connection
    let formatter: DateFormatter
    /// 三列有效文本宽（host=布局有效槽-16；rule/node=偏好文本宽，§14 R3）
    let hostTextWidth: CGFloat
    let ruleTextWidth: CGFloat
    let nodeTextWidth: CGFloat
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        HStack(spacing: 0) {
            cell(ConnectionsRowLogic.timeText(from: connection.startDate, formatter: formatter),
                 textWidth: ConnectionTableLayout.timeSlot - 16)
            // 主机列加重（demo td.host font-weight 550 + 主文本色）
            cell(ConnectionsRowLogic.hostText(connection.metadata), textWidth: hostTextWidth)
                .font(.system(size: 11, weight: .semibold, design: .monospaced))
                .foregroundStyle(Color.primary)
            cell(connection.rule, textWidth: ruleTextWidth)
            cell(ConnectionsRowLogic.nodeText(connection.chains), textWidth: nodeTextWidth)
            cell(ConnectionsRowLogic.trafficText(download: connection.download,
                                                 upload: connection.upload),
                 textWidth: ConnectionTableLayout.trafficSlot - 16)
        }
        .font(.system(size: 11, design: .monospaced)) // 等宽 11px（交付规格）
        .foregroundStyle(Theme.secondary(scheme))
        .padding(.vertical, 6)
        .background(alignment: .top) {
            Rectangle().fill(Theme.muted(scheme).opacity(0.2)).frame(height: 0.5)
        }
    }

    /// demo td white-space:nowrap + ellipsis：单行尾截断；.help 悬停看全文（§14 R3）
    private func cell(_ text: String, textWidth: CGFloat) -> some View {
        Text(text)
            .lineLimit(1)
            .frame(width: textWidth, alignment: .leading)
            .padding(.horizontal, 8)
            .help(text)
    }
}

// MARK: - 连接表行展示逻辑（demo :808-811 口径的壳侧映射；纯函数可测）

enum ConnectionsRowLogic {
    /// 主机列：host → sniffHost → destinationIP[:port]（直连无 host 形态）；全空 "—"
    static func hostText(_ metadata: ConnectionMetadata) -> String {
        if !metadata.host.isEmpty { return metadata.host }
        if !metadata.sniffHost.isEmpty { return metadata.sniffHost }
        if !metadata.destinationIP.isEmpty {
            return metadata.destinationPort.isEmpty
                ? metadata.destinationIP
                : "\(metadata.destinationIP):\(metadata.destinationPort)"
        }
        return "—"
    }

    /// 节点列：chains[0]=实际出口节点（实测形态 ["HK-1","Auto","Proxy"]）；空链 "—"
    static func nodeText(_ chains: [String]) -> String {
        chains.first ?? "—"
    }

    /// 时间列：HH:mm（demo 口径）；解析失败 "—"。formatter 注入（测试锚定时区）。
    static func timeText(from date: Date?, formatter: DateFormatter) -> String {
        guard let date else { return "—" }
        return formatter.string(from: date)
    }

    /// ↓↑ 列：`↓值 / ↑值`（demo fmtB(download) / fmtB(upload)；RateText 单位口径）
    static func trafficText(download: Double, upload: Double) -> String {
        "\(RateText.bytes(download)) / \(RateText.bytes(upload))"
    }

    /// 行时间 formatter（"HH:mm"；WHY 每次构造：DateFormatter 非 Sendable，不做
    /// 静态共享——SubsPaneLogic.makeCardDateFormatter 同注）
    static func makeTimeFormatter() -> DateFormatter {
        let formatter = DateFormatter()
        formatter.dateFormat = "HH:mm"
        return formatter
    }
}
