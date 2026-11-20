import SwiftUI

// MARK: - 通用小节/行装配（demo .sec/.sec-t/.fgroup/.frow 语义，设置窗口各页共用）

/// 小节：大写间距标题（.sec-t 11px bold）+ 卡片组容器（.fgroup mat-card 圆角 10 + 描边）。
struct SettingsSection<Content: View>: View {
    let title: String
    @ViewBuilder var content: () -> Content
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            Text(title)
                .font(.system(size: 11, weight: .bold))
                .tracking(0.5)
                .foregroundStyle(Theme.muted(scheme))
            VStack(spacing: 0) { content() }
                .background(Theme.cardBackground(scheme))
                .clipShape(RoundedRectangle(cornerRadius: Theme.radiusCard))
                .overlay(
                    RoundedRectangle(cornerRadius: Theme.radiusCard)
                        .strokeBorder(Color.primary.opacity(0.08), lineWidth: 1)
                )
        }
    }
}

/// 设置行（.frow）：左主标签+可选副标签（flex 1），右控件；行内 padding 9/12。
/// boldLabel=链槽位等 demo 用 <b> 的形态；调用方以 Divider() 分隔相邻行（.frow+.frow）。
struct SettingsRow<Control: View>: View {
    let label: String
    var subtitle: String?
    var boldLabel = false
    @ViewBuilder var control: () -> Control
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 1) {
                Text(label)
                    .font(.system(size: 13, weight: boldLabel ? .semibold : .regular))
                if let subtitle {
                    Text(subtitle)
                        .font(.system(size: 11))
                        .foregroundStyle(Theme.muted(scheme))
                }
            }
            Spacer(minLength: 12)
            control()
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
    }
}

// MARK: - 页 3 通用（M3-plan §11.3，demo :445-470）

/// 通用页：数据源 settings get --json + autostart status --json（+ list --json 间隔档）。
/// 写链路：number 失焦/回车提交且值未变化不写（评审 P2）；开关乐观回显失败回退；
/// 恢复默认 NSAlert 确认后调 settings reset 并重拉。desc 按实际语义改写：
/// mixed_port 热重载生效；gateway_port/panel_enabled 需重启 Tradewind。
struct GeneralPane: View {
    @ObservedObject var model: SettingsModel
    @Environment(\.colorScheme) private var scheme
    @State private var confirmReset = false

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            header
            if !model.available {
                Text("tradewind CLI 未找到，设置不可用")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.muted(scheme))
            } else if let settings = model.settings {
                portSection(settings)
                subsSection(settings)
                launchSection(settings)
                resetSection
            } else if model.loadFailed {
                Text("设置读取失败，请关闭窗口后重开")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.muted(scheme))
            } else {
                Text("加载中…")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.muted(scheme))
            }
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 20)
        .frame(maxWidth: .infinity, alignment: .leading)
        .alert("恢复默认设置？", isPresented: $confirmReset) {
            Button("恢复", role: .destructive) {
                Task { await model.resetToDefaults() }
            }
            Button("取消", role: .cancel) {}
        } message: {
            Text("端口等设置将恢复默认值；混合端口变更会触发热重载与系统代理迁移。")
        }
    }

    // WHY SwiftUI .alert 而非 NSAlert.beginSheetModal：窗口句柄从视图树取不可靠
    // （accessory 态 keyWindow 漂移），.alert 自动附着宿主窗口且语义等价（模态确认）。

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("通用").font(.system(size: 17, weight: .bold))
            Text("修改将写入 settings.yaml：混合端口热重载生效；面板网关端口与面板开关需重启 Tradewind 后生效。")
                .font(.system(size: 12))
                .foregroundStyle(Theme.muted(scheme))
        }
    }

    // MARK: 三组字段

    private func portSection(_ settings: SettingsSnapshot) -> some View {
        SettingsSection(title: "端口与入站") {
            SettingsRow(label: "混合端口（HTTP + SOCKS5）",
                        subtitle: "mixed-port，本机回环 + 局域网可选") {
                NumberValueField(model: model, field: .mixedPort, snapshot: settings)
            }
            rowDivider
            SettingsRow(label: "面板网关端口",
                        subtitle: "metacubexd 经控制进程网关访问，token 入口 + 会话 cookie") {
                NumberValueField(model: model, field: .gatewayPort, snapshot: settings)
            }
            rowDivider
            SettingsRow(label: "允许局域网", subtitle: "允许局域网设备使用本代理") {
                SwitchPill(isOn: Binding(
                    get: { settings.allowLan },
                    set: { newValue in Task { await model.setAllowLan(newValue) } }
                ), small: true, accessibilityLabel: "允许局域网")
                .disabled(model.busy)
            }
        }
    }

    private func subsSection(_ settings: SettingsSnapshot) -> some View {
        SettingsSection(title: "订阅") {
            // 偏差 #3（M3-plan §11.7，仅登记不进 UI）：demo 全局 select 语义落为当前订阅 per-item
            SettingsRow(label: "自动更新间隔") {
                intervalPicker
            }
            rowDivider
            // 偏差 #1（M3-plan §11.7）：只读展示——UA 影响机场兼容面，可编辑延后；
            // 值与 Go fetch.UA 常量一致（clash.meta/1.19.34 (tradewind/0.1)）
            SettingsRow(label: "订阅 User-Agent",
                        subtitle: "默认 clash 兼容 UA，避免机场回退 base64 格式") {
                Text("clash.meta/1.19.34 (tradewind/0.1)")
                    .font(.system(size: 11).monospaced())
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var intervalPicker: some View {
        Picker("", selection: Binding(
            get: { model.intervalMode },
            set: { newValue in Task { await model.setInterval(newValue) } }
        )) {
            Text("每 24 小时").tag("24")
            Text("每 12 小时").tag("12")
            Text("每小时").tag("1")
            Text("关闭").tag("off")
        }
        .pickerStyle(.menu)
        .frame(width: 116)
        .disabled(model.busy)
        .accessibilityLabel("自动更新间隔")
    }

    private func launchSection(_ settings: SettingsSnapshot) -> some View {
        SettingsSection(title: "启动") {
            SettingsRow(label: "开机自启",
                        subtitle: "LaunchAgent · io.github.chaojimaimi.tradewind.agent") {
                SwitchPill(isOn: Binding(
                    get: { model.autostartOn },
                    set: { newValue in Task { await model.setAutostart(newValue) } }
                ), small: true, accessibilityLabel: "开机自启")
                .disabled(model.busy)
            }
            rowDivider
            // 偏差 #2（M3-plan §11.7）：禁用占位——壳 LSUIElement 恒静默，无对应开关语义；
            // 文案对齐 demo，禁用缘由入 tooltip 不入行副文
            SettingsRow(label: "静默启动", subtitle: "启动时不弹出任何窗口") {
                SwitchPill(isOn: .constant(false), small: true,
                           accessibilityLabel: "静默启动")
                    .disabled(true)
                    .help("壳为菜单栏常驻应用（LSUIElement），恒静默启动")
            }
            rowDivider
            SettingsRow(label: "日志保留", subtitle: "按天数自动清理滚动日志") {
                NumberValueField(model: model, field: .logRetention, snapshot: settings)
            }
        }
    }

    private var resetSection: some View {
        HStack {
            Spacer()
            Button("恢复默认") { confirmReset = true }
                .disabled(model.busy)
        }
    }

    /// 行分隔（demo .frow + .frow 的 0.5px hair；同 ToggleGroupCard 分隔口径）
    private var rowDivider: some View {
        Divider().overlay(Theme.muted(scheme).opacity(0.2))
    }
}

/// number 输入（demo .num/.num-w 84px 等宽数字）：失焦/回车提交（评审 P2），
/// 值未变化/非法不写；提交被拒（含失败回退）回显快照现值。
private struct NumberValueField: View {
    @ObservedObject var model: SettingsModel
    let field: SettingsNumberField
    let snapshot: SettingsSnapshot
    @State private var text: String
    @FocusState private var focused: Bool

    init(model: SettingsModel, field: SettingsNumberField, snapshot: SettingsSnapshot) {
        self.model = model
        self.field = field
        self.snapshot = snapshot
        _text = State(initialValue: String(field.currentValue(in: snapshot)))
    }

    var body: some View {
        TextField("", text: $text)
            .focused($focused)
            .textFieldStyle(.roundedBorder)
            .font(.system(size: 12).monospacedDigit())
            .frame(width: 84)
            .onSubmit { Task { await commit() } } // 回车提交
            .onChange(of: focused) { isFocused in
                if !isFocused { Task { await commit() } } // 失焦提交
            }
            .onChange(of: field.currentValue(in: snapshot)) { current in
                // 快照被其他路径改写（重置/回退）时同步显示；编辑中不抢输入焦点
                if !focused { text = String(current) }
            }
            .disabled(model.busy)
            .accessibilityLabel(field.rawValue)
    }

    private func commit() async {
        let accepted = await model.commitNumber(field, raw: text)
        if !accepted {
            // 未变化（无需动作）或非法/失败：回显现值传达"未生效"
            text = String(field.currentValue(in: snapshot))
        }
    }
}
