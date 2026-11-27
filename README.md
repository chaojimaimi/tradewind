# Tradewind · 信风

<p align="center">
  <img src="apps/menubar/assets/brand-mark-mono.svg" width="120" alt="Tradewind 品牌标形">
</p>

<h1 align="center">Tradewind · 信风</h1>

<p align="center">
  <a href="#全部子命令"><img alt="status" src="https://img.shields.io/badge/status-M3%20%E4%BD%93%E9%AA%8C%E5%AE%8C%E5%96%84-2FB6DC"></a>
  <img alt="platform" src="https://img.shields.io/badge/platform-macOS%2013%2B-lightgrey">
  <img alt="core" src="https://img.shields.io/badge/core-mihomo%20v1.19.34-4FDCE4">
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-blue"></a>
</p>

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

### 公开安装（Release 下载，免构建）

无需克隆与构建，直接从 [Releases](https://github.com/chaojimaimi/tradewind/releases/latest) 下载
`tradewind-<版本>-darwin-arm64.tar.gz` 并解压安装：

```bash
tar -xzf tradewind-<版本>-darwin-arm64.tar.gz
mkdir -p /opt/homebrew/bin && cp tradewind /opt/homebrew/bin/   # 或 /usr/local/bin（PATH 内即可）
cp -R Tradewind.app /Applications/
mkdir -p ~/Library/Application\ Support/tradewind
cp -R bin ui ~/Library/Application\ Support/tradewind/          # 内核（含 sha256 sidecar）+ 面板资源落数据目录
```

1. **首次打开 Gatekeeper 放行**：签名无公证，直接双击会被拦截——到**系统设置 →
   隐私与安全性 → 仍要打开**（macOS 15 起右键「打开」已不能绕过，见 DESIGN §7.7）。
2. 启动：点击菜单栏 Tradewind 图标，或终端 `tradewind start`（Ctrl+C 优雅退出并还原系统代理）。
3. 升级：`tradewind self-update`（CLI+内核自动回滚；app 内关于页"检查更新"由 Sparkle 托管）。

注：`version --check` / `self-update` 公开仓匿名可用（60 req/h/IP）；私有仓场景配置
`release_token`：`gh auth token > ~/Library/Application\ Support/tradewind/release_token && chmod 600 ~/Library/Application\ Support/tradewind/release_token`。

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

| 命令                                      | 说明                                       |
| ----------------------------------------- | ------------------------------------------ |
| `add <url> \| --file`                     | 导入订阅并置为当前                         |
| `update [UID\|current\|all]`              | 手动更新订阅（remote 型）                  |
| `start [--no-sysproxy]`                   | 前台运行（冒烟/开发用 no-sysproxy 隔离）   |
| `stop`                                    | 停止并精确还原系统代理                     |
| `status [--json]`                         | 概览；`--json` 机器可读（壳专用）          |
| `nodes [GROUP]` / `select <GROUP> <NODE>` | 节点查看/切换                              |
| `delay [GROUP]`                           | 延迟测试（并发 ≤8）                        |
| `mode [rule\|global\|direct]`             | 出站模式查看/热切换（崩溃重启不回退）      |
| `sysproxy on\|off`                        | 系统代理开关（幂等；on 需内核运行）        |
| `list`                                    | 订阅表（uid/名称/类型/更新时间/流量/到期） |
| `use <UID\|NAME>`                         | 切换当前订阅（快照回写 + 热重载）          |
| `autostart on\|off\|status`               | LaunchAgent 开机自启（崩溃自动拉起）       |
| `panel [--url]`                           | metacubexd 面板；`--url` 仅打印入口        |
| `version`                                 | 版本                                       |

命令分组补充（M3 批② c1）：`settings [get [--json] | set <key> <value> | reset]` 应用设置
（白名单键 mixed_port/gateway_port/panel_enabled/allow_lan/log_retention_days；写后热重载，
mixed_port 变更且系统代理已应用时自动迁移到新端口）；`subs delete <UID|NAME> [--force]` 删除
订阅（当前订阅需 `--force`，删除后落裸内核态）、`subs interval <UID|NAME> <off|1|12|24>`
按订阅设置自动更新间隔（off 为真实关闭）；`list --json` 与 `autostart status --json` 输出
机器可读单行 JSON，与 `settings get --json` 一同为菜单栏壳的数据源。

## 菜单栏壳（Tradewind.app）

功能：三态图标（品牌色=运行+有接入 / 灰=停止或无接入）、popover 主面板
（状态/当前节点/模式切换/系统代理开关/速率图/策略组与节点切换/测速）、
右键图标退出壳（内核继续运行）。

**节点旗帜映射**（M3 批①）：节点名旁的国旗 emoji 由 `CountryFlag` 四级匹配生成——
① 名称已含旗帜 emoji → 直通；② 中文区名最长匹配（含 印尼/沙特 等短形别名）；
③ 英文：两字码仅整 token 匹配（`US-01`→🇺🇸，但 `AUSTIN-01`→🌐 禁子串误判）+
英文城市名精确大小写子串（`Tokyo-01`→🇯🇵）；④ 兜底 🌐。误匹配仅为观感层面，无功能影响。

**视觉保真②**（M3 批①.5）：popover 底色改显式双主题 tokens（冷白 `#F2F6F8` / 冷黑
`#26292E`，近似 demo 半透明+blur 合成）；出站模式分段与底部按钮自绘对齐 demo
（胶囊分段容器/选中钮三层色阶、底部等宽按钮、红⏻——⏻ 保留 SF Symbol 防字体回退）；
节点列表修复确定性高度（≤5 行全显、>5 行固定 168pt 滚动——修复 NSPopover
fittingSize 路径下 maxHeight 收缩为 0 致整块不渲染）；延迟徽标按延迟色阶着色
（<100 绿 / 100-299 橙 / ≥300 红，用户拍板偏离 demo 固定绿）；速率符号改 ▼▲ 文本、
今日流量加 ↓ 前缀。

### 安装与启动

1. 将 `Tradewind.app` 拷入 `/Applications`（或任意位置）。
2. **Gatekeeper 放行**：首次打开请到**系统设置 → 隐私与安全性 → 仍要打开**
   （自签身份无公证，直接双击会被拦截；macOS 15 起右键「打开」已不能绕过 Gatekeeper；
   自用可接受，见 DESIGN §7.7）。
3. 壳要求本机可寻到 `tradewind` CLI：装到 `/opt/homebrew/bin` 或 `/usr/local/bin`，
   或设环境变量 `TRADEDWIND_CLI=/path/to/tradewind` 后从终端启动壳。
4. 数据面凭据来自 `tradewind status --json` 的 `panel_url`（面板网关 token 入口）。

已知限制：壳关闭期间今日流量欠计。

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
