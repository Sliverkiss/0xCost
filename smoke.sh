#!/usr/bin/env bash
# smoke.sh — 用真实 token 对线上游做只读冒烟验证（绝不在 go test 里跑）。
#
# 数据源：/root/richee_test_token.json（人工验证专用，勿提交勿进 CI）。
# 只读端点：credit/info、userSign/today、gift/status、claw/user/current。
# 最后附一次 chat 5-token 探针（最小计费，验证 SSE 链路）。
set -euo pipefail

TOKEN_FILE="${RICHEE_TOKEN_FILE:-/root/richee_test_token.json}"
BASE="${RICHEE_BASE:-https://claw.richee.cn/claw-api}"

if [[ ! -f "$TOKEN_FILE" ]]; then
    echo "token file not found: $TOKEN_FILE" >&2
    exit 1
fi
TOKEN=$(python3 -c "import json; print(json.load(open('$TOKEN_FILE'))['token'])")

echo "== credit/info =="
curl -sS -m 15 "$BASE/credit/info" -H "richee-token: $TOKEN"
echo; echo
echo "== userSign/today =="
curl -sS -m 15 "$BASE/userSign/today" -H "richee-token: $TOKEN"
echo; echo
echo "== gift/status =="
curl -sS -m 15 "$BASE/gift/status" -H "richee-token: $TOKEN"
echo; echo
echo "== claw/user/current =="
curl -sS -m 15 "$BASE/claw/user/current" -H "richee-token: $TOKEN"
echo; echo
echo "== chat probe (5 tokens) =="
curl -sS -m 30 -N "$BASE/claw/v1/chat/completions" \
  -H "richee-token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":5}' | head -c 800
echo
echo "== smoke done =="
