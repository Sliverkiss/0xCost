#!/usr/bin/env bash
# dev.sh — 开发模式：构建并前台运行网关（Ctrl+C 退出）
#
# 用法: ./dev.sh [config.json]
set -euo pipefail
cd "$(dirname "$0")"
CFG="${1:-config.json}"
if [[ ! -f "$CFG" ]]; then
    echo "缺少 $CFG（可先 cp config.example.json config.json）" >&2
    exit 1
fi
mkdir -p auths data
exec go run ./cmd/server -config "$CFG"
