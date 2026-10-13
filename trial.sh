#!/usr/bin/env bash
# trial.sh — 新用户礼包领取（遍历 auths 全部账号，幂等）
#
# 用法: ./trial.sh [auths_dir]
set -euo pipefail
cd "$(dirname "$0")"
BIN=./trial_bin
if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 trial_bin" >&2
        exit 1
    fi
    go build -o "$BIN" ./cmd/trial
fi
exec "$BIN" "${1:-auths}"
