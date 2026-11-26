import Foundation

/// 功能开关的单一事实源（M3 batch6 release-gate）。
///
/// 开关只决定一项能力**对用户是否可见、可达**；被它隐藏的能力，其模型 / 桥接 / 解码层
/// 有意保留（暂时不可达的代码），加固完成后置 true 即原样重新启用。
enum FeatureFlags {
    /// 特权 TUN 模式（root 服务托管内核）是否对用户开放：popover 开关组的 TUN 行与
    /// 设置窗口「网络代理」区的 TUN 行，仅当本开关为 true 时才进入渲染。
    ///
    /// WHY 已置 true（批⑥b 阶段二，2026-11-26）：R16 提权边界已闭环（批⑥b c1–c8，
    /// docs/plans/batch6-plan.md §9）——root 内核工作目录收进锚目录内 root 属主的
    /// service-data/（不再读用户可写目录），安装脚本带摘要绑定 + 执行自测 + 候选过滤；
    /// §8 真机验证 A1–A15 全部通过后用户确认开放，CLI 侧联锁
    /// （TRADEDWIND_ALLOW_INSECURE_TUN=1）与 --allow-insecure-tun 确认旗标已随开放
    /// 一并移除，TUN 成为正常功能。
    ///
    /// includeTUN 的间接层（`var includeTUN = FeatureFlags.privilegedTUN`）有意保留：
    /// 测试以它为参数注入 false/true，对"隐藏/显示"两态做渲染结构对比。
    ///
    /// 不受本开关约束：`PanelModel.intake` 仍计入 TUN（见其注释）——开关管的是"用户
    /// 能否点 TUN"，不是"本机是否实际在走 TUN"。
    static let privilegedTUN = true
}
