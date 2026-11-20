import SwiftUI

// MARK: - 页 1 订阅管理（M3-plan §11.3 页 1，demo :414-424 + :765-806）
//
// 结构：顶部说明 + 按钮行 [＋ 添加订阅][↻ 全部更新] + 订阅卡列表。
// 卡片：名称+“当前”chip / 元信息+更新时间 / 流量条（>85% warn 色）/ 到期 foot /
// 操作按钮（更新 / 改名[改名 sheet 预填现名，§14 R1] / 设为当前[非当前] / 删除[danger]）。
// 删除 confirm 用 SwiftUI .alert（GeneralPane 同款选型：窗口句柄取 NSAlert
// 不可靠）；删 current 直接传 --force 且文案含裸内核态警示（UI 二次确认语义）。
// WHY 不自建 ScrollView：宿主 ShellSettingsView.paneContent 已提供整页滚动
// （GeneralPane 同口径），嵌套 ScrollView 在固定窗口内无法滚动。

struct SubscriptionsPane: View {
    @ObservedObject var model: SubsModel
    @Environment(\.colorScheme) private var scheme
    @State private var showAddSheet = false
    /// 删除确认目标（nil=无；SwiftUI .alert 的 isPresented 由其派生）
    @State private var deleteTarget: SubsCardEntry?
    /// 重命名目标（nil=无；RenameSheet 的 isPresented 由其派生，§14 R1）
    @State private var renameTarget: SubsCardEntry?
    /// 卡片共享日期 formatter（@State 免逐卡重造）
    @State private var dateFormatter = SubsPaneLogic.makeCardDateFormatter()

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            header
            if !model.available {
                Text("tradewind CLI 未找到，订阅管理不可用")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.muted(scheme))
            } else {
                buttonRow
                cardList
            }
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 20)
        .frame(maxWidth: .infinity, alignment: .leading)
        .sheet(isPresented: $showAddSheet) {
            AddSubSheet(model: model)
        }
        .sheet(isPresented: Binding(
            get: { renameTarget != nil },
            set: { if !$0 { renameTarget = nil } }
        )) {
            if let target = renameTarget {
                RenameSheet(entry: target, model: model)
            }
        }
        .alert(deleteTitle, isPresented: Binding(
            get: { deleteTarget != nil },
            set: { if !$0 { deleteTarget = nil } }
        )) {
            Button("删除", role: .destructive) {
                if let target = deleteTarget {
                    Task { await model.delete(target) }
                }
                deleteTarget = nil
            }
            Button("取消", role: .cancel) { deleteTarget = nil }
        } message: {
            Text(deleteMessage)
        }
    }

    // MARK: 头部与按钮行

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("订阅管理").font(.system(size: 17, weight: .bold))
            Text("订阅原文件保持只读，个性化修改经增强链叠加。")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
        }
    }

    private var buttonRow: some View {
        HStack(spacing: 8) {
            primaryButton("＋ 添加订阅") { showAddSheet = true }
                .disabled(model.busy)
            FootButton {
                model.taskUpdateAll()
            } label: {
                Text("↻ 全部更新")
            }
            .disabled(model.busy)
            Spacer()
        }
    }

    /// 品牌主按钮（demo .btn.primary：brand 底白字；FootButton 底色不可换故本地实现）
    private func primaryButton(_ title: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Text(title)
                .font(.system(size: 12.5))
                .padding(.vertical, 5)
                .padding(.horizontal, 14)
                .background(RoundedRectangle(cornerRadius: Theme.radiusControl)
                    .fill(Theme.brand(scheme)))
                .foregroundStyle(Color.white)
        }
        .buttonStyle(.plain)
    }

    // MARK: 卡片列表

    @ViewBuilder
    private var cardList: some View {
        if model.loadFailed && model.subs.isEmpty {
            Text("订阅列表读取失败，请关闭窗口后重开")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
        } else if model.subs.isEmpty {
            Text("暂无订阅，点击「＋ 添加订阅」导入 URL 订阅")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
        } else {
            VStack(spacing: 10) { // demo .sub-card + .sub-card margin-top 10
                ForEach(model.subs, id: \.uid) { entry in
                    SubCardView(entry: entry, dateFormatter: dateFormatter,
                                onUpdate: { model.taskUpdate(entry) },
                                onRename: { renameTarget = entry },
                                onUse: { model.taskUse(entry) },
                                onDelete: { deleteTarget = entry })
                        .disabled(model.busy)
                }
            }
        }
    }

    // MARK: 删除确认（删 current 文案含裸内核态警示）

    private var deleteTitle: String {
        deleteTarget?.current == true ? "删除当前订阅？" : "删除订阅？"
    }

    private var deleteMessage: String {
        guard let target = deleteTarget else { return "" }
        if target.current {
            return "「\(target.name)」是当前订阅，删除后 Tradewind 将处于裸内核态（纯模板配置），"
                + "请导入或 use 订阅。"
        }
        return "「\(target.name)」的索引与文件将被移除（其余配置不受影响）。"
    }
}

// MARK: - 订阅卡（demo .sub-card：mat-card 底 + hair 描边 + current 品牌描边）

private struct SubCardView: View {
    let entry: SubsCardEntry
    let dateFormatter: DateFormatter
    let onUpdate: () -> Void
    let onRename: () -> Void
    let onUse: () -> Void
    let onDelete: () -> Void
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            topRow
            trafficBlock
            footRow
        }
        .padding(.vertical, 13)
        .padding(.horizontal, 14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Theme.cardBackground(scheme))
        .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
        .overlay(currentBorder)
    }

    /// 当前订阅：inset 1.5px brand 55% 描边（demo .sub-card.current）
    private var currentBorder: some View {
        RoundedRectangle(cornerRadius: Theme.radiusCard)
            .strokeBorder(entry.current
                ? AnyShapeStyle(Theme.brand(scheme).opacity(0.55))
                : AnyShapeStyle(Color.primary.opacity(0.08)),
                lineWidth: entry.current ? 1.5 : 1)
    }

    private var topRow: some View {
        HStack(alignment: .top, spacing: 8) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(entry.name)
                        .font(.system(size: 13.5, weight: .semibold))
                    if entry.current { currentChip }
                }
                Text(metaLine)
                    .font(.system(size: 11))
                    .foregroundStyle(Theme.muted(scheme))
            }
            Spacer(minLength: 8)
            actionButtons
        }
    }

    private var currentChip: some View {
        Text("当前")
            .font(.system(size: 10, weight: .medium))
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(Capsule().fill(Theme.brand(scheme).opacity(0.18)))
            .foregroundStyle(Theme.brand(scheme))
    }

    /// 元信息行（demo :771 口径：host + 更新时间；c3 保真补齐——Go ProfileJSON 已加
    /// url 字段）。host 从 url 提取；url 缺省（file 型/旧数据）回退 type。
    private var metaLine: String {
        let updated = SubsPaneLogic.dateText(entry.updated, formatter: dateFormatter) ?? "未知"
        let host = entry.url.flatMap { URL(string: $0)?.host } ?? ""
        let source = host.isEmpty ? entry.type : host
        return "\(source) · 更新于 \(updated)"
    }

    private var actionButtons: some View {
        HStack(spacing: 6) { // demo .sub-actions gap 6
            FootButton(horizontalPadding: 10) { onUpdate() } label: { Text("↻ 更新") }
            // ✎ 改名（§14 R1）：纯索引写（name 不进合成配置），改名 sheet 预填现名
            FootButton(horizontalPadding: 10) { onRename() } label: { Text("✎ 改名") }
            if !entry.current {
                FootButton(horizontalPadding: 10) { onUse() } label: { Text("设为当前") }
            }
            FootButton(horizontalPadding: 10) { onDelete() } label: {
                Text("删除").foregroundStyle(Theme.bad) // demo .btn.danger
            }
        }
    }

    // MARK: 流量条（demo .traffic：meta 行 + 6px 圆角条）

    private var trafficBlock: some View {
        let used = SubsPaneLogic.usedBytes(upload: entry.extra.upload,
                                           download: entry.extra.download)
        let percent = SubsPaneLogic.trafficPercent(used: used, total: entry.extra.total)
        return VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("已用 \(SubsPaneLogic.trafficBytes(used)) / \(totalText)")
                Spacer()
                Text(SubsPaneLogic.percentText(percent))
            }
            .font(.system(size: 11))
            .foregroundStyle(Theme.secondary(scheme))
            if let percent {
                trafficBar(percent)
            }
        }
    }

    /// 流量条：6px 圆角条按容器宽比例填充（demo .bar height 6 + width:pct%）；
    /// >85% warn 渐变（demo .bar i.warn #b06800→warn），否则 brand 渐变。
    private func trafficBar(_ percent: Double) -> some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(Theme.controlBackground(scheme))
                Capsule()
                    .fill(SubsPaneLogic.isTrafficWarn(percent)
                            ? AnyShapeStyle(warnFill) : AnyShapeStyle(brandFill))
                    .frame(width: max(2, geo.size.width * percent / 100))
            }
        }
        .frame(height: 6)
    }

    private var brandFill: LinearGradient {
        LinearGradient(colors: [Theme.brandDeep(scheme), Theme.brand(scheme)],
                       startPoint: .leading, endPoint: .trailing)
    }

    private var warnFill: LinearGradient {
        LinearGradient(colors: [Color(.sRGB, red: 0xB0 / 255, green: 0x68 / 255, blue: 0, opacity: 1),
                                Theme.warn], // ok/warn/bad 为双主题同值的 static let
                       startPoint: .leading, endPoint: .trailing)
    }

    private var totalText: String {
        entry.extra.total > 0 ? SubsPaneLogic.trafficBytes(entry.extra.total) : "不限量"
    }

    private var footRow: some View {
        HStack(spacing: 14) { // demo .sub-foot gap 14
            let expire = SubsPaneLogic.dateText(entry.extra.expire, formatter: dateFormatter)
            Text("到期：\(expire ?? "未知")")
            Text("来源：subscription-userinfo 响应头")
        }
        .font(.system(size: 11))
        .foregroundStyle(Theme.muted(scheme))
    }
}

// MARK: - 添加订阅 sheet（demo .sheet：420px + URL/名称 + 取消/导入）

private struct AddSubSheet: View {
    @ObservedObject var model: SubsModel
    @Environment(\.dismiss) private var dismiss
    @Environment(\.colorScheme) private var scheme
    @State private var url = ""
    @State private var name = ""
    @State private var urlInvalid = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("添加订阅")
                .font(.system(size: 14, weight: .bold))
                .padding(.bottom, 8)
            fieldLabel("订阅链接（含 token，将以 600 权限存储）")
            TextField("https://example.com/api/v1/client/subscribe?token=…", text: $url)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12))
            fieldLabel("名称")
            TextField("我的机场", text: $name)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12))
            if urlInvalid {
                Text("请输入订阅链接")
                    .font(.system(size: 11))
                    .foregroundStyle(Theme.bad)
            }
            HStack {
                Spacer()
                Button("取消") { dismiss() }
                Button("导入") { Task { await submit() } }
                    .disabled(model.busy)
            }
            .padding(.top, 16)
        }
        .padding(18)
        .frame(width: 420)
    }

    private func fieldLabel(_ text: String) -> some View {
        Text(text)
            .font(.system(size: 12))
            .foregroundStyle(Theme.secondary(scheme))
            .padding(.top, 10)
            .padding(.bottom, 2)
    }

    /// 导入提交：URL 空校验（demo `if(!url){focus();return;}`）；成功收起 sheet，
    /// 失败留在 sheet（toast 由宿主窗口层展示）。
    private func submit() async {
        let trimmed = url.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            urlInvalid = true
            return
        }
        if await model.add(url: url, name: name) {
            dismiss()
        } else {
        }
    }
}

// MARK: - 重命名 sheet（M3-plan §14 R1；版式照 AddSubSheet：420px + 名称 + 取消/保存）

private struct RenameSheet: View {
    let entry: SubsCardEntry
    @ObservedObject var model: SubsModel
    @Environment(\.dismiss) private var dismiss
    @Environment(\.colorScheme) private var scheme
    @State private var name: String

    init(entry: SubsCardEntry, model: SubsModel) {
        self.entry = entry
        self.model = model
        _name = State(initialValue: entry.name) // 预填现名
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("重命名订阅")
                .font(.system(size: 14, weight: .bold))
                .padding(.bottom, 8)
            fieldLabel("名称")
            TextField("订阅名称", text: $name)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12))
            HStack {
                Spacer()
                Button("取消") { dismiss() }
                Button("保存") { Task { await submit() } }
                    .disabled(model.busy)
            }
            .padding(.top, 16)
        }
        .padding(18)
        .frame(width: 420)
    }

    private func fieldLabel(_ text: String) -> some View {
        Text(text)
            .font(.system(size: 12))
            .foregroundStyle(Theme.secondary(scheme))
            .padding(.top, 10)
            .padding(.bottom, 2)
    }

    /// 保存提交：空名前置校验（toast 不落 CLI，sheet 留存）；成功收起 sheet。
    private func submit() async {
        if await model.rename(entry, to: name) {
            dismiss()
        }
    }
}

// MARK: - 动作桥接（Task 包装 async 模型调用；拆出以便单测对位与 body 瘦身）

extension SubsModel {
    func taskUpdate(_ entry: SubsCardEntry) {
        Task { await update(entry) }
    }

    func taskUse(_ entry: SubsCardEntry) {
        Task { await use(entry) }
    }

    func taskUpdateAll() {
        Task { await updateAll() }
    }
}
