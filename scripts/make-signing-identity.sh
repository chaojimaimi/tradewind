#!/bin/bash
# Tradewind 自签代码签名身份（批⑥ signing identity）：幂等、可重复执行。
#
# 背景：壳此前以 ad-hoc 签名（codesign -）交付，Sparkle 更新链路的安装期签名校验
# 因此未生效（batch5 §3.4 前置动作原靠 Keychain Access 手工建证书）。本脚本把
# "生成自签代码签名证书 + 私钥"收敛成一条可重复命令，供 apps/menubar/build.sh 的
# codesign 使用。
#
# 产物（密码/私钥绝不打印、不落日志、不进仓库；成功只打印证书 SHA-1 与钥匙串路径）：
#   $SIGN_KEYCHAIN            钥匙串（tradewind-signing.keychain-db）
#   $SIGN_KEYCHAIN_PASS_FILE  钥匙串密码（600，随机生成——不是登录密码）
#
# 用法：
#   scripts/make-signing-identity.sh                              # 身份名 "Tradewind Developer"
#   SIGN_IDENTITY="自定义名" scripts/make-signing-identity.sh     # 自定义身份名
#   SIGN_KEYCHAIN=<路径> SIGN_KEYCHAIN_PASS_FILE=<路径> scripts/make-signing-identity.sh
#
# 不加信任设置：`security find-identity -v` 会滤掉它（CSSMERR_TP_NOT_TRUSTED），这是
# 预期——Sparkle 的签名比对是静态校验，不要求受信任；加信任需要 GUI 授权，不做。
# build.sh 据此在不带 -v 的 find-identity 里探测它。
set -euo pipefail
umask 077

: "${HOME:?HOME 未设置}"
SIGN_IDENTITY="${SIGN_IDENTITY:-Tradewind Developer}"
SIGN_KEYCHAIN="${SIGN_KEYCHAIN:-$HOME/.sparkle/tradewind-signing.keychain-db}"
SIGN_KEYCHAIN_PASS_FILE="${SIGN_KEYCHAIN_PASS_FILE:-$HOME/.sparkle/tradewind-signing.pass}"

# 身份名进 openssl -subj 与 find-identity 的引号匹配：这些字符会破坏二者之一。
case "$SIGN_IDENTITY" in
''|*/*|*\'*|*\"*|*\\*)
	echo "error: SIGN_IDENTITY 不能为空，且不能包含 / ' \" \\" >&2
	exit 1
	;;
esac

die() {
	echo "error: $*" >&2
	exit 1
}

# find_openssl 选出 OpenSSL（非 LibreSSL）：PATH 优先，其次 Homebrew 前缀。
# WHY：证书扩展靠 req -addext，LibreSSL 不保证支持。
find_openssl() {
	local candidate
	for candidate in "$(command -v openssl 2>/dev/null || true)" /opt/homebrew/opt/openssl*/bin/openssl; do
		[ -n "$candidate" ] && [ -x "$candidate" ] || continue
		case "$("$candidate" version 2>/dev/null || true)" in
		OpenSSL*) echo "$candidate"; return 0 ;;
		esac
	done
	return 1
}

# identity_sha1 打印钥匙串内同名证书的 SHA-1（公开信息）；找不到返回非零。
identity_sha1() {
	security find-certificate -c "$SIGN_IDENTITY" -Z "$SIGN_KEYCHAIN" 2>/dev/null \
		| awk -F': ' '$1 == "SHA-1 hash" { print $2; found = 1 } END { exit found ? 0 : 1 }'
}

# identity_usable 钥匙串内有同名、未过期的代码签名身份（证书 + 私钥）。
# 单进程 awk 做过滤 + 字面匹配：pipefail 下 `grep -v | grep -q` 的 grep 提前退出会让
# 前段吃 SIGPIPE。
identity_usable() {
	security find-identity -p codesigning "$SIGN_KEYCHAIN" 2>/dev/null \
		| awk -v name="\"$SIGN_IDENTITY\"" '/EXPIRED/ { next } index($0, name) { found = 1 } END { exit found ? 0 : 1 }'
}

# --- 落点目录（700）与密码 ---

KEYCHAIN_DIR="$(dirname "$SIGN_KEYCHAIN")"
if [ ! -d "$KEYCHAIN_DIR" ]; then
	mkdir -p "$KEYCHAIN_DIR" # umask 077 → 新建目录 700
elif [ "$KEYCHAIN_DIR" = "$HOME/.sparkle" ]; then
	# 既有约定目录（release.sh 的 EdDSA 私钥同在此）：显式收紧——mkdir -p 不改
	# 已存在目录的权限
	chmod 700 "$KEYCHAIN_DIR"
fi
PASS_DIR="$(dirname "$SIGN_KEYCHAIN_PASS_FILE")"
[ -d "$PASS_DIR" ] || mkdir -p "$PASS_DIR"

# PASS 仅存于变量与密码文件；从不 echo、不进日志。
PASS=""
if [ -f "$SIGN_KEYCHAIN_PASS_FILE" ]; then
	PASS="$(tr -d '\r\n' < "$SIGN_KEYCHAIN_PASS_FILE")"
	chmod 600 "$SIGN_KEYCHAIN_PASS_FILE"
fi

# --- 幂等：钥匙串已存在 → 能解锁且同名证书已在则 no-op ---

if [ -f "$SIGN_KEYCHAIN" ]; then
	[ -n "$PASS" ] || die "钥匙串已存在但密码文件缺失或为空：$SIGN_KEYCHAIN_PASS_FILE（无法解锁；删除钥匙串与密码文件后重跑）"
	if ! security unlock-keychain -p "$PASS" "$SIGN_KEYCHAIN" 2>/dev/null; then
		die "钥匙串 $SIGN_KEYCHAIN 无法用 $SIGN_KEYCHAIN_PASS_FILE 解锁（密码不匹配）；删除钥匙串与密码文件后重跑"
	fi
	if SHA1="$(identity_sha1)"; then
		echo "==> codesign identity \"$SIGN_IDENTITY\" already present in $SIGN_KEYCHAIN (SHA-1: $SHA1); nothing to do"
		exit 0
	fi
fi

OPENSSL="$(find_openssl)" || die "找不到 OpenSSL（需支持 -addext；LibreSSL 不保证支持）——请 brew install openssl"

if [ ! -f "$SIGN_KEYCHAIN" ]; then
	[ -n "$PASS" ] || PASS="$("$OPENSSL" rand -hex 32)"
	# 密码先落盘（600）再建钥匙串：半途失败后重跑可凭密码文件复用同一钥匙串
	printf '%s\n' "$PASS" > "$SIGN_KEYCHAIN_PASS_FILE"
	chmod 600 "$SIGN_KEYCHAIN_PASS_FILE"
	# create-keychain 同时把新钥匙串加入搜索列表（build.sh 的 find-identity 据此可见）
	security create-keychain -p "$PASS" "$SIGN_KEYCHAIN"
fi

# --- 生成自签证书 → PKCS12 → 导入（私钥材料只存在于本进程的私有临时目录）---

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tradewind-signing.XXXXXX")"
trap 'rm -f "$TMP_DIR/key.pem" "$TMP_DIR/cert.pem" "$TMP_DIR/identity.p12" "$TMP_DIR/req.err" "$TMP_DIR/p12.err"; rmdir "$TMP_DIR" 2>/dev/null || true' EXIT

echo "==> generating self-signed code-signing certificate \"$SIGN_IDENTITY\" (RSA 2048, 10 years)"
if ! "$OPENSSL" req -x509 -newkey rsa:2048 -sha256 -nodes -days 3650 \
	-subj "/CN=$SIGN_IDENTITY" \
	-addext "basicConstraints=critical,CA:false" \
	-addext "keyUsage=critical,digitalSignature" \
	-addext "extendedKeyUsage=critical,codeSigning" \
	-keyout "$TMP_DIR/key.pem" -out "$TMP_DIR/cert.pem" >/dev/null 2>"$TMP_DIR/req.err"; then
	cat "$TMP_DIR/req.err" >&2
	die "openssl req 生成自签证书失败"
fi

# PKCS12 必须用兼容算法：macOS security import 不认 OpenSSL 3+ 默认的新算法
# （实测 MAC verification failed）。密码经 -passout file: 从密码文件读取，不进 argv。
if ! "$OPENSSL" pkcs12 -export -inkey "$TMP_DIR/key.pem" -in "$TMP_DIR/cert.pem" -name "$SIGN_IDENTITY" \
	-keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1 \
	-passout "file:$SIGN_KEYCHAIN_PASS_FILE" -out "$TMP_DIR/identity.p12" 2>"$TMP_DIR/p12.err"; then
	cat "$TMP_DIR/p12.err" >&2
	die "openssl pkcs12 导出失败"
fi

echo "==> importing into $SIGN_KEYCHAIN"
# -T：允许 codesign / security 无提示访问该私钥
security import "$TMP_DIR/identity.p12" -k "$SIGN_KEYCHAIN" -P "$PASS" \
	-T /usr/bin/codesign -T /usr/bin/security >/dev/null
# 分区列表（apple-tool: / apple:）：用钥匙串自己的随机密码即可，零弹窗
security set-key-partition-list -S apple-tool:,apple: -s -k "$PASS" "$SIGN_KEYCHAIN" >/dev/null

# --- 核验后汇报（只打印公开信息）---

SHA1="$(identity_sha1)" || die "导入后未在钥匙串中找到同名证书；删除 $SIGN_KEYCHAIN 与密码文件后重跑"
identity_usable || die "钥匙串中没有可用的代码签名身份（证书 + 私钥）；删除 $SIGN_KEYCHAIN 与密码文件后重跑"
echo "==> codesign identity: \"$SIGN_IDENTITY\""
echo "==> certificate SHA-1: $SHA1"
echo "==> keychain: $SIGN_KEYCHAIN"
echo "note: 未加信任设置——security find-identity -v 滤掉它（CSSMERR_TP_NOT_TRUSTED）属预期；build.sh 不带 -v 探测"
