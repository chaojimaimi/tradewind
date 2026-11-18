#!/bin/bash
# tradewind M0 E2E 冒烟（M0-plan §6）
#
# 默认路径：隔离 TRADEDWIND_HOME（mktemp 临时目录）+ 端口 17897 + 不触碰系统代理。
# 用法：
#   scripts/smoke.sh                      默认冒烟（fixture，不出网断言）
#   scripts/smoke.sh --url <订阅URL>      追加真实订阅出网 204 断言（凭据不入库，仅写隔离目录）
#   scripts/smoke.sh --with-sysproxy      启用系统代理精确还原断言（要求本机 Clash Verge 已退出）
#
# 断言清单：add 成功 → start 握手 ready → status 退出码 0 → SIGTERM 优雅退出 →
# core.sock 已 unlink → 无孤儿 mihomo → 端口释放 → [--with-sysproxy] networksetup 四要素逐字段还原。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MIXED_PORT=17897
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

WITH_URL=""
WITH_SYSPROXY=0
while (($#)); do
  case "$1" in
    --url) WITH_URL="${2:?--url needs a value}"; shift 2 ;;
    --with-sysproxy) WITH_SYSPROXY=1; shift ;;
    *) fail "unknown argument: $1" ;;
  esac
done

# --- 系统代理快照（--with-sysproxy 断言的对照基准）---
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
if [[ $WITH_SYSPROXY -eq 1 ]]; then
  # 前提门槛：Clash Verge 在跑则拒绝（它会持有系统代理，避免互搏，M0-plan §8）
  pgrep -f "Clash Verge" >/dev/null 2>&1 && fail "--with-sysproxy requires Clash Verge to be quit first"
  snapshot_sysproxy > "$WORK/sysproxy-before.txt"
fi

log "building tradewind"
(cd "$REPO_ROOT" && go build -o "$TW" ./cmd/tradewind) || fail "go build failed"

log "staging kernel and geo assets into isolated home"
[[ -x "$DEFAULT_HOME/bin/mihomo" ]] || fail "kernel not found at $DEFAULT_HOME/bin/mihomo"
mkdir -p "$TW_HOME/bin"
cp "$DEFAULT_HOME/bin/mihomo" "$TW_HOME/bin/mihomo"
if [[ -f "$DEFAULT_HOME/bin/mihomo.sha256" ]]; then
  cp "$DEFAULT_HOME/bin/mihomo.sha256" "$TW_HOME/bin/mihomo.sha256"
else
  shasum -a 256 "$TW_HOME/bin/mihomo" | awk '{print $1}' > "$TW_HOME/bin/mihomo.sha256"
fi
# geo 缺失时从默认数据目录补齐（存在且非占位才复制——占位文件是下载失败的残留）
for f in geoip.metadb geosite.dat; do
  if [[ -f "$DEFAULT_HOME/$f" ]] && (("$(stat -f%z "$DEFAULT_HOME/$f")" > 1024)) && [[ ! -f "$TW_HOME/$f" ]]; then
    cp "$DEFAULT_HOME/$f" "$TW_HOME/$f"
  fi
done

log "writing settings (mixed_port=$MIXED_PORT)"
cat > "$TW_HOME/settings.yaml" <<EOF
mixed_port: $MIXED_PORT
mode: rule
log_level: info
EOF

log "pre-bind probe on $MIXED_PORT"
if port_busy "$MIXED_PORT"; then
  fail "port $MIXED_PORT already in use; stop the occupant or change settings"
fi

log "add --file fixture"
"$TW" add --file "$REPO_ROOT/scripts/fixtures/smoke-subscription.yaml" --name smoke \
  || fail "add fixture failed"

log "start (sysproxy=$([[ $WITH_SYSPROXY -eq 1 ]] && echo on || echo off))"
START_ARGS=()
if [[ $WITH_SYSPROXY -ne 1 ]]; then
  START_ARGS+=(--no-sysproxy)
fi
"$TW" start "${START_ARGS[@]}" > "$WORK/start.log" 2>&1 &
START_PID=$!

log "waiting for handshake (<=15s, fail-fast on early exit)"
READY=0
for _ in $(seq 1 75); do
  if ! kill -0 "$START_PID" 2>/dev/null; then
    echo "--- start.log ---"; cat "$WORK/start.log"
    fail "start process exited before ready"
  fi
  if grep -q '^ready:' "$WORK/start.log"; then READY=1; break; fi
  sleep 0.2
done
if [[ $READY -ne 1 ]]; then
  echo "--- start.log ---"; cat "$WORK/start.log"
  fail "handshake not ready within 15s"
fi
cat "$WORK/start.log"

log "status (expect exit 0)"
"$TW" status || fail "status exited non-zero"

if [[ -n "$WITH_URL" ]]; then
  # 真实订阅仅写入隔离目录，凭据不入仓库（M0-plan §6 fixture 二分）
  log "add real subscription (--url) + hot reload"
  "$TW" add "$WITH_URL" --name real || fail "add real subscription failed"
  "$TW" update current || fail "update current failed"
  log "curl via proxy (expect 204)"
  CODE="$(curl -x "127.0.0.1:$MIXED_PORT" --max-time 10 -s -o /dev/null -w '%{http_code}' \
    http://www.gstatic.com/generate_204)"
  [[ "$CODE" == "204" ]] || fail "proxied egress got HTTP $CODE, want 204"
fi

log "SIGTERM (graceful exit path)"
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

if [[ $WITH_SYSPROXY -eq 1 ]]; then
  log "asserting precise sysproxy restore (diff vs snapshot)"
  snapshot_sysproxy > "$WORK/sysproxy-after.txt"
  diff -u "$WORK/sysproxy-before.txt" "$WORK/sysproxy-after.txt" \
    || fail "system proxy was NOT restored precisely"
fi

log "SMOKE PASS"
