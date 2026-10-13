#!/usr/bin/env bash
# checkin.sh — 批量签到（ signin_bin 表格输出的薄包装）
#
# 用法:
#   ./checkin.sh            # 网关状态头 + 签到表格
#   ./checkin.sh auths_dir  # 指定 auths 目录
#
# 依赖: go（构建 signin_bin）、curl
# 环境变量:
#   R2A_CONFIG  配置文件路径（默认 ./config.json）
#   R2A_URL     直接指定服务地址（设置后忽略配置文件）
set -euo pipefail
cd "$(dirname "$0")"

AUTHS_DIR="${1:-auths}"

if ! command -v curl >/dev/null 2>&1; then
    echo "需要 curl" >&2
    exit 1
fi

# ─── 网关状态头：GET /status 判活（活着 = 定时签到已在网关侧运行）───
CFG=${R2A_CONFIG:-config.json}
PORT=7863
KEY=""
if [[ -f "$CFG" ]] && command -v jq >/dev/null 2>&1; then
    LISTEN=$(jq -r '.listen // ":7863"' "$CFG")
    PORT=${LISTEN##*:}
    KEY=$(jq -r '.api_key // ""' "$CFG")
fi
PORT=${PORT:-7863}
BASE=${R2A_URL:-http://localhost:$PORT}
AUTH=()
[[ -n "$KEY" ]] && AUTH=(-H "Authorization: Bearer $KEY")

RESP=$(curl -s -w $'\n%{http_code}' ${AUTH+"${AUTH[@]}"} "$BASE/status" || true)
if [[ -n "$RESP" ]]; then
    CODE=${RESP##*$'\n'}
    if [[ "$CODE" == "200" ]]; then
        echo "── 网关在线（/status 200）：定时签到已在网关侧运行（09/21 点）。"
    else
        echo "── 网关不在线（HTTP $CODE）：下面跑离线签到工具。"
    fi
else
    echo "── 连不上 $BASE：下面跑离线签到工具。"
fi
echo

# ─── 核心：构建并执行 signin_bin ───
BIN=./signin_bin
if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
    echo "build signin_bin ..."
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 signin_bin" >&2
        exit 1
    fi
    go build -o "$BIN" ./cmd/signin
fi
exec "$BIN" "$AUTHS_DIR"
