#!/bin/bash
# tradewind M1 E2E 冒烟（M1-plan §6 三阶段）：gateway / 节点切换 / sysproxy 回归。
#
# 默认路径：隔离 TRADEDWIND_HOME（mktemp 临时目录）+ mixed 17897 + gateway 19097
# （避开默认 39097 与 Verge 9097）+ 不触碰系统代理。
# 阶段 3 预检本机 Clash Verge：在跑则 SKIP 并打印可见警告（整体仍 PASS，M1-plan §7.6）。
# 用法：
#   scripts/smoke.sh                      默认冒烟（fixture，不出网断言）
#   scripts/smoke.sh --url <订阅URL>      追加真实订阅出网 204 断言（凭据不入库）
#
# 断言清单：
#   阶段1 网关：无凭据 404 / 错 token 404 / 入口 Set-Cookie(Path=/,HttpOnly,SameSite=Lax) /
#     cookie jar 下 /version /configs 200 且不含 secret / /traffic 流式（超时断开）/
#     SPA 深链 /ui/proxies 返回 index.html / 伪造 Origin 与 Host 拒绝
#   阶段2 节点：select → /proxies now 变化 → profiles.yaml.selected 回写 →
#     stop → start → now 保持（回归 profile.store-selected 注入）
#   阶段3 sysproxy：networksetup 快照 → start → 应用 → stop → 逐字段还原 diff
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MIXED_PORT=17897
GATEWAY_PORT=19097
SMOKE_VERSION=v0.2.0-smoke
DEFAULT_HOME="$HOME/Library/Application Support/tradewind"
TMP_ROOT="$(mktemp -d /tmp/tradewind-smoke.XXXXXX)"
TW_HOME="$TMP_ROOT/home"
WORK="$TMP_ROOT/work"
mkdir -p "$TW_HOME" "$WORK"
TW="$WORK/tradewind"
export TRADEDWIND_HOME="$TW_HOME"

START_PID=""
cleanup() {
  if [[ -n "$START_PID" ]] && kill -0 "$START_PID" 2>/dev/null; then
    kill -TERM "$START_PID" 2>/dev/null || true
    sleep 1
    kill -KILL "$START_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

log() { printf '[smoke] %s\n' "$*"; }
fail() { printf '[smoke] FAIL: %s\n' "$*" >&2; exit 1; }
port_busy() { nc -z -w 1 127.0.0.1 "$1" 2>/dev/null; }

GW="http://127.0.0.1:$GATEWAY_PORT"
JAR="$WORK/jar.txt"

# --- sysproxy 快照（阶段 3 断言的对照基准）---
snapshot_sysproxy() {
  local svc
  networksetup -listallnetworkservices | tail -n +2 | grep -v '^\*' | while read -r svc; do
    echo "== $svc"
    networksetup -getwebproxy "$svc"
    networksetup -getsecurewebproxy "$svc"
    networksetup -getsocksfirewallproxy "$svc"
    networksetup -getproxybypassdomains "$svc"
  done
}

WITH_URL=""
while (($#)); do
  case "$1" in
    --url) WITH_URL="${2:?--url needs a value}"; shift 2 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

log "building tradewind via scripts/build.sh (VERSION=$SMOKE_VERSION)"
VERSION="$SMOKE_VERSION" OUT="$TW" "$REPO_ROOT/scripts/build.sh" >/dev/null || fail "build.sh failed"

log "asserting version command reports the injected build version"
"$TW" version | grep -q "$SMOKE_VERSION" || fail "version output missing $SMOKE_VERSION"

log "staging kernel, geo and ui assets into isolated home"
[[ -x "$DEFAULT_HOME/bin/mihomo" ]] || fail "kernel not found at $DEFAULT_HOME/bin/mihomo"
mkdir -p "$TW_HOME/bin"
cp "$DEFAULT_HOME/bin/mihomo" "$TW_HOME/bin/mihomo"
if [[ -f "$DEFAULT_HOME/bin/mihomo.sha256" ]]; then
  cp "$DEFAULT_HOME/bin/mihomo.sha256" "$TW_HOME/bin/mihomo.sha256"
else
  shasum -a 256 "$TW_HOME/bin/mihomo" | awk '{print $1}' > "$TW_HOME/bin/mihomo.sha256"
fi
for f in geoip.metadb geosite.dat; do
  if [[ -f "$DEFAULT_HOME/$f" ]] && (("$(stat -f%z "$DEFAULT_HOME/$f")" > 1024)) && [[ ! -f "$TW_HOME/$f" ]]; then
    cp "$DEFAULT_HOME/$f" "$TW_HOME/$f"
  fi
done
[[ -f "$DEFAULT_HOME/ui/index.html" ]] || fail "metacubexd ui not found at $DEFAULT_HOME/ui"
cp -R "$DEFAULT_HOME/ui" "$TW_HOME/ui"

log "writing settings (mixed_port=$MIXED_PORT, gateway_port=$GATEWAY_PORT)"
cat > "$TW_HOME/settings.yaml" <<EOF
mixed_port: $MIXED_PORT
gateway_port: $GATEWAY_PORT
panel_enabled: true
mode: rule
log_level: info
EOF

log "pre-bind probe (mixed=$MIXED_PORT, gateway=$GATEWAY_PORT)"
if port_busy "$MIXED_PORT"; then fail "port $MIXED_PORT already in use"; fi
if port_busy "$GATEWAY_PORT"; then fail "port $GATEWAY_PORT already in use"; fi

log "add --file fixture"
"$TW" add --file "$REPO_ROOT/scripts/fixtures/smoke-subscription.yaml" --name smoke \
  || fail "add fixture failed"

wait_ready() { # $1=log file; fail-fast on early exit, ready 判据含 panel 入口 URL
  local f="$1"
  for _ in $(seq 1 75); do
    if [[ -f "$f" ]] && grep -q '^ready:' "$f"; then return 0; fi
    if ! kill -0 "$START_PID" 2>/dev/null; then
      echo "--- $f ---"; cat "$f"
      fail "start process exited before ready"
    fi
    sleep 0.2
  done
  echo "--- $f ---"; cat "$f"
  fail "handshake not ready within 15s"
}

start_bg() { # $1=log file; 其余为 start 参数
  local f="$1"; shift
  "$TW" start "$@" > "$f" 2>&1 &
  START_PID=$!
  wait_ready "$f"
  cat "$f"
}

log "start #1 (--no-sysproxy)"
START_LOG="$WORK/start.log"
start_bg "$START_LOG" --no-sysproxy
grep -q "panel=http://127.0.0.1:$GATEWAY_PORT/ui/" "$START_LOG" \
  || fail "ready line missing panel entry URL"
"$TW" status || fail "status exited non-zero"

TOKEN="$(cat "$TW_HOME/panel_token")"
SECRET="$(cat "$TW_HOME/secret")"
WRONG_TOKEN="$(printf 'f%.0s' {1..64})"

# ============ 阶段 1：网关（M1-plan §6.1）============
log "== STAGE 1: gateway"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$GW/")
[[ "$CODE" == "404" ]] || fail "no-cred / got $CODE, want 404"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$GW/ui/")
[[ "$CODE" == "404" ]] || fail "no-cred /ui/ got $CODE, want 404"

CODE=$(curl -s -o /dev/null -w '%{http_code}' "$GW/ui/$WRONG_TOKEN/")
[[ "$CODE" == "404" ]] || fail "wrong-token entry got $CODE, want 404"

# ③ 正确入口：cookie jar 承接（后续 ④-⑦ 一律 -b jar，真实回归 Path=/ 语义）
curl -s -D "$WORK/entry-headers.txt" -o /dev/null -c "$JAR" -L "$GW/ui/$TOKEN/" \
  || fail "token entry request failed"
grep -qi "Set-Cookie: .*tradewind_panel=" "$WORK/entry-headers.txt" \
  || fail "entry did not set session cookie"
grep -qi "Set-Cookie: .*Path=/" "$WORK/entry-headers.txt" || fail "cookie Path must be /"
grep -qi "Set-Cookie: .*HttpOnly" "$WORK/entry-headers.txt" || fail "cookie must be HttpOnly"
grep -qi "Set-Cookie: .*SameSite=Lax" "$WORK/entry-headers.txt" || fail "cookie must be SameSite=Lax"
# set -e 下 grep && fail 会在"未找到"时误触发退出，必须用 if 形式
if grep -qi "Set-Cookie: .*Domain=" "$WORK/entry-headers.txt"; then
  fail "cookie must be host-only (no Domain)"
fi

CODE=$(curl -s -b "$JAR" -o "$WORK/version.json" -w '%{http_code}' "$GW/version")
[[ "$CODE" == "200" ]] || fail "cookie /version got $CODE, want 200"
if grep -q "$SECRET" "$WORK/version.json"; then fail "/version response leaked secret"; fi

CODE=$(curl -s -b "$JAR" -o "$WORK/configs.json" -w '%{http_code}' "$GW/configs")
[[ "$CODE" == "200" ]] || fail "cookie /configs got $CODE, want 200"
if grep -q "$SECRET" "$WORK/configs.json"; then fail "/configs response leaked secret"; fi

RC=0
curl -s -b "$JAR" --max-time 2 "$GW/traffic" -o /dev/null || RC=$?
[[ $RC -eq 28 ]] || fail "/traffic should stream until our timeout (rc=$RC), not close early"

BODY_SHA=$(curl -s -b "$JAR" "$GW/ui/proxies" | shasum -a 256 | awk '{print $1}')
IDX_SHA=$(shasum -a 256 "$TW_HOME/ui/index.html" | awk '{print $1}')
[[ "$BODY_SHA" == "$IDX_SHA" ]] || fail "SPA deep link /ui/proxies did not return index.html"

CODE=$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' -H "Origin: http://evil.example" "$GW/version")
[[ "$CODE" == "403" ]] || fail "forged Origin got $CODE, want 403"
CODE=$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' -H "Host: evil.example" "$GW/version")
[[ "$CODE" == "403" ]] || fail "forged Host got $CODE, want 403"
CODE=$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' \
  -H "Origin: http://evil.example" -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" -H "Sec-WebSocket-Version: 13" \
  "$GW/connections")
[[ "$CODE" == "403" ]] || fail "forged-Origin WS got $CODE, want 403"

CODE=$(curl -s -b "$JAR" -o "$WORK/config.js" -w '%{http_code}' "$GW/ui/config.js")
if [[ "$CODE" == "200" ]] && grep -q "$SECRET" "$WORK/config.js"; then
  fail "frontend asset config.js leaked secret (audit #7 ④)"
fi
log "STAGE 1 gateway PASS"

# ============ 阶段 2：节点切换（M1-plan §6.2）============
log "== STAGE 2: node switch"
now_of() {
  curl -s -b "$JAR" "$GW/proxies" | python3 -c \
    'import json,sys; print(json.load(sys.stdin)["proxies"]["FAKE-GROUP"]["now"])'
}
NOW1=$(now_of)
[[ "$NOW1" == "FAKE-HK-01" ]] || fail "initial now = $NOW1, want FAKE-HK-01"

"$TW" select FAKE-GROUP FAKE-JP-01 || fail "select failed"
NOW2=$(now_of)
[[ "$NOW2" == "FAKE-JP-01" ]] || fail "now after select = $NOW2, want FAKE-JP-01"
grep -q 'node: FAKE-JP-01' "$TW_HOME/profiles.yaml" || fail "profiles.yaml.selected not written"

"$TW" nodes | grep -q "FAKE-GROUP" || fail "nodes listing missing FAKE-GROUP"
"$TW" nodes FAKE-GROUP | grep -q "FAKE-JP-01" || fail "nodes FAKE-GROUP missing member"
"$TW" delay FAKE-GROUP | grep -q "unreachable" || fail "delay output missing per-node results"

log "stop -> start, asserting store-selected persistence"
"$TW" stop || fail "stop failed"
sleep 1
START_LOG2="$WORK/start2.log"
start_bg "$START_LOG2" --no-sysproxy
NOW3=$(now_of)
[[ "$NOW3" == "FAKE-JP-01" ]] || fail "now after restart = $NOW3, want FAKE-JP-01 (store-selected)"
grep -q "panel=http://127.0.0.1:$GATEWAY_PORT/ui/" "$START_LOG2" || fail "gateway missing after restart"
log "STAGE 2 node switch PASS"

# ============ 真实订阅（可选，M0 断言延续）============
if [[ -n "$WITH_URL" ]]; then
  log "add real subscription (--url) + hot reload"
  "$TW" add "$WITH_URL" --name real || fail "add real subscription failed"
  "$TW" update current || fail "update current failed"
  log "curl via proxy (expect 204)"
  CODE=$(curl -x "127.0.0.1:$MIXED_PORT" --max-time 10 -s -o /dev/null -w '%{http_code}' \
    http://www.gstatic.com/generate_204)
  [[ "$CODE" == "204" ]] || fail "proxied egress got HTTP $CODE, want 204"
fi

log "SIGTERM (graceful exit path; stop-snapshot hook runs before core stop)"
kill -TERM "$START_PID"
WAITED=0
while kill -0 "$START_PID" 2>/dev/null && ((WAITED < 100)); do
  sleep 0.2; WAITED=$((WAITED + 1))
done
kill -0 "$START_PID" 2>/dev/null && fail "start did not exit after SIGTERM"
START_PID=""

log "asserting cleanup"
[[ ! -e "$TW_HOME/core.sock" ]] || fail "core.sock was not unlinked"
ORPHANS="$(pgrep -f "$TW_HOME" 2>/dev/null || true)"
if [[ -n "$ORPHANS" ]]; then
  pgrep -fl "$TW_HOME" || true
  fail "orphan mihomo process(es) left behind"
fi
if port_busy "$MIXED_PORT"; then fail "port $MIXED_PORT still busy after stop"; fi
if port_busy "$GATEWAY_PORT"; then fail "port $GATEWAY_PORT still busy after stop"; fi
grep -q 'node: FAKE-JP-01' "$TW_HOME/profiles.yaml" || fail "stop snapshot lost selected record"

# ============ 阶段 3：sysproxy 回归（M1-plan §6.3，§7.10 兑现）============
log "== STAGE 3: sysproxy regression"
if pgrep -f "Clash Verge" >/dev/null 2>&1; then
  log "SKIP: sysproxy stage (Clash Verge is running; precheck per M1-plan §6.3)"
  log "SMOKE PASS (stage 3 skipped)"
  exit 0
fi
log "Verge not running: full sysproxy chain (start with sysproxy on)"
snapshot_sysproxy > "$WORK/sysproxy-before.txt"
START_LOG3="$WORK/start3.log"
start_bg "$START_LOG3"
grep -q "sysproxy=true" "$START_LOG3" || fail "stage 3 start did not apply sysproxy"
"$TW" status | grep -q "applied" || fail "status does not show sysproxy applied"
kill -TERM "$START_PID"
WAITED=0
while kill -0 "$START_PID" 2>/dev/null && ((WAITED < 100)); do
  sleep 0.2; WAITED=$((WAITED + 1))
done
kill -0 "$START_PID" 2>/dev/null && fail "stage 3 start did not exit after SIGTERM"
START_PID=""
snapshot_sysproxy > "$WORK/sysproxy-after.txt"
if ! diff -u "$WORK/sysproxy-before.txt" "$WORK/sysproxy-after.txt"; then
  fail "system proxy was NOT restored precisely"
fi
log "STAGE 3 sysproxy PASS"
log "SMOKE PASS"
