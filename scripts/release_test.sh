#!/bin/bash
# release.sh 测试（batch5 §5；batch6b plan §4.7 验签自检扩展）：bash -n 语法门禁 +
# generate_appcast golden（占位签名）+ DRY_RUN 干跑断言（无 --prerelease 默认 /
# 资产清单含 zip+appcast / --upload 显式语义保持）+ Sparkle 链沙盒真跑（stub build/
# mihomo/git + 伪 sign_update/edpubkey 注入）：happy path 恰好"签名一次 + --verify
# 一次"且顺序正确；推导公钥≠内嵌公钥 / edpubkey 失败 → 签名前拦截（sign_update
# 零调用）；--verify 失败 → appcast 前拦截（无 appcast、资产清单不出现 zip+appcast）。
# 真私钥/真网络/真 deploy 零接触：密钥全用临时占位（内容不被读取），HOME 指向
# mktemp 临时目录，--no-deploy 收尾。
# 用法：bash scripts/release_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/release.sh"
GOLDEN="$HERE/testdata/appcast-golden.xml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }
pass() { echo "PASS: $1"; }

# —— Sparkle 链沙盒（4b/5/6/7 共用）：以 stub 复刻 repo 形态，令 release.sh 真跑
# Sparkle 链段本身（zip→推导比对→签名→--verify→appcast）。swift build/deploy/gh
# 绝不进测试：build 产物与数据目录全用 stub；PATH 前置的 fakebin 只含 git stub
#（BUILD_NUMBER=42，与断言无关）。沙盒都在 $TMP 下，由单一 trap 清理。
make_sandbox() {
  local s="$1"
  mkdir -p "$s/scripts" "$s/apps/menubar" "$s/fakebin" \
    "$s/home/.sparkle" "$s/data/bin" "$s/data/ui" "$s/log"
  cp "$SCRIPT" "$s/scripts/release.sh"
  cat > "$s/scripts/build.sh" <<EOF
#!/bin/sh
set -eu
mkdir -p "$s/dist"
: > "$s/dist/tradewind"
chmod +x "$s/dist/tradewind"
EOF
  cat > "$s/apps/menubar/build.sh" <<EOF
#!/bin/sh
set -eu
mkdir -p "$s/apps/menubar/dist/Tradewind.app/Contents/MacOS"
EOF
  cat > "$s/data/bin/mihomo" <<'EOF'
#!/bin/sh
echo "stub mihomo v1.19.34"
EOF
  : > "$s/data/bin/mihomo.sha256"
  : > "$s/data/ui/index.html"
  printf '#!/bin/sh\necho 42\n' > "$s/fakebin/git"
  cp "$TMP/fakebin/sign_update" "$s/fakebin/sign_update"
  printf '#!/bin/sh\necho TESTEDPUBKEY\n' > "$s/fakebin/edpubkey" # EDPUBKEY_CMD stub
  printf '#!/bin/sh\necho "stub edpubkey failure" >&2\nexit 1\n' > "$s/fakebin/edpubkey-fail"
  chmod +x "$s/scripts/build.sh" "$s/apps/menubar/build.sh" "$s/data/bin/mihomo" \
    "$s/fakebin/git" "$s/fakebin/sign_update" "$s/fakebin/edpubkey" "$s/fakebin/edpubkey-fail"
  : > "$s/home/.sparkle/tradewind.ed25519.priv" # 占位空文件：内容经 stub 间接化，绝不被读取
}

# 沙盒真跑 release.sh（--no-deploy 收尾零副作用）；$2 可覆盖 EDPUBKEY_CMD
run_chain() {
  local s="$1" edpubkey="${2:-$1/fakebin/edpubkey}"
  HOME="$s/home" \
    TRADEDWIND_HOME="$s/data" \
    SPARKLE_BIN="$s/fakebin" \
    EDDSA_PRIV="$s/home/.sparkle/tradewind.ed25519.priv" \
    EDDSA_PUB_FILE="$s/pub.txt" \
    EDPUBKEY_CMD="$edpubkey" \
    PATH="$s/fakebin:$PATH" \
    SIGN_UPDATE_LOG="$s/log/sign_update.log" \
    bash "$s/scripts/release.sh" v9.9.9-test --no-deploy
}

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
grep -q '<sparkle:minimumSystemVersion>13.0</sparkle:minimumSystemVersion>' "$TMP/appcast.xml" || fail "minimumSystemVersion != 13.0"
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

# 4. DRY_RUN --upload + --pre + 伪 Sparkle 工具：资产清单含 zip+appcast；
#    伪 sign_update 每次调用把首个参数追加进日志（调用次数/顺序断言用），--verify 恒过
mkdir -p "$TMP/fakehome/.sparkle" "$TMP/fakebin"
touch "$TMP/fakehome/.sparkle/tradewind.ed25519.priv" # 占位空文件：干跑不读取内容
cat > "$TMP/fakebin/sign_update" <<'EOF'
#!/bin/sh
# POSIX sh 伪 sign_update：日志追加首个参数；--verify 恒过；签名调用答占位行（真私钥绝不进测试）
printf '%s\n' "$1" >> "${SIGN_UPDATE_LOG:-/dev/null}"
if [ "$1" = "--verify" ]; then
  exit 0
fi
echo 'sparkle:edSignature="DRYRUN_PLACEHOLDER_SIG" length="42"'
EOF
chmod +x "$TMP/fakebin/sign_update"
OUT="$(HOME="$TMP/fakehome" SPARKLE_BIN="$TMP/fakebin" DRY_RUN=1 bash "$SCRIPT" v9.9.9-dry --upload --pre)"
echo "$OUT" | grep -q "prerelease: yes" || fail "--pre must request prerelease"
echo "$OUT" | grep -q "upload: yes" || fail "--upload flag must show upload"
echo "$OUT" | grep -q "Tradewind.app.zip appcast.xml" || fail "asset list must contain zip+appcast"
pass "DRY_RUN --upload --pre: prerelease / opt-in upload / zip+appcast assets"

# 4b. Sparkle 链沙盒真跑（happy path，batch6b plan §4.7）：EDPUBKEY_CMD stub 输出与
#     内嵌 pub 文件同串 → 链走通；sign_update 恰好两次：先签名（--ed-key-file）后 --verify
S4="$(mktemp -d "$TMP/sandbox.XXXXXX")"
make_sandbox "$S4"
printf 'TESTEDPUBKEY\n' > "$S4/pub.txt"
OUT="$(run_chain "$S4")" || fail "happy path: full chain must succeed"
LOG="$S4/log/sign_update.log"
[ "$(wc -l < "$LOG" | tr -d '[:space:]')" -eq 2 ] || fail "sign_update must be called exactly twice"
[ "$(sed -n '1p' "$LOG")" = "--ed-key-file" ] || fail "first sign_update call must be the signing call (--ed-key-file)"
[ "$(sed -n '2p' "$LOG")" = "--verify" ] || fail "second sign_update call must be --verify"
[ -f "$S4/dist/Tradewind.app.zip" ] || fail "zip must be produced on happy path"
[ -f "$S4/dist/appcast.xml" ] || fail "appcast.xml must be written on happy path"
# 覆盖点被改动 → 发布记录里必须看得见（沙盒里 EDPUBKEY_CMD 与 EDDSA_PUB_FILE 真被
# 改了；EDDSA_PRIV 等于沙盒 HOME 下的默认路径，不算覆盖，不得出现提示）
echo "$OUT" | grep -q "^==> note: EDPUBKEY_CMD overridden:" || fail "an overridden EDPUBKEY_CMD must be visible in the release transcript"
echo "$OUT" | grep -q "^==> note: EDDSA_PUB_FILE overridden:" || fail "an overridden EDDSA_PUB_FILE must be visible in the release transcript"
if echo "$OUT" | grep -q "EDDSA_PRIV overridden"; then
  fail "EDDSA_PRIV at its default path must not be reported as overridden"
fi
pass "Sparkle chain happy path: sign once then --verify once, appcast written; overrides visible"

# 5. 双闸·闸 1（签名前）：推导公钥 ≠ 内嵌公钥 → 拦截（sign_update 零调用；stderr 点名两份文件）
S5="$(mktemp -d "$TMP/sandbox.XXXXXX")"
make_sandbox "$S5"
printf 'MISMATCHEDPUB\n' > "$S5/pub.txt" # 与 edpubkey stub 输出（TESTEDPUBKEY）不同
if run_chain "$S5" >/dev/null 2>"$S5/err.txt"; then
  fail "key mismatch must abort the release"
fi
if [ -s "$S5/log/sign_update.log" ]; then
  fail "sign_update must not be called when keys mismatch"
fi
grep -qF "$S5/home/.sparkle/tradewind.ed25519.priv" "$S5/err.txt" || fail "stderr must name the private key path"
grep -qF "$S5/pub.txt" "$S5/err.txt" || fail "stderr must name the embedded pub file path"
pass "key mismatch: abort before signing (sign_update untouched)"

# 6. 双闸·闸 1：edpubkey 推导命令失败 → 拦截（sign_update 零调用）
S6="$(mktemp -d "$TMP/sandbox.XXXXXX")"
make_sandbox "$S6"
if run_chain "$S6" "$S6/fakebin/edpubkey-fail" >/dev/null 2>"$S6/err.txt"; then
  fail "edpubkey failure must abort the release"
fi
if [ -s "$S6/log/sign_update.log" ]; then
  fail "sign_update must not be called when derivation fails"
fi
pass "edpubkey failure: abort before signing"

# 7. 双闸·闸 2（签名后）：sign_update --verify 失败 → 拦截（无 appcast；stdout 不列 zip+appcast 资产）
S7="$(mktemp -d "$TMP/sandbox.XXXXXX")"
make_sandbox "$S7"
cat > "$S7/fakebin/sign_update" <<'EOF'
#!/bin/sh
printf '%s\n' "$1" >> "${SIGN_UPDATE_LOG:-/dev/null}"
if [ "$1" = "--verify" ]; then
  exit 1
fi
echo 'sparkle:edSignature="DRYRUN_PLACEHOLDER_SIG" length="42"'
EOF
chmod +x "$S7/fakebin/sign_update"
printf 'TESTEDPUBKEY\n' > "$S7/pub.txt"
if OUT="$(run_chain "$S7" 2>"$S7/err.txt")"; then
  fail "--verify failure must abort the release"
fi
if [ -f "$S7/dist/appcast.xml" ]; then
  fail "appcast.xml must not be written when --verify fails"
fi
# 签名已经发生（zip 在），appcast 一步不得开始
[ -f "$S7/dist/Tradewind.app.zip" ] || fail "zip must exist by the time --verify runs"
if echo "$OUT" | grep -q "^==> appcast.xml"; then
  fail "the appcast step must not start when --verify fails"
fi
LOG="$S7/log/sign_update.log"
[ "$(sed -n '2p' "$LOG" 2>/dev/null)" = "--verify" ] || fail "abort must happen at the --verify stage"
pass "--verify failure: abort before appcast"

echo "ALL PASS (release.sh tests)"
