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
    /// P3-6：当前订阅 type（file 型 interval 下拉禁用）经 subsModel 卡片数据判定
    @ObservedObject var subsModel: SubsModel
    /// 系统代理形态编辑模型（批④ §3.4）：ShellSettingsView @StateObject 装配注入
    /// （toast 经窗口层 toastLayer 链展示）
    @ObservedObject var proxyModeModel: ProxyModeModel
    @Environment(\.colorScheme) private var scheme
    @State private var confirmReset = false
    /// 壳偏好（增强 #1，M3-plan §11.7）：@AppStorage 直写 UserDefaults 即时生效，
    /// 写后 post prefChangedNotification——App 层察觉后刷新/清空菜单栏速率 title
    /// （不走 SettingsModel/CLI 写链路；m3b2 返修：键含点 KVO 不送达，通知见
    /// ShellPrefs.prefChangedNotification 注释）
    @AppStorage(ShellPrefs.menubarRateKey) private var menubarRate = false
    /// 界面主题（§12 c2）：@AppStorage 写 tradewind.shell.appearance，写后 post
    /// 通知，App 层 applyAppearance 即时切换 popover/设置窗口 NSAppearance
    @AppStorage(ShellPrefs.appearanceKey) private var appearanceRaw = ShellPrefs.ShellAppearance.system.rawValue
    /// 静默启动（§13 R1 用户拍板做实）：@AppStorage 写 tradewind.shell.silent_launch，
    /// 写后 post 通知（App 层运行期无动作，语义=下次启动生效）
    @AppStorage(ShellPrefs.silentLaunchKey) private var silentLaunch = true

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            header
            // 外观小节无条件渲染（§12 c2 评审 P3）：纯壳偏好不依赖 CLI 快照，
            // CLI 缺失/加载失败仍可切主题
            appearanceSection
            shellSection
            if !model.available {
                Text("tradewind CLI 未找到，设置不可用")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.muted(scheme))
            } else if let settings = model.settings {
                portSection(settings)
                proxySection
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
        // 系统代理形态数据装载（批④ §3.4：页面出现时拉 settings 快照）
        .task { await proxyModeModel.load() }
        // 壳偏好热变更投递（m3b2 返修）：@AppStorage 落盘后 App 层无 KVO 可依赖
        // （键含点），由本页唯一写入点 post 通知携带变更键名
        .onChange(of: appearanceRaw) { _ in
            NotificationCenter.default.post(name: ShellPrefs.prefChangedNotification,
                                            object: nil, userInfo: [ShellPrefs.appearanceKey: true])
        }
        .onChange(of: menubarRate) { _ in
            NotificationCenter.default.post(name: ShellPrefs.prefChangedNotification,
                                            object: nil, userInfo: [ShellPrefs.menubarRateKey: true])
        }
        .onChange(of: silentLaunch) { _ in
            NotificationCenter.default.post(name: ShellPrefs.prefChangedNotification,
                                            object: nil, userInfo: [ShellPrefs.silentLaunchKey: true])
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

    /// P3-6（F13）：file 型订阅无自动更新语义（Go dueForUpdate 恒 false，interval
    /// 落盘即静默无效）——下拉禁用；无当前订阅/未装载保持可用（保守口径）。
    private var intervalDisabled: Bool {
        GeneralPaneLogic.intervalDisabled(
            type: subsModel.subs.first(where: { $0.current })?.type)
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
        .disabled(model.busy || intervalDisabled)
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
            // 静默启动（§13 R1 用户拍板做实，废止原禁用占位偏差 #2）：关=启动后
            // 自动打开设置窗口；行归属 launchSection（仅 settings 快照分支渲染）
            // 为有意取舍——语义属"启动"分组，M3-plan §13.3 注明
            SettingsRow(label: "静默启动", subtitle: "关闭后每次启动 Tradewind 自动打开设置窗口") {
                SwitchPill(isOn: $silentLaunch, small: true, accessibilityLabel: "静默启动")
            }
            rowDivider
            SettingsRow(label: "日志保留", subtitle: "按天数自动清理滚动日志") {
                NumberValueField(model: model, field: .logRetention, snapshot: settings)
            }
        }
    }

    // MARK: 壳偏好（M3-plan §11.2-1 双轨制：UserDefaults 直写，不经 SettingsModel/CLI）

    /// 外观小节（§12 c2）：界面主题三档 Picker，即写即生效（App 层 KVO→applyAppearance）。
    /// 不受 model.busy 影响（壳偏好无 CLI 写编排）。
    private var appearanceSection: some View {
        SettingsSection(title: "外观") {
            SettingsRow(label: "界面主题", subtitle: "跟随系统 / 浅色 / 深色（即时生效）") {
                Picker("", selection: $appearanceRaw) {
                    Text("跟随系统").tag(ShellPrefs.ShellAppearance.system.rawValue)
                    Text("浅色").tag(ShellPrefs.ShellAppearance.light.rawValue)
                    Text("深色").tag(ShellPrefs.ShellAppearance.dark.rawValue)
                }
                .pickerStyle(.menu)
                .frame(width: 116)
                .accessibilityLabel("界面主题")
            }
        }
    }

    /// 增强 #1/#2（M3-plan §11.7，demo 均无此字段）：菜单栏速率 opt-in 开关 +
    /// 启停快捷键只读展示。两行不受 model.busy 影响（壳偏好无 CLI 写编排）。
    /// P3-3 降级口径统一（批④ §4）：CLI 缺失 → 区块可见 + 写控件禁用 + 说明行
    ///（SubscriptionsPane 降级口径一致——区块不再随 CLI 缺失整段消失）。
    private var shellSection: some View {
        SettingsSection(title: "菜单栏与快捷键") {
            SettingsRow(label: "菜单栏速率",
                        subtitle: !model.available
                            ? "tradewind CLI 未找到：壳偏好暂停编辑（外观区块不受影响）"
                            : "状态栏图标旁显示实时 ↓↑ 速率，关闭仅保留图标") {
                SwitchPill(isOn: $menubarRate, small: true, accessibilityLabel: "菜单栏速率")
                    .disabled(!model.available)
            }
            rowDivider
            // 增强 #2：固定 ⌥⇧T 只读——改键 UI 裁剪（§11.1-H），hotkey_enabled 写逻辑
            // 保留但 UI 不给开关，避免热卸载歧义（M3-plan §11.2-7）
            SettingsRow(label: "启停快捷键",
                        subtitle: "全局切换 Tradewind 启动/停止") {
                Text("⌥⇧T")
                    .font(.system(size: 11).monospaced())
                    .foregroundStyle(.secondary)
            }
        }
    }

    // MARK: 系统代理（批④ §3.4：PAC 模式 + 绕过域名）

    /// PAC 模式开关 + 绕过域名编辑行（走 ProxyModeModel settingsGet/set 链——双轨制
    /// 不走 @AppStorage；写编排含 applied 态形态翻转重放与 gateway_port 仅警告联动，
    /// 由 Go 侧编排）。
    private var proxySection: some View {
        SettingsSection(title: "系统代理") {
            SettingsRow(label: "PAC 模式",
                        subtitle: "PAC 需面板网关（panel_enabled）；绕过默认 localhost/内网段") {
                SwitchPill(isOn: Binding(
                    get: { proxyModeModel.pacMode },
                    set: { newValue in Task { await proxyModeModel.setPacMode(newValue) } }
                ), small: true, accessibilityLabel: "PAC 模式")
                .disabled(proxyModeModel.busy)
            }
            rowDivider
            SettingsRow(label: "绕过域名",
                        subtitle: "逗号分隔；留空恢复缺省（默认 localhost/内网段）") {
                BypassValueField(model: proxyModeModel)
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


/// 绕过域名编辑行（批④ §3.4）：逗号分隔 TextField，占位=当前生效值（未设置展示
/// Go 缺省集）；失焦/回车提交（NumberValueField 同款交互），成功回显权威态、
/// 失败回显现值。
private struct BypassValueField: View {
    @ObservedObject var model: ProxyModeModel
    @State private var text: String = ""
    @FocusState private var focused: Bool

    var body: some View {
        TextField(BypassEditorLogic.effectiveDisplay(current: model.currentBypass), text: $text)
            .focused($focused)
            .textFieldStyle(.roundedBorder)
            .font(.system(size: 12).monospaced())
            .frame(width: 220)
            .onSubmit { Task { await commit() } } // 回车提交
            .onChange(of: focused) { isFocused in
                if !isFocused { Task { await commit() } } // 失焦提交
            }
            .onChange(of: model.currentBypass) { current in
                // 权威态被其他路径改写（写入回拉/失败回退）时同步显示；编辑中不抢焦点
                if !focused { text = BypassEditorLogic.effectiveDisplay(current: current) }
            }
            .disabled(model.busy)
            .accessibilityLabel("绕过域名")
    }

    private func commit() async {
        await model.commitBypass(text)
        // 成功/失败后回显权威值（unchanged 分支无写入，回显等价无害）
        text = BypassEditorLogic.effectiveDisplay(current: model.currentBypass)
    }
}
