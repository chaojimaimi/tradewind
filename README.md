# Tradewind · 信风

个人代理管理工具（macOS 优先）：Go 控制面 + 外挂 mihomo 内核 + Swift 菜单栏壳。
设计哲学：不自研代理内核，自建控制面；所有变更走"改配置 → 合成 → 热重载"单一管线。

- 设计文档：`docs/DESIGN.md`（仅本地留存）
- 内核：mihomo（外挂进程，锁版本，随发布物携带，许可独立）
- 许可：本项目代码 MIT；捆绑 mihomo 时附其许可与上游源码 tag 指引

## 安装

```bash
# 构建单二进制（版本注入）
scripts/build.sh                      # → dist/tradewind
# 构建菜单栏壳（SPM，零第三方依赖）
apps/menubar/build.sh                 # → apps/menubar/dist/Tradewind.app
```

发布物（GitHub Release）内含 `tradewind`、`Tradewind.app`、`bin/mihomo`、`ui/`（metacubexd）
与许可文件。

### 数据目录

`~/Library/Application Support/tradewind/`（测试/隔离用 `TRADEDWIND_HOME` 覆盖）。

## 快速上手（CLI）

```bash
tradewind add <订阅URL> | --file <path> [--name NAME]   # 导入订阅（校验通过才落盘）
tradewind start                                          # 前台运行；Ctrl+C 优雅退出（还原系统代理）
tradewind status                                         # 内核/系统代理/订阅概览
tradewind stop                                           # SIGTERM 运行实例
```

## 全部子命令

| 命令 | 说明 |
| --- | --- |
| `add <url> \| --file` | 导入订阅并置为当前 |
| `update [UID\|current\|all]` | 手动更新订阅（remote 型） |
| `start [--no-sysproxy]` | 前台运行（冒烟/开发用 no-sysproxy 隔离） |
| `stop` | 停止并精确还原系统代理 |
| `status [--json]` | 概览；`--json` 机器可读（壳专用） |
| `nodes [GROUP]` / `select <GROUP> <NODE>` | 节点查看/切换 |
| `delay [GROUP]` | 延迟测试（并发 ≤8） |
| `mode [rule\|global\|direct]` | 出站模式查看/热切换（崩溃重启不回退） |
| `sysproxy on\|off` | 系统代理开关（幂等；on 需内核运行） |
| `list` | 订阅表（uid/名称/类型/更新时间/流量/到期） |
| `use <UID\|NAME>` | 切换当前订阅（快照回写 + 热重载） |
| `autostart on\|off\|status` | LaunchAgent 开机自启（崩溃自动拉起） |
| `panel [--url]` | metacubexd 面板；`--url` 仅打印入口 |
| `version` | 版本 |

## 菜单栏壳（Tradewind.app）

功能：三态图标（品牌色=运行+有接入 / 灰=停止或无接入）、popover 主面板
（状态/当前节点/模式切换/系统代理开关/速率图/策略组与节点切换/测速）、
右键图标退出壳（内核继续运行）。

### 安装与启动

1. 将 `Tradewind.app` 拷入 `/Applications`（或任意位置）。
2. **Gatekeeper 放行**：首次打开请**右键点击 Tradewind.app → 打开 → 打开**
   （ad-hoc 签名无公证，直接双击会被拦截；自用可接受，见 DESIGN §7.7）。
3. 壳要求本机可寻到 `tradewind` CLI：装到 `/opt/homebrew/bin` 或 `/usr/local/bin`，
   或设环境变量 `TRADEDWIND_CLI=/path/to/tradewind` 后从终端启动壳。
4. 数据面凭据来自 `tradewind status --json` 的 `panel_url`（面板网关 token 入口）。

已知限制（M2）：壳关闭期间今日流量欠计；设置窗口/连接面板/启停快捷键/TUN/DNS 接管/
增强链/代理守护归 M3+。

## 安全说明

- 订阅 URL 与 secret 均以 600 权限存储，目录 700。
- 内核控制面仅 unix socket + 随机 secret；浏览器面板经网关 token 入口 + 会话 cookie。
- 系统代理写入前按网络服务备份，退出/崩溃后精确还原（绝不留指向死端口的代理）。

## 开发

```bash
go test ./...                      # Go 门禁（gofmt / vet / test）
cd apps/menubar && swift test      # 壳门禁（swift build + swift test）
scripts/smoke.sh                   # 六阶段 E2E（阶段③实接管系统代理并精确还原）
```
