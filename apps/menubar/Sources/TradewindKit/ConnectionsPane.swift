import SwiftUI

// MARK: - 页 2 连接与流量（M3-plan §11.3 页 2，demo :427-442 + :806-828）
//
// 结构：统计卡×4（活跃连接/下行/上行/今日↓）+ 大图表（TrafficStore.history 复用
// SparkChart 放大）+ 搜索框 + 连接表（时间/主机/规则/节点/↓↑ 5 列，等宽 11px，
// 行上限 500）。
// 数据口径（交付批注）：↓↑ 速率与今日流量统一以 TrafficStore 为准显示（规避
// snapshot 差值异源数字不一致），ConnectionsStore 只管连接表 + 活跃数。
// WS 生命周期：页面 onAppear/onDisappear + 窗口层 showSettings/orderOut 双挂
// （orderOut 不触发 SwiftUI onDisappear，窗口钩子兜底 §11.2-3；start/stop 幂等）。
// WHY 不自建整页滚动：宿主 ShellSettingsView.paneContent 已提供（GeneralPane 同款）；
// 连接表自带固定高度内层 ScrollView——确定性高度，上限型约束在滚动容器不可靠
// （NodeListLayout §3.1 同因）。

struct ConnectionsPane: View {
    @ObservedObject var store: ConnectionsStore
    @ObservedObject var traffic: TrafficStore
    @State private var query = ""
    @State private var timeFormatter = ConnectionsRowLogic.makeTimeFormatter()
    @Environment(\.colorScheme) private var scheme

    /// 连接表固定高度：窗口 540 内统计卡+大图约占 2/3，168 令统计卡常驻可见；
    /// 行 ≈25px 即 6 行视口，500 行上限由内层滚动消化
    static let tableHeight: CGFloat = 168

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

    // MARK: 连接表（demo table.conns：列宽 72/自适应/118/104/132，行 6px 纵向 padding）

    private var connectionTable: some View {
        VStack(spacing: 0) {
            tableHeader
            ScrollView(showsIndicators: true) {
                LazyVStack(spacing: 0) {
                    let rows = store.filtered(query: query)
                    if rows.isEmpty {
                        emptyState
                    } else {
                        ForEach(rows) { connection in
                            ConnectionRow(connection: connection, formatter: timeFormatter)
                        }
                    }
                }
            }
            .frame(height: Self.tableHeight)
        }
        .background(Theme.cardBackground(scheme))
        .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
        .overlay(cardStroke)
    }

    private var tableHeader: some View {
        HStack(spacing: 0) {
            headerCell("时间", width: 72)
            headerCell("主机", width: nil)
            headerCell("规则", width: 118)
            headerCell("节点", width: 104)
            headerCell("↓ / ↑", width: 132)
        }
        .padding(.vertical, 6)
        .background(alignment: .bottom) {
            Rectangle().fill(Theme.muted(scheme).opacity(0.2)).frame(height: 0.5)
        }
    }

    private func headerCell(_ title: String, width: CGFloat?) -> some View {
        Group {
            if let width {
                Text(title).frame(width: width, alignment: .leading)
            } else {
                Text(title).frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .font(.system(size: 11, weight: .semibold))
        .foregroundStyle(Theme.muted(scheme))
        .padding(.horizontal, 8)
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
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        HStack(spacing: 0) {
            cell(ConnectionsRowLogic.timeText(from: connection.startDate, formatter: formatter), width: 72)
            // 主机列加重（demo td.host font-weight 550 + 主文本色）
            cell(ConnectionsRowLogic.hostText(connection.metadata), width: nil)
                .font(.system(size: 11, weight: .semibold, design: .monospaced))
                .foregroundStyle(Color.primary)
            cell(connection.rule, width: 118)
            cell(ConnectionsRowLogic.nodeText(connection.chains), width: 104)
            cell(ConnectionsRowLogic.trafficText(download: connection.download,
                                                 upload: connection.upload), width: 132)
        }
        .font(.system(size: 11, design: .monospaced)) // 等宽 11px（交付规格）
        .foregroundStyle(Theme.secondary(scheme))
        .padding(.vertical, 6)
        .background(alignment: .top) {
            Rectangle().fill(Theme.muted(scheme).opacity(0.2)).frame(height: 0.5)
        }
    }

    /// demo td white-space:nowrap + ellipsis：单行尾截断；width nil = 自适应列（主机）
    @ViewBuilder
    private func cell(_ text: String, width: CGFloat?) -> some View {
        if let width {
            Text(text).lineLimit(1).frame(width: width, alignment: .leading).padding(.horizontal, 8)
        } else {
            Text(text).lineLimit(1).frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 8)
        }
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
