// 批⑦ c1（docs/plans/batch7-plan.md §3.8）：DMG 安装器窗口。纯展示层——全部状态
// 来自 InstallerModel（业务逻辑与副作用都在模型/支撑层，注入缝已测）；进度只到
// 步粒度，不展示用户路径（完成注记的 /Applications/Tradewind.app 公开定值豁免）。
import SwiftUI

struct InstallerWindow: View {
    @ObservedObject var model: InstallerModel

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            header
            stepList
            pathOption
            Spacer(minLength: 0)
            actionArea
        }
        .padding(24)
        .frame(width: 480)
    }

    // MARK: 头部

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("安装 Tradewind")
                .font(.title2.weight(.semibold))
            Text("内核、网页面板与命令行工具将安装到 Tradewind 数据目录；应用本体安装到「应用程序」文件夹。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    // MARK: 步清单（步粒度进度，§3.4 纪律）

    private var stepList: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(InstallerModel.Step.allCases, id: \.rawValue) { step in
                if step != .pathAuth || model.installPATH || model.pathAuthSkipped {
                    stepRow(step)
                }
            }
        }
    }

    private func stepRow(_ step: InstallerModel.Step) -> some View {
        HStack(spacing: 10) {
            Image(systemName: symbol(for: state(of: step)))
                .foregroundStyle(color(for: state(of: step)))
                .frame(width: 18)
            Text(step.title)
            Spacer()
            if step == .pathAuth && model.pathAuthSkipped {
                Text("已跳过")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .font(.callout)
    }

    // MARK: PATH 授权勾选项（可选步，默认不勾选；文案按 §3.5 定稿）

    private var pathOption: some View {
        Toggle(isOn: $model.installPATH) {
            VStack(alignment: .leading, spacing: 4) {
                Text("在终端可用 tradewind 命令（创建指向 Tradewind 管理副本的链接；需要输入一次管理员密码）")
                    .font(.callout)
                    .fixedSize(horizontal: false, vertical: true)
                if model.willTakeOverExistingCLI {
                    Text("检测到既有 tradewind 命令：将接管为指向 Tradewind 管理副本")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .disabled(!installOptionEditable)
    }

    /// 勾选项只在未开跑/等授权两态可改（运行/失败/完成期锁定）。
    private var installOptionEditable: Bool {
        if case .idle = model.phase { return true }
        if case .needsAppQuit = model.phase { return true }
        return false
    }

    // MARK: 动作区（idle/running/needsAppQuit/failed/finished 五态）

    @ViewBuilder
    private var actionArea: some View {
        switch model.phase {
        case .idle:
            HStack {
                Spacer()
                // 模型驱动 async（评审必修 2）：Task 派发，步体在模型内部后台执行
                Button("开始安装") { Task { await model.startInstall() } }
                    .keyboardShortcut(.defaultAction)
            }
        case .running(let step):
            HStack(spacing: 10) {
                ProgressView().controlSize(.small)
                Text(step.title).font(.callout).foregroundStyle(.secondary)
                Spacer()
            }
        case .needsAppQuit:
            VStack(alignment: .leading, spacing: 10) {
                Text("检测到已安装的 Tradewind 正在运行，需要先退出才能替换应用本体。")
                    .font(.callout)
                HStack {
                    Spacer()
                    Button("退出并继续") { Task { await model.confirmQuitAndContinue() } }
                        .keyboardShortcut(.defaultAction)
                }
            }
        case .failed(let step, let message):
            VStack(alignment: .leading, spacing: 10) {
                Label("「\(step.title)」未完成", systemImage: "xmark.octagon.fill")
                    .foregroundStyle(.red)
                Text(message)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(4)
                    .fixedSize(horizontal: false, vertical: true)
                HStack {
                    if case .pathAuth = step {
                        Button("跳过此步") { Task { await model.skipPathAuth() } }
                    }
                    Spacer()
                    Button("重试") { Task { await model.retry() } }
                        .keyboardShortcut(.defaultAction)
                }
            }
        case .finished(let note):
            VStack(alignment: .leading, spacing: 8) {
                Label("安装完成", systemImage: "checkmark.circle.fill")
                    .foregroundStyle(.green)
                if let note {
                    Text(note)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    // MARK: 步状态解析（done/active/pending/skipped/failed/waiting）

    private enum RowState {
        case done, active, pending, failed, waiting
    }

    private func state(of step: InstallerModel.Step) -> RowState {
        switch model.phase {
        case .idle:
            return .pending
        case .running(let current):
            if current == step { return .active }
            return step.rawValue < current.rawValue ? .done : .pending
        case .needsAppQuit:
            if step == .app { return .waiting }
            return step.rawValue < InstallerModel.Step.app.rawValue ? .done : .pending
        case .failed(let failedStep, _):
            if step == failedStep { return .failed }
            return step.rawValue < failedStep.rawValue ? .done : .pending
        case .finished:
            return .done
        }
    }

    private func symbol(for state: RowState) -> String {
        switch state {
        case .done: return "checkmark.circle.fill"
        case .active: return "clock"
        case .pending: return "circle"
        case .failed: return "xmark.octagon.fill"
        case .waiting: return "pause.circle"
        }
    }

    private func color(for state: RowState) -> Color {
        switch state {
        case .done: return .green
        case .active: return .accentColor
        case .pending: return .secondary
        case .failed: return .red
        case .waiting: return .orange
        }
    }
}
