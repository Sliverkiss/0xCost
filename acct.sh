#!/usr/bin/env bash
# acct.sh — 账号运维工具包装（临时停用 / 恢复 / 复活）
#
# 用法:
#   ./acct.sh list                     # 列出账号与双位状态
#   ./acct.sh disable <uid> [原因]     # 临时停用（对话流量摘除，保留在池里）
#   ./acct.sh enable  <uid>            # 解除手动停用
#   ./acct.sh revive  <uid>            # 解除系统自动禁用（token 失效位）
#
# 需 config 里 admin.enabled = true（且 api_key 非空）。
#
# 二进制升级: go build -o acct_bin ./cmd/acct
set -euo pipefail
cd "$(dirname "$0")"
BIN=./acct_bin
if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 acct_bin" >&2
        exit 1
    fi
    go build -o "$BIN" ./cmd/acct
fi
exec "$BIN" "$@"
