#!/bin/bash
# Tradewind 发版脚本（批③ B3-2，batch3-plan §4.2；batch5 §3.3 Sparkle 资产链；
# batch7-plan §4.1 DMG 资产）：
# 构建 → 打包（m3b5 形态）→ sha256 sidecar → Sparkle 资产链（zip + EdDSA 签名 +
# appcast 单条目快照；工具/密钥缺失 → 警告跳过）→ DMG 一体化安装包（不依赖
# Sparkle 工具，恒产出；签名身份缺失 → 警告跳过签名）→（--upload）GitHub Release →
# （默认）本机守护换装 + app 重启。
#
# 用法：
#   scripts/release.sh v0.5.0-m3e1                  # 构建+打包+本机换装（默认正式版，无 Pre-release 标签）
#   scripts/release.sh v0.5.0-m3e1 --upload         # 另上传 Release + 资产（含 Sparkle 链产出）
#   scripts/release.sh v0.5.0-m3e1 --upload --no-deploy  # 纯打包+上传，不动本机
#   scripts/release.sh v0.5.0-m3e1 --pre            # 观察期小版本：重带 prerelease 标签
#                                                    #（配合 Go 侧两级过滤 F12：--pre 版不推普通 CLI 用户）
#   DRY_RUN=1 scripts/release.sh v0.5.0-m3e1 --upload   # 干跑：只打印计划，零副作用
#
# Sparkle 资产链（batch5 §3.3）：SPARKLE_BIN=Sparkle 预编译 zip 的 bin 目录（一次性
# 下载不入仓）；EdDSA 私钥 $HOME/.sparkle/tradewind.ed25519.priv（600）——任一缺失
# → 警告跳过整段链，tarball/sidecar 照常发。签名经 sign_update --ed-key-file
# （私钥不经 argv 红线）；appcast 单条目快照 + enclosure 钉 tag URL（壳内 feed 用
# latest/download 稳定 URL，分工防"feed 拉取与 zip 下载之间发布新版"竞态错配，评审 P2-2）。
#
# 打包形态（与 m3b5 tarball 逐条目同构，flat 根）：
#   ./tradewind  ./Tradewind.app/  ./ui/  ./bin/{mihomo,mihomo.sha256}  ./NOTICE.txt
# sidecar 资产名 = tradewind-<ver>-darwin-arm64.tar.gz.sha256，
# 内容 = <hex>  <tarball 文件名>（sha256sum 两空格；与 B1 self-update 消费端对齐）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LAUNCH_AGENT_LABEL="io.github.chaojimaimi.tradewind.agent"

usage() {
  echo "usage: scripts/release.sh <version> [--upload] [--no-deploy] [--pre]（DRY_RUN=1 干跑）" >&2
  exit 2
}

# generate_appcast 组装单条目 appcast.xml（batch5 §3.3）：签名值由调用方注入——
# 生产取官方 sign_update 输出，测试用占位（真私钥绝不进测试）。抽为独立函数供
# golden 测试直接调用（source 本脚本即得；main 仅 $0 直跑时执行）。
generate_appcast() {
  # WHY minimumSystemVersion 恒带 sparkle: 前缀（批⑤ code-reviewer P2-1）：
  # Sparkle 2.9.1 只按 sparkle:minimumSystemVersion 键取值（SUAppcastItem.m:644），
  # 裸元素静默失效=版本门槛不生效。XML 内不加注释——golden 逐字节对比。
  local version="$1" build="$2" ed_sig="$3" zip_len="$4" pubdate="$5" out="$6"
  cat > "$out" <<EOF
<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle" xmlns:dc="http://purl.org/dc/elements/1.1/">
  <channel>
    <title>Tradewind</title>
    <link>https://github.com/chaojimaimi/tradewind</link>
    <description>Tradewind macOS app 自动更新 feed</description>
    <language>en</language>
    <item>
      <title>Tradewind $version</title>
      <pubDate>$pubdate</pubDate>
      <sparkle:version>$build</sparkle:version>
      <sparkle:shortVersionString>$version</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>13.0</sparkle:minimumSystemVersion>
      <enclosure url="https://github.com/chaojimaimi/tradewind/releases/download/$version/Tradewind.app.zip" sparkle:edSignature="$ed_sig" length="$zip_len" type="application/octet-stream" />
    </item>
  </channel>
</rss>
EOF
}

main() {
  [ $# -ge 1 ] || usage
  VERSION="$1"
  shift || true
  case "$VERSION" in
    v[0-9]*) ;; # tag 必须带 v 前缀（CLI 版本注入/git describe 语义一致）
    *) echo "error: version must look like v0.5.0-m3e1, got '$VERSION'" >&2; exit 2 ;;
  esac

  UPLOAD=0
  NO_DEPLOY=0
  PRE=0
  for arg in "$@"; do
    case "$arg" in
      --upload) UPLOAD=1 ;;
      --no-deploy) NO_DEPLOY=1 ;;
      --pre) PRE=1 ;;
      *) usage ;;
    esac
  done

  # DRY_RUN 干跑（batch5 §5）：零副作用打印计划——flag 语义与资产清单可测
  #（无 --prerelease 默认 / 资产清单含 tarball+sidecar+dmg（DMG 不依赖 SPARKLE_BIN，
  # batch7-plan §4.1）/ Sparkle 工具在位时追加 zip+appcast / --upload 显式语义保持）
  if [ "${DRY_RUN:-0}" = "1" ]; then
    local assets="tradewind-$VERSION-darwin-arm64.tar.gz tradewind-$VERSION-darwin-arm64.tar.gz.sha256 tradewind-$VERSION-darwin-arm64.dmg tradewind-$VERSION-darwin-arm64.dmg.sha256"
    if [ -n "${SPARKLE_BIN:-}" ] && [ -x "$SPARKLE_BIN/sign_update" ] && [ -f "${EDDSA_PRIV:-$HOME/.sparkle/tradewind.ed25519.priv}" ]; then
      assets="$assets Tradewind.app.zip appcast.xml"
    else
      assets="$assets (Sparkle 资产链跳过：SPARKLE_BIN/EdDSA 密钥缺失)"
    fi
    echo "DRY_RUN: plan for $VERSION"
    echo "  prerelease: $([ "$PRE" -eq 1 ] && echo yes || echo no)（默认正式版，--pre 可选重带）"
    echo "  upload: $([ "$UPLOAD" -eq 1 ] && echo yes || echo no)（须显式 --upload）"
    echo "  deploy: $([ "$NO_DEPLOY" -eq 1 ] && echo no || echo yes)"
    echo "  assets: $assets"
    exit 0
  fi

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
  # 自检不经 shasum -c（其按 CWD 解析内容里的裸文件名，dist/ 以外调用必败——
  # code-reviewer P1-2）：直接复算比对十六进制值
  ACTUAL_HEX="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
  if [ "$ACTUAL_HEX" != "$HEX" ]; then
    echo "error: sidecar checksum mismatch (expected $HEX, got $ACTUAL_HEX)" >&2
    exit 1
  fi
  echo "    sidecar verified ($HEX)"

  # --- 3.5 Sparkle 资产链（batch5 §3.3）：zip + EdDSA 签名 + appcast 单条目快照 ----
  # SPARKLE_BIN / 私钥任一缺失 → 警告跳过整段链（tarball/sidecar 照常发）；
  # 签名输出解析失败 = 工具在位却产出不可解析 → fail fast（发布半残比失败更糟）。
  # 密钥双闸自检（batch6b plan §4.7 验签自检）：签名前推导公钥=壳内嵌公钥、签名后 --verify 复验。
  ZIP="$REPO_ROOT/dist/Tradewind.app.zip"
  APPCAST="$REPO_ROOT/dist/appcast.xml"
  SPARKLE_ASSETS=()
  SPARKLE_BIN="${SPARKLE_BIN:-}"
  # 覆盖点（测试注 stub 用，默认=既有约定）；EDPUBKEY_CMD 以私钥路径收尾调用，
  # 取值不得含空格与通配符（下方不加引号展开，按词切分是其工作方式；不合规的值
  # 以 command not found 失败，方向安全）
  EDDSA_PRIV="${EDDSA_PRIV:-$HOME/.sparkle/tradewind.ed25519.priv}"
  EDDSA_PUB_FILE="${EDDSA_PUB_FILE:-$REPO_ROOT/apps/menubar/sparkle-eddsa-pub.txt}"
  EDPUBKEY_CMD="${EDPUBKEY_CMD:-go run ./tools/edpubkey --key-file}"
  if [ -z "$SPARKLE_BIN" ]; then
    echo "warning: SPARKLE_BIN unset; skipping Sparkle asset chain (zip/appcast) — tarball/sidecar 照常" >&2
  elif [ ! -x "$SPARKLE_BIN/sign_update" ]; then
    echo "warning: sign_update not found under SPARKLE_BIN=$SPARKLE_BIN; skipping Sparkle asset chain" >&2
  elif [ ! -f "$EDDSA_PRIV" ]; then
    echo "warning: EdDSA private key $EDDSA_PRIV missing; skipping Sparkle asset chain（前置动作 batch5 §3.4）" >&2
  else
    # 覆盖点被改动时在发布记录里留一行：闸 1 的可信度取决于"推导公钥的命令"没被
    # 调包——EDPUBKEY_CMD 是三者里唯一能把"校验的键"与"签名的键"解耦的，环境里
    # 残留的调试值不该悄悄让闸 1 变成恒真。只提示不拒绝：能往这个 shell 注入环境
    # 的人本来就能经 PATH/SPARKLE_BIN 调包 sign_update。
    [ "$EDDSA_PRIV" = "$HOME/.sparkle/tradewind.ed25519.priv" ] || echo "==> note: EDDSA_PRIV overridden: $EDDSA_PRIV"
    [ "$EDDSA_PUB_FILE" = "$REPO_ROOT/apps/menubar/sparkle-eddsa-pub.txt" ] || echo "==> note: EDDSA_PUB_FILE overridden: $EDDSA_PUB_FILE"
    [ "$EDPUBKEY_CMD" = "go run ./tools/edpubkey --key-file" ] || echo "==> note: EDPUBKEY_CMD overridden: $EDPUBKEY_CMD"
    # 双闸自检·闸 1（签名前；batch6b plan §4.7）：私钥推导公钥 ≠ 壳内嵌公钥 → 拦截。
    # WHY：错私钥签出的发布装得上、appcast 照发，但每个客户端都在 Sparkle 验签处拒绝
    # （半残发布比失败更糟）；apps/menubar/sparkle-eddsa-pub.txt 就是壳实际信任的键。
    # 推导经 edpubkey（唯一允许读私钥文件的东西，只吐公钥；--ed-key-file 红线不变）。
    # 威胁模型：两道闸防的是"拿错钥匙"这类诚实的失误（陈旧私钥、检出错的公钥文件），
    # 不是签名机上的攻击者——私钥文件在闸 1、签名、复验三处各读一次，能在其间换掉
    # 它的人本来就能调包 sign_update 或改 app 包，且仍改不了仓库里的内嵌公钥。
    DERIVED_PUB="$(cd "$REPO_ROOT" && $EDPUBKEY_CMD "$EDDSA_PRIV")" || {
      echo "error: cannot derive public key from $EDDSA_PRIV (edpubkey failed); app trusts $EDDSA_PUB_FILE" >&2
      exit 1
    }
    DERIVED_PUB="$(printf '%s' "$DERIVED_PUB" | tr -d '[:space:]')"
    if [ -z "$DERIVED_PUB" ]; then
      echo "error: empty public key derived from $EDDSA_PRIV; app trusts $EDDSA_PUB_FILE" >&2
      exit 1
    fi
    if [ ! -f "$EDDSA_PUB_FILE" ]; then
      echo "error: embedded pub key file $EDDSA_PUB_FILE missing (signing key $EDDSA_PRIV)" >&2
      exit 1
    fi
    EMBEDDED_PUB="$(tr -d '[:space:]' < "$EDDSA_PUB_FILE")" || {
      echo "error: cannot read embedded pub key file $EDDSA_PUB_FILE (signing key $EDDSA_PRIV)" >&2
      exit 1
    }
    if [ "$DERIVED_PUB" != "$EMBEDDED_PUB" ]; then
      echo "error: $EDDSA_PRIV does not match app-embedded pub $EDDSA_PUB_FILE — abort before signing" >&2
      exit 1
    fi
    echo "==> zip Tradewind.app.zip"
    ditto -c -k --keepParent "$REPO_ROOT/apps/menubar/dist/Tradewind.app" "$ZIP"
    echo "==> sign_update (EdDSA；私钥经 --ed-key-file，不经 argv)"
    SIGN_OUT="$("$SPARKLE_BIN/sign_update" --ed-key-file "$EDDSA_PRIV" "$ZIP")"
    ED_SIG="$(printf '%s\n' "$SIGN_OUT" | sed -n 's/.*sparkle:edSignature="\([^"]*\)".*/\1/p')"
    ZIP_LEN="$(printf '%s\n' "$SIGN_OUT" | sed -n 's/.*[^a-zA-Z]length="\([0-9]*\)".*/\1/p')"
    if [ -z "$ED_SIG" ] || [ -z "$ZIP_LEN" ]; then
      # 不原样回显：sign_update 解码 key 失败时会把整个 key 字符串打进错误信息
      # （sign_update/main.swift）——≥16 个 base64 字符的连续串一律打码后再回显
      echo "error: cannot parse sign_update output: $(printf '%s\n' "$SIGN_OUT" | sed -E 's#[A-Za-z0-9+/=]{16,}#<redacted>#g' | cut -c1-400)" >&2
      exit 1
    fi
    # 双闸自检·闸 2（签名后；batch6b plan §4.7）：--verify 复验不过 → 不写 appcast、不入资产
    "$SPARKLE_BIN/sign_update" --verify --ed-key-file "$EDDSA_PRIV" "$ZIP" "$ED_SIG" || {
      echo "error: signature verification failed for $ZIP — abort before writing appcast" >&2
      exit 1
    }
    # sparkle:version = build 号（与 build.sh CFBundleVersion 同源同式：同一 commit
    # 的提交计数一致；单调递增保证 Sparkle 版本比较方向正确）
    BUILD_NUMBER="$(git -C "$REPO_ROOT" rev-list --count HEAD)"
    echo "==> appcast.xml (sparkle:version=$BUILD_NUMBER)"
    # pubDate RFC 2822（BSD date 无 -R，手动格式 + LC_ALL 锁定星期/月份拼写）
    generate_appcast "$VERSION" "$BUILD_NUMBER" "$ED_SIG" "$ZIP_LEN" \
      "$(LC_ALL=en_US.UTF-8 date -u '+%a, %d %b %Y %H:%M:%S +0000')" "$APPCAST"
    SPARKLE_ASSETS=("$ZIP" "$APPCAST")
  fi

  # --- 3.6 DMG 一体化安装包（batch7-plan §2.1/§4.1）：tarball staging 同源四件 + NOTICE +
  # /Applications 符号链接 → UDZO 只读卷（壳端安装器模式判据前提：bundlePath 恒 /Volumes/）。
  # DMG 不依赖 SPARKLE_BIN——无 Sparkle 工具也照产；上传走既有 gh release upload 路径。
  DMG="$REPO_ROOT/dist/tradewind-$VERSION-darwin-arm64.dmg"
  DMG_NAME="$(basename "$DMG")"
  DMG_STAGE="$REPO_ROOT/dist/dmg-stage-$VERSION"
  echo "==> dmg stage $DMG_STAGE"
  rm -rf "$DMG_STAGE"
  mkdir -p "$DMG_STAGE"
  cp -R "$STAGE/Tradewind.app" "$DMG_STAGE/Tradewind.app"
  cp "$STAGE/tradewind" "$STAGE/NOTICE.txt" "$DMG_STAGE/"
  cp -R "$STAGE/bin" "$STAGE/ui" "$DMG_STAGE/"
  ln -s /Applications "$DMG_STAGE/Applications"

  # DMG 签名身份探测（batch7-plan §4.1）：语义复刻 apps/menubar/build.sh 的签名探测段
  #（同名变量同默认值 + 钥匙串解锁/find-identity 参数拼装，release.sh 原无此机制）。
  # 身份在 → codesign 签 DMG；缺 → 警告跳过签名，DMG 照常产出（build.sh"构建不失败"
  # 降级哲学；ad-hoc 对 DMG 无验证意义，直接跳过而非降级"-"）。
  SIGN_IDENTITY="${SIGN_IDENTITY:-Tradewind Developer}"
  SIGN_KEYCHAIN="${SIGN_KEYCHAIN:-$HOME/.sparkle/tradewind-signing.keychain-db}"
  SIGN_KEYCHAIN_PASS_FILE="${SIGN_KEYCHAIN_PASS_FILE:-$HOME/.sparkle/tradewind-signing.pass}"
  SIGN_KC_SIGN=() # codesign 的 --keychain 参数（空 = 不指定钥匙串）
  IDENTITY_FIND=(-v -p codesigning)
  IDENTITY_SOURCE="default keychain search list"
  if [ -f "$SIGN_KEYCHAIN" ] && [ -f "$SIGN_KEYCHAIN_PASS_FILE" ]; then
    if security unlock-keychain -p "$(cat "$SIGN_KEYCHAIN_PASS_FILE")" "$SIGN_KEYCHAIN" 2>/dev/null; then
      IDENTITY_FIND=(-p codesigning "$SIGN_KEYCHAIN")
      SIGN_KC_SIGN=(--keychain "$SIGN_KEYCHAIN")
      IDENTITY_SOURCE="keychain $SIGN_KEYCHAIN"
    else
      echo "warning: cannot unlock signing keychain $SIGN_KEYCHAIN with $SIGN_KEYCHAIN_PASS_FILE (mismatched password?) — keychain ignored" >&2
    fi
  fi
  IDENTITY_LISTING="$(security find-identity "${IDENTITY_FIND[@]}" 2>/dev/null || true)"
  DMG_SIGN_IDENTITY=""
  if grep -v EXPIRED <<<"$IDENTITY_LISTING" | grep -qF "\"$SIGN_IDENTITY\""; then
    echo "==> codesign identity: $SIGN_IDENTITY ($IDENTITY_SOURCE)"
    DMG_SIGN_IDENTITY="$SIGN_IDENTITY"
  else
    echo "warning: codesign identity \"$SIGN_IDENTITY\" not found — skipping DMG signing（DMG 照常产出）" >&2
  fi
  echo "==> hdiutil create $DMG"
  hdiutil create -volname "Tradewind" -srcfolder "$DMG_STAGE" -ov -format UDZO -o "$DMG"
  if [ -n "$DMG_SIGN_IDENTITY" ]; then
    echo "==> codesign $DMG_NAME"
    codesign --sign "$DMG_SIGN_IDENTITY" ${SIGN_KC_SIGN[@]+"${SIGN_KC_SIGN[@]}"} "$DMG"
  fi

  # sha256 sidecar（与 tarball sidecar 同格式：<hex>  <文件名>，sha256sum 两空格）
  DMG_SIDECAR="$DMG.sha256"
  DMG_HEX="$(shasum -a 256 "$DMG" | awk '{print $1}')"
  printf '%s  %s\n' "$DMG_HEX" "$DMG_NAME" > "$DMG_SIDECAR"
  echo "==> dmg sidecar $DMG_SIDECAR"
  DMG_ASSETS=("$DMG" "$DMG_SIDECAR")

  # --- 4. --upload：查重（已存在只补资产 --clobber）→ create（默认正式版） ----------
  if [ "$UPLOAD" -eq 1 ]; then
    NOTES="${VERSION}。

资产：
  tradewind-${VERSION}-darwin-arm64.tar.gz（整包：CLI + Tradewind.app + bin/mihomo + ui/ + NOTICE）
  ${TARBALL_NAME}.sha256（sha256 sidecar；self-update 消费）
  ${DMG_NAME}（一体化安装包：挂载后打开 DMG 里的 Tradewind 点安装）
  ${DMG_NAME}.sha256（sha256 sidecar）"
    if [ "${#SPARKLE_ASSETS[@]}" -gt 0 ]; then
      NOTES="$NOTES
  Tradewind.app.zip + appcast.xml（app 自动更新；EdDSA 签名单条目快照）"
    fi
    NOTES="$NOTES

升级：\`tradewind version --check\` → \`tradewind self-update\`（CLI）；\`self-update --core\` 升级内核（失败自动回滚）。"
    CREATE_FLAGS=(--repo chaojimaimi/tradewind --title "Tradewind $VERSION")
    if [ "$PRE" -eq 1 ]; then
      CREATE_FLAGS+=(--prerelease) # 观察期小版本；Go 两级过滤（F12）保证 --pre 不推普通 CLI 用户
    fi
    if gh release view "$VERSION" --repo chaojimaimi/tradewind > /dev/null 2>&1; then
      echo "==> release $VERSION exists; uploading assets (--clobber)"
      gh release upload "$VERSION" "$TARBALL" "$SIDECAR" \
        ${SPARKLE_ASSETS[@]+"${SPARKLE_ASSETS[@]}"} \
        ${DMG_ASSETS[@]+"${DMG_ASSETS[@]}"} \
        --repo chaojimaimi/tradewind --clobber
    else
      echo "==> gh release create $VERSION ($([ "$PRE" -eq 1 ] && echo 'prerelease（--pre）' || echo '正式版，无 Pre-release 标签'))"
      gh release create "$VERSION" "$TARBALL" "$SIDECAR" \
        ${SPARKLE_ASSETS[@]+"${SPARKLE_ASSETS[@]}"} \
        ${DMG_ASSETS[@]+"${DMG_ASSETS[@]}"} \
        "${CREATE_FLAGS[@]}" \
        --notes "$NOTES"
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
  # app 重启（build.sh 已 pkill 旧实例；bundle 已重建并签名）
  open "$REPO_ROOT/apps/menubar/dist/Tradewind.app"
  echo "==> release $VERSION done (tarball+sidecar+dmg+dmg.sha256 in dist/)"
}

# 仅 $0 直跑时执行 main（source 供 golden 测试直接调用 generate_appcast，
# batch5 §3.3"XML 组装抽为可独立调用的函数"）
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
