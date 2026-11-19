import SwiftUI

/// ④ 速率卡：▼下行 ▲上行 + 今日流量 + WS 实时迷你图（下行面积 brand / 上行线 warn）。
/// WHY 独立文件：PanelCards.swift 收口后超 400 行硬上限，速率卡/图表自成一簇整体搬移（M3 批①）。
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
