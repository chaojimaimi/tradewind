import SwiftUI

/// 空 popover 骨架（commit 7）：占位主面板，commit 9 按 demo 主面板填充。
/// 对照口径（M2-plan §5.5）：布局/语义/tokens 对齐 demo，非像素级。
struct ShellRootView: View {
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(spacing: Theme.spacing[1]) {
            Image(systemName: "wind")
                .font(.system(size: 28))
                .foregroundStyle(Theme.brand(scheme))
            Text("Tradewind")
                .font(.system(size: Theme.fontSizeTitle, weight: .semibold))
            Text("面板加载中…")
                .font(.system(size: Theme.fontSizeSub))
                .foregroundStyle(Theme.muted(scheme))
        }
        .frame(width: Theme.popoverWidth, height: 220)
    }
}
