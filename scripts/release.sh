#!/bin/bash
# Tradewind 发版脚本（批③ B3-2，batch3-plan §4.2）：构建 → 打包（m3b5 形态）→
# sha256 sidecar →（--upload）GitHub Release →（默认）本机守护换装 + app 重启。
#
# 用法：
#   scripts/release.sh v0.5.0-m3c1                  # 构建+打包+本机换装
#   scripts/release.sh v0.5.0-m3c1 --upload         # 另上传 Release（Pre-release）+ 资产
#   scripts/release.sh v0.5.0-m3c1 --upload --no-deploy  # 纯打包+上传，不动本机
#
# 打包形态（与 m3b5 tarball 逐条目同构，flat 根）：
#   ./tradewind  ./Tradewind.app/  ./ui/  ./bin/{mihomo,mihomo.sha256}  ./NOTICE.txt
# sidecar 资产名 = tradewind-<ver>-darwin-arm64.tar.gz.sha256，
# 内容 = <hex>  <tarball 文件名>（sha256sum 两空格；与 B1 self-update 消费端对齐）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LAUNCH_AGENT_LABEL="io.github.chaojimaimi.tradewind.agent"

usage() {
  echo "usage: scripts/release.sh <version> [--upload] [--no-deploy]" >&2
  exit 2
}

[ $# -ge 1 ] || usage
VERSION="$1"
shift || true
case "$VERSION" in
  v[0-9]*) ;; # tag 必须带 v 前缀（CLI 版本注入/git describe 语义一致）
  *) echo "error: version must look like v0.5.0-m3c1, got '$VERSION'" >&2; exit 2 ;;
esac

UPLOAD=0
NO_DEPLOY=0
for arg in "$@"; do
  case "$arg" in
    --upload) UPLOAD=1 ;;
    --no-deploy) NO_DEPLOY=1 ;;
    *) usage ;;
  esac
done

# --- 1. 构建（CLI + app；版本注入单一来源，build.sh 同款 VERSION 语义） ----------
echo "==> build (CLI + app, version $VERSION)"
VERSION="$VERSION" "$REPO_ROOT/scripts/build.sh"
VERSION="$VERSION" "$REPO_ROOT/apps/menubar/build.sh"

# --- 2. 打包（staging；ui/ 与内核取本机数据目录已部署副本——known-good 且 sidecar 已验）---
STAGE="$REPO_ROOT/dist/pkg-$VERSION"
DATA_DIR="${TRADEDWIND_HOME:-$HOME/Library/Application Support/tradewind}"
echo "==> stage $STAGE (ui/bin from $DATA_DIR)"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/ui"
cp "$REPO_ROOT/dist/tradewind" "$STAGE/tradewind"
cp -R "$REPO_ROOT/apps/menubar/dist/Tradewind.app" "$STAGE/Tradewind.app"
[ -f "$DATA_DIR/bin/mihomo" ] || { echo "error: $DATA_DIR/bin/mihomo missing" >&2; exit 1; }
[ -d "$DATA_DIR/ui" ] || { echo "error: $DATA_DIR/ui missing" >&2; exit 1; }
cp "$DATA_DIR/bin/mihomo" "$DATA_DIR/bin/mihomo.sha256" "$STAGE/bin/"
cp -R "$DATA_DIR/ui/." "$STAGE/ui/"

# 内核版本探测（NOTICE 用；-v 首行取 vX.Y.Z）
MIHOMO_VER="$("$STAGE/bin/mihomo" -v 2>/dev/null | head -1 | grep -o 'v[0-9][0-9.]*' | head -1 || true)"
MIHOMO_VER="${MIHOMO_VER:-v1.19.34}"
cat > "$STAGE/NOTICE.txt" <<EOF
Tradewind $VERSION (darwin arm64)

内容：
  tradewind          CLI 控制面
  Tradewind.app      菜单栏壳（原生设置窗口/连接面板/快捷键/主题/Dock 集成）
  bin/mihomo         ${MIHOMO_VER}（该 tag 源码 GPLv3；主分支已转 MIT）
  ui/                metacubexd v1.275.2（MIT）
对应源码：
  mihomo: https://github.com/MetaCubeX/mihomo/releases/tag/${MIHOMO_VER}
  metacubexd: https://github.com/MetaCubeX/metacubexd/releases/tag/v1.275.2
本项目代码为 MIT（仓库 LICENSE）。
EOF

# --- 3. tarball + sha256 sidecar -------------------------------------------------
TARBALL="$REPO_ROOT/dist/tradewind-$VERSION-darwin-arm64.tar.gz"
TARBALL_NAME="$(basename "$TARBALL")"
SIDECAR="$TARBALL.sha256"
echo "==> tarball $TARBALL"
tar -czf "$TARBALL" -C "$STAGE" .
HEX="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
printf '%s  %s\n' "$HEX" "$TARBALL_NAME" > "$SIDECAR"
echo "==> sidecar $SIDECAR"
shasum -a 256 -c <<< "$(cat "$SIDECAR")" > /dev/null
echo "    sidecar verified ($HEX)"

# --- 4. --upload：查重（已存在只补资产 --clobber）→ create --prerelease ----------
if [ "$UPLOAD" -eq 1 ]; then
  if gh release view "$VERSION" --repo chaojimaimi/tradewind > /dev/null 2>&1; then
    echo "==> release $VERSION exists; uploading assets (--clobber)"
    gh release upload "$VERSION" "$TARBALL" "$SIDECAR" --repo chaojimaimi/tradewind --clobber
  else
    echo "==> gh release create $VERSION (prerelease)"
    gh release create "$VERSION" "$TARBALL" "$SIDECAR" \
      --repo chaojimaimi/tradewind \
      --prerelease \
      --title "Tradewind $VERSION" \
      --notes "Pre-release $VERSION。

资产：
  tradewind-$VERSION-darwin-arm64.tar.gz（整包：CLI + Tradewind.app + bin/mihomo + ui/ + NOTICE）
  $TARBALL_NAME.sha256（sha256 sidecar；self-update 消费）

升级：\`tradewind version --check\` → \`tradewind self-update\`（CLI）；\`self-update --core\` 升级内核（失败自动回滚）。"
  fi
fi

# --- 5. 本机换装（--no-deploy 跳过；两坑铁律：rm+cp 新 inode、kickstart 无 -k）----
if [ "$NO_DEPLOY" -eq 1 ]; then
  echo "==> --no-deploy: skip daemon swap (本机保持旧版；可立即 tradewind version --check 验证远端可见)"
  exit 0
fi

echo "==> deploy: stop → rm+cp → kickstart → health probe"
tradewind stop || true
# rm+cp 新 inode（严禁 cp 原地覆写运行中二进制——SIGKILL exit 137）
INSTALL_PATH="/opt/homebrew/bin/tradewind"
rm -f "$INSTALL_PATH"
cp "$REPO_ROOT/dist/tradewind" "$INSTALL_PATH"
chmod 0755 "$INSTALL_PATH"
# 换装后版本自证（防拷错产物）
INSTALLED_VER="$("$INSTALL_PATH" version | head -1 | awk '{print $2}')"
if [ "$INSTALLED_VER" != "$VERSION" ]; then
  echo "error: installed binary reports $INSTALLED_VER, want $VERSION" >&2
  exit 1
fi
# kickstart 需在 rm+cp 之后；无 -k（对 wrapper 模式 -k 是 no-op）
launchctl kickstart "gui/$(id -u)/$LAUNCH_AGENT_LABEL"
# health 探测：status --json 退出码 0（desired=running 且内核健康）≤30s
echo "==> health probe"
probe=0
while [ "$probe" -lt 30 ]; do
  if tradewind status --json > /dev/null 2>&1; then
    echo "==> deployed $VERSION; core healthy"
    break
  fi
  probe=$((probe + 1))
  if [ "$probe" -ge 30 ]; then
    echo "error: core did not become healthy within 30s (check launchd.err.log)" >&2
    exit 1
  fi
  sleep 1
done
# app 重启（build.sh 已 pkill 旧实例；bundle 已重建并 ad-hoc 签名）
open "$REPO_ROOT/apps/menubar/dist/Tradewind.app"
echo "==> release $VERSION done (tarball+sidecar in dist/)"
