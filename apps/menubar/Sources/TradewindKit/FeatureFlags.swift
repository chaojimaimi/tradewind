import Foundation

/// 功能开关的单一事实源（M3 batch6 release-gate）。
///
/// 开关只决定一项能力**对用户是否可见、可达**；被它隐藏的能力，其模型 / 桥接 / 解码层
/// 有意保留（暂时不可达的代码），加固完成后置 true 即原样重新启用。
enum FeatureFlags {
    /// 特权 TUN 模式（root 服务托管内核）是否对用户开放：popover 开关组的 TUN 行与
    /// 设置窗口「网络代理」区的 TUN 行，仅当本开关为 true 时才进入渲染。
    ///
    /// WHY 默认 false：R16 提权边界尚未闭环——root 内核以用户可写的数据目录为工作
    /// 目录并读取用户可写的 runtime.yaml，内核的 geo/provider 下载会顺悬空符号链接在
    /// 任意路径创建文件，任何以当前账号运行的程序都可借它取得 root
    /// （docs/plans/batch6-plan.md §9 第六轮，已用真实内核 v1.19.34 实测确认）。
    /// v0.5.0-m3f1 因此以"特权服务保持关闭"发版。CLI 侧联锁已让 `tun on` /
    /// `tun install` / `tun reinstall` 默认拒绝，仅 `TRADEDWIND_ALLOW_INSECURE_TUN=1`
    /// 放行——壳端若仍露出开关，用户点了只会得到通用的"开启失败"提示。
    /// 批⑥b 加固完成后置 true。
    ///
    /// 有意保留、暂时不可达的代码（置 true 后重新启用，勿当死代码清理）：
    /// `ProxyModeModel.tunEnabled` / `ProxyModeModel.setTUN(_:)`、`CoreBridge.setTUN`、
    /// `StatusReport.Tun` 三态解码、`SettingsSnapshot.tunEnabled`、
    /// `PanelModel.pendingTUN` / `effectiveTUN` / `tunRunning` / `tunNote` / `setTUN(on:)`，
    /// 以及它们各自的测试。
    ///
    /// 不受本开关约束：`PanelModel.intake` 仍计入 TUN（见其注释）——开关管的是"用户
    /// 能否点 TUN"，不是"本机是否实际在走 TUN"。
    static let privilegedTUN = false
}
