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
BIN="$(swift build -c "$CONFIG" --show-bin-path)/TradewindShell"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/Tradewind"
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
</dict>
</plist>
EOF
codesign --force --sign - "$APP"
echo "built $APP (version $VERSION)"
