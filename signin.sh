#!/usr/bin/env bash
# signin.sh — 发送 SMS 验证码（登录第一步独立包装）
#
# 用法: ./signin.sh <phone>
#
# 与 login.sh 的分工：本脚本只发码（POST /claw/auth/verify-code/send），
# 不做登录。适合「先发码再择时登录」的场景。
set -euo pipefail
cd "$(dirname "$0")"

if [[ $# -lt 1 ]]; then
    echo "用法: $0 <phone>" >&2
    exit 2
fi

LOGIN_BIN="./login_bin"
if [[ ! -x "$LOGIN_BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$LOGIN_BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 login_bin" >&2
        exit 1
    fi
    go build -o "$LOGIN_BIN" ./cmd/login
fi
exec "$LOGIN_BIN" send "$1"
