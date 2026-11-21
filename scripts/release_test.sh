#!/bin/bash
# release.sh 测试（batch5 §5）：bash -n 语法门禁 + generate_appcast golden（占位签名）
# + DRY_RUN 干跑断言（无 --prerelease 默认 / 资产清单含 zip+appcast / --upload 显式
# 语义保持）。全绿退出 0；真私钥/真网络零接触（占位签名注入，评审 P3）。
# 用法：bash scripts/release_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/release.sh"
GOLDEN="$HERE/testdata/appcast-golden.xml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }
pass() { echo "PASS: $1"; }

# 1. 语法门禁
bash -n "$SCRIPT" || fail "bash -n release.sh"
pass "bash -n release.sh"

# 2. generate_appcast golden（source 直调函数；签名占位注入——真私钥不进测试）
# shellcheck source=/dev/null
source "$SCRIPT"
generate_appcast "v9.9.9-test" 42 "PLACEHOLDER_EDDSA_SIG" 12345 \
  "Fri, 20 Nov 2026 12:00:00 +0000" "$TMP/appcast.xml"
diff "$GOLDEN" "$TMP/appcast.xml" > /dev/null || fail "appcast golden mismatch"
# 锁点复核：enclosure 钉 tag URL（评审 P2-2）/ build 号 / 占位签名 / minimumSystemVersion
grep -q 'releases/download/v9.9.9-test/Tradewind.app.zip' "$TMP/appcast.xml" || fail "enclosure not pinned to tag URL"
grep -q '<sparkle:version>42</sparkle:version>' "$TMP/appcast.xml" || fail "sparkle:version != build number"
grep -q 'sparkle:edSignature="PLACEHOLDER_EDDSA_SIG"' "$TMP/appcast.xml" || fail "placeholder signature missing"
grep -q '<minimumSystemVersion>13.0</minimumSystemVersion>' "$TMP/appcast.xml" || fail "minimumSystemVersion != 13.0"
pass "generate_appcast golden + lockpoints"

# 3. DRY_RUN 默认（无 Sparkle 工具）：正式版默认（无 --prerelease）、不 upload、链跳过
OUT="$(HOME="$TMP/fakehome" DRY_RUN=1 bash "$SCRIPT" v9.9.9-dry)"
echo "$OUT" | grep -q "prerelease: no" || fail "default must not be prerelease (m3e1 正式版)"
echo "$OUT" | grep -q "upload: no" || fail "default must not upload (--upload 显式语义保持)"
echo "$OUT" | grep -q "Sparkle 资产链跳过" || fail "sparkle chain must be skipped without tools"
if echo "$OUT" | grep -q "Tradewind.app.zip appcast.xml$"; then
  fail "asset list must not contain zip+appcast when tools missing"
fi
pass "DRY_RUN default: 正式版 / no upload / sparkle skipped"

# 4. DRY_RUN --upload + --pre + 伪 Sparkle 工具：资产清单含 zip+appcast
mkdir -p "$TMP/fakehome/.sparkle" "$TMP/fakebin"
touch "$TMP/fakehome/.sparkle/tradewind.ed25519.priv" # 占位空文件：干跑不读取内容
printf '#!/bin/sh\necho "sparkle:edSignature=\\"DRYRUN_PLACEHOLDER_SIG\\" length=\\"42\\""\n' > "$TMP/fakebin/sign_update"
chmod +x "$TMP/fakebin/sign_update"
OUT="$(HOME="$TMP/fakehome" SPARKLE_BIN="$TMP/fakebin" DRY_RUN=1 bash "$SCRIPT" v9.9.9-dry --upload --pre)"
echo "$OUT" | grep -q "prerelease: yes" || fail "--pre must request prerelease"
echo "$OUT" | grep -q "upload: yes" || fail "--upload flag must show upload"
echo "$OUT" | grep -q "Tradewind.app.zip appcast.xml" || fail "asset list must contain zip+appcast"
pass "DRY_RUN --upload --pre: prerelease / opt-in upload / zip+appcast assets"

echo "ALL PASS (release.sh tests)"
