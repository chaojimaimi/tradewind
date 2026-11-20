#!/bin/bash
# Tradewind 壳构建（M2-plan §3）：swift build → 最小 .app bundle（LSUIElement）。
# 自用 ad-hoc 签名（DESIGN §7.7：右键打开绕过 Gatekeeper；分发再买 Developer ID）。
# 用法：
#   apps/menubar/build.sh                      # release 构建 → apps/menubar/dist/Tradewind.app
#   CONFIG=debug apps/menubar/build.sh         # 调试构建
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$DIR/../.." && pwd)"
VERSION="${VERSION:-$(git -C "$REPO_ROOT" describe --tags --always)}"
CONFIG="${CONFIG:-release}"
APP="$DIR/dist/Tradewind.app"

cd "$DIR"
swift build -c "$CONFIG"
BINDIR="$(swift build -c "$CONFIG" --show-bin-path)"
BIN="$BINDIR/TradewindShell"

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
	<string>$VERSION</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>NSHumanReadableCopyright</key>
	<string>MIT License</string>
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
codesign --force --sign - "$APP"
echo "built $APP (version $VERSION)"
