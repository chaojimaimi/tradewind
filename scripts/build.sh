#!/bin/bash
# Tradewind 统一构建脚本（M1-plan §5.3）：smoke 与发版共用，保证版本注入单一来源。
#
# 用法：
#   scripts/build.sh                    # 版本 = git describe --tags --always
#   VERSION=v0.2.0-m1 scripts/build.sh  # 显式覆盖版本
#   OUT=/tmp/tradewind scripts/build.sh # 覆盖输出路径（默认 dist/tradewind）
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="${VERSION:-$(git -C "$REPO_ROOT" describe --tags --always)}"
OUT="${OUT:-$REPO_ROOT/dist/tradewind}"

mkdir -p "$(dirname "$OUT")"
# -trimpath：产物不含本机路径；-s -w：瘦身（个人分发不需要符号表）
go build -trimpath -ldflags "-s -w -X github.com/chaojimaimi/tradewind/internal/version.Version=$VERSION" \
  -o "$OUT" "$REPO_ROOT/cmd/tradewind"
echo "built $OUT (version $VERSION)"
