#!/usr/bin/env bash
# credit.sh — 积分查询（全部账号 + 总计）
#
# 用法:
#   ./credit.sh           # JSON 输出
#   ./credit.sh -pretty   # 美化日报
set -euo pipefail
cd "$(dirname "$0")"
BIN=./credit_bin
if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 credit_bin" >&2
        exit 1
    fi
    go build -o "$BIN" ./cmd/credit
fi
exec "$BIN" "$@"
