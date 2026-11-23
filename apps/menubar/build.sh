#!/bin/bash
# Tradewind 壳构建（M2-plan §3）：swift build → 最小 .app bundle（LSUIElement）。
# 签名身份（batch5 §3.1 + 批⑥ signing identity）：优先用 scripts/make-signing-identity.sh
# 产出的独立签名钥匙串（SIGN_KEYCHAIN + SIGN_KEYCHAIN_PASS_FILE 都在 → 解锁后在其中
# 探测，未受信任的自签证书也接受）；二者不在 → 维持 batch5 路径（默认钥匙串搜索列表里
# 经 Keychain Access 手工创建的证书）。都没有 → 降级 ad-hoc + stderr 警告（构建不
# 失败——ad-hoc 签名下 Sparkle 更新链路的安装期签名校验未生效，m3e2 实测 gate）。
# Sparkle 嵌入（batch5 §3.1 终审 P2-2 fail fast）：Package.swift 声明 Sparkle 依赖即
# dyld 加载期硬依赖——framework 探测失败 → 构建失败 exit 1（警告跳过=产出打不开的
# app）；Package.swift 尚无 Sparkle 依赖时静默跳过。"构建不失败"原则仅适用于签名
# 身份降级与 SUPublicEDKey 缺失两处真降级。
# 用法：
#   apps/menubar/build.sh                      # release 构建 → apps/menubar/dist/Tradewind.app
#   CONFIG=debug apps/menubar/build.sh         # 调试构建
#   SIGN_IDENTITY="自定义名" apps/menubar/build.sh   # 显式指定签名身份
#   SIGN_KEYCHAIN=... SIGN_KEYCHAIN_PASS_FILE=... apps/menubar/build.sh   # 覆盖签名钥匙串落点
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$DIR/../.." && pwd)"
VERSION="${VERSION:-$(git -C "$REPO_ROOT" describe --tags --always)}"
CONFIG="${CONFIG:-release}"
APP="$DIR/dist/Tradewind.app"
# CFBundleVersion=build 号（batch5 §3.1：git 提交计数，单调递增；ShortVersionString
# 保持 $VERSION。F2 已证 CFBundleVersion 全仓无消费点——壳只读 ShortVersionString、
# Go 比对用 ldflags version，改 build 号零回归。appcast sparkle:version 同源同值，
# release.sh 以同式计算）
BUILD_NUMBER="$(git -C "$REPO_ROOT" rev-list --count HEAD)"
SIGN_IDENTITY="${SIGN_IDENTITY:-Tradewind Developer}"
# 签名钥匙串（批⑥ signing identity）：scripts/make-signing-identity.sh 的产物（默认落点
# 与该脚本一致；EdDSA 私钥同在 ~/.sparkle 的既有约定）。
SIGN_KEYCHAIN="${SIGN_KEYCHAIN:-$HOME/.sparkle/tradewind-signing.keychain-db}"
SIGN_KEYCHAIN_PASS_FILE="${SIGN_KEYCHAIN_PASS_FILE:-$HOME/.sparkle/tradewind-signing.pass}"

cd "$DIR"
swift build -c "$CONFIG"
BINDIR="$(swift build -c "$CONFIG" --show-bin-path)"
BIN="$BINDIR/TradewindShell"

# 签名身份探测（batch5 §3.1；批⑥ signing identity）：独立签名钥匙串可用 → 只在其中
# 探测；否则查默认钥匙串搜索列表（batch5 手工证书路径）；均缺失 → 降级 ad-hoc "-"
# + 显著警告（构建不失败）。
# WHY 不带 -v 查独立钥匙串：-v 按信任状态过滤，而自签证书有意不加信任设置（加信任
# 需 GUI 授权）——会被整段滤掉；改以排除 EXPIRED 行表达"可用"。
# WHY 先把清单读进变量再 grep：pipefail 下 `security | grep -q` 的 grep 提前退出会让
# security 吃 SIGPIPE，命中也可能被判失败。
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
if grep -v EXPIRED <<<"$IDENTITY_LISTING" | grep -qF "\"$SIGN_IDENTITY\""; then
  echo "==> codesign identity: $SIGN_IDENTITY ($IDENTITY_SOURCE)"
else
  echo "warning: codesign identity \"$SIGN_IDENTITY\" not found (生成：scripts/make-signing-identity.sh); falling back to ad-hoc — Sparkle 更新链路未生效" >&2
  SIGN_IDENTITY="-"
  SIGN_KC_SIGN=()
fi

# 先停运行中的旧实例（code-reviewer P3）：删除运行中 bundle 不终止进程（inode 存续），
# 重建后 LaunchServices 按 bundle id 激活旧实例 → 热修验证被误导为"修复无效"或双图标
pkill -x Tradewind 2>/dev/null || true
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/Tradewind"
# 品牌图标（v0.4.10 logo 资产接入，2026-11-19 拍板）：仓内 assets/ 拷入 Resources
cp "$DIR/assets/Tradewind.icns" "$APP/Contents/Resources/Tradewind.icns"
# SPM 资源包（v0.4.11 热修）：TradewindKit 有 .copy 资源后 SPM 生成 TradewindShell_TradewindKit.bundle，
# Bundle.module 运行时按「可执行文件同目录 Resources」约定查找；缺失即启动 fatal error 闪退
# （v0.4.10 真机 open 静默失败的根因）。守卫存在性：资源清空回退时不阻塞构建。
if [ -d "$BINDIR/TradewindShell_TradewindKit.bundle" ]; then
  cp -R "$BINDIR/TradewindShell_TradewindKit.bundle" "$APP/Contents/Resources/"
else
  echo "warning: SPM resource bundle not found; app would crash at startup if TradewindKit uses Bundle.module" >&2
fi

# Sparkle.framework 嵌入（batch5 §3.1 终审 P2-2 fail fast）：SPM binaryTarget 的
# framework 不进手工 .app bundle——壳引用 SPU 符号即 dyld 加载期硬依赖，缺失任何
# 运行时防御都执行不到（加载期闪退）。探测 bin-path 邻近路径 + XCFramework 解包
# 兜底（.build/artifacts）；Package.swift 已声明 Sparkle 而探测失败 → exit 1；
# Package.swift 尚无 Sparkle 依赖（batch5 c3 落地前现状）→ 静默跳过。
SPARKLE_FW="$BINDIR/Sparkle.framework"
if [ ! -d "$SPARKLE_FW" ]; then
  SPARKLE_FW="$(find "$DIR/.build/artifacts" -maxdepth 5 -type d -name Sparkle.framework -path "*macos*" 2>/dev/null | head -1 || true)"
fi
if [ -d "$SPARKLE_FW" ]; then
  echo "==> embed Sparkle.framework ($SPARKLE_FW)"
  mkdir -p "$APP/Contents/Frameworks"
  cp -R "$SPARKLE_FW" "$APP/Contents/Frameworks/"
  # bundle 内 framework 的 dyld 定位 rpath（SPM 产物按 build 目录路径链接，
  # bundle 化后必须自带 @executable_path/../Frameworks）
  if ! otool -l "$APP/Contents/MacOS/Tradewind" | grep -q "@executable_path/../Frameworks"; then
    install_name_tool -add_rpath "@executable_path/../Frameworks" "$APP/Contents/MacOS/Tradewind"
  fi
elif grep -qi sparkle "$DIR/Package.swift"; then
  echo "error: Package.swift declares Sparkle but Sparkle.framework not found in SPM output (probed $BINDIR and .build/artifacts) — dyld 加载期硬依赖，缺 framework 的 app 打不开（终审 P2-2 fail fast）" >&2
  exit 1
fi

# SUFeedURL 恒注入（F7 稳定 feed URL：公开仓 latest/download 匿名可达、自动指向
# Latest 资产；public 前私有仓 404 → Sparkle 标准错误窗不静默，README/报告注明）。
# SUPublicEDKey 从 sparkle-eddsa-pub.txt 注入（batch5 §3.4：首次 generate_keys 的
# stdout 捕获公钥后写入入仓）；缺失/空 → stderr 警告 + 跳过注入（构建不失败，评审 P3）。
SU_FEED_URL="https://github.com/chaojimaimi/tradewind/releases/latest/download/appcast.xml"
PUB_KEY_LINE=""
if [ -f "$DIR/sparkle-eddsa-pub.txt" ]; then
  PUB_KEY="$(tr -d '[:space:]' < "$DIR/sparkle-eddsa-pub.txt")"
  if [ -n "$PUB_KEY" ]; then
    PUB_KEY_LINE="	<key>SUPublicEDKey</key>
	<string>$PUB_KEY</string>"
  else
    echo "warning: sparkle-eddsa-pub.txt is empty; SUPublicEDKey not injected — Sparkle EdDSA 验签未生效" >&2
  fi
else
  echo "warning: sparkle-eddsa-pub.txt not found; SUPublicEDKey not injected — Sparkle EdDSA 验签未生效" >&2
fi
cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>Tradewind</string>
	<key>CFBundleDisplayName</key>
	<string>Tradewind</string>
	<key>CFBundleIdentifier</key>
	<string>io.github.chaojimaimi.tradewind</string>
	<key>CFBundleExecutable</key>
	<string>Tradewind</string>
	<key>CFBundleIconFile</key>
	<string>Tradewind</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>$VERSION</string>
	<key>CFBundleVersion</key>
	<string>$BUILD_NUMBER</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>NSHumanReadableCopyright</key>
	<string>MIT License</string>
	<key>SUFeedURL</key>
	<string>$SU_FEED_URL</string>
$PUB_KEY_LINE
	<key>CFBundleURLTypes</key>
	<array>
		<dict>
			<key>CFBundleURLName</key>
			<string>tradewind</string>
			<key>CFBundleURLSchemes</key>
			<array>
				<string>tradewind</string>
			</array>
		</dict>
	</array>
</dict>
</plist>
EOF
# 嵌套签名（batch5 §3.1）：framework 先签 → app 后签（--deep 仅作降级路径，登记不用）
# --keychain 仅在独立签名钥匙串可用时附带；ad-hoc 降级不带。bash 3.2 + set -u 下空
# 数组不可直接展开，用 ${arr[@]+"${arr[@]}"} 惯用法。
if [ -d "$APP/Contents/Frameworks/Sparkle.framework" ]; then
  codesign --force --sign "$SIGN_IDENTITY" ${SIGN_KC_SIGN[@]+"${SIGN_KC_SIGN[@]}"} "$APP/Contents/Frameworks/Sparkle.framework"
fi
codesign --force --sign "$SIGN_IDENTITY" ${SIGN_KC_SIGN[@]+"${SIGN_KC_SIGN[@]}"} "$APP"
echo "built $APP (version $VERSION, build $BUILD_NUMBER)"
