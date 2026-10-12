#!/usr/bin/env bash
# End-to-end verification: OxCost host serving the mimo2api external plugin.
# Run from the repo root: scripts/e2e-mimo2api.sh
#
# Reads the plugin key at runtime from /root/mimo2api/data/deploy.env
# (NEVER printed, NEVER persisted). Host key is set from a local test value
# so the 401 negative case is exercised with a *wrong* key.
set -euo pipefail

HOST_BIN="${HOST_BIN:-./target/debug/oxcost}"
LISTEN="${LISTEN:-127.0.0.1:8080}"
DEPLOY_ENV="/root/mimo2api/data/deploy.env"
HOST_PID=""

log() { printf '\n=== %s ===\n' "$*"; }
pass() { printf '[PASS] %s\n' "$*"; }
fail() { printf '[FAIL] %s\n' "$*"; exit 1; }

cleanup() {
  if [[ -n "$HOST_PID" ]]; then
    kill "$HOST_PID" 2>/dev/null || true
    wait "$HOST_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# 1. Source the plugin key from the deployment env (runtime only).
[[ -f "$DEPLOY_ENV" ]] || fail "deploy env not found: $DEPLOY_ENV"
set -a; source "$DEPLOY_ENV"; set +a
[[ -n "${MIMO2API_API_KEYS:-}" ]] || fail "MIMO2API_API_KEYS not set after sourcing"
export OXCOST_API_KEYS="e2e-host-test-key"
log "key sourced (value not shown), host key set"

# 2. Start the host with the real plugins dir.
"$HOST_BIN" --plugins plugins --listen "$LISTEN" &
HOST_PID=$!
for _ in $(seq 1 50); do
  curl -sf "http://$LISTEN/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sf "http://$LISTEN/healthz" >/dev/null || fail "host did not come up"

# 3. Health: mimo2api must be Serving.
log "GET /healthz"
HEALTH=$(curl -s "http://$LISTEN/healthz")
echo "$HEALTH" | python3 -c '
import json,sys
h = json.load(sys.stdin)
plugins = {p["name"]: p for p in h["plugins"]}
assert "mimo2api" in plugins, f"mimo2api missing: {list(plugins)}"
p = plugins["mimo2api"]
assert p["state"] == "Serving", "mimo2api state = " + p["state"]
print("mimo2api state:", p["state"])
' || fail "healthz check"
pass "healthz shows mimo2api Serving"

# 4. Models: at least one mimo2api/ model.
log "GET /v1/models"
MODELS=$(curl -s "http://$LISTEN/v1/models")
echo "$MODELS" | python3 -c '
import json,sys
m = json.load(sys.stdin)
ids = [x["id"] for x in m["data"]]
mimo = [i for i in ids if i.startswith("mimo2api/")]
assert mimo, f"no mimo2api/ models in {ids}"
print(len(mimo), "mimo2api models, e.g.", mimo[:3])
' || fail "models check"
pass "/v1/models contains mimo2api/ models"

AUTH=(-H "Authorization: Bearer $OXCOST_API_KEYS")

# 5. Real non-stream chat through the host.
log "POST /v1/chat/completions (stream:false, max_tokens:64)"
CHAT=$(curl -s "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"model":"mimo2api/mimo-v2.6-flash","messages":[{"role":"user","content":"Say OK"}],"max_tokens":64,"stream":false}' \
  "http://$LISTEN/v1/chat/completions")
echo "$CHAT" | python3 -c '
import json,sys
r = json.load(sys.stdin)
assert r["object"] == "chat.completion", "object = " + str(r.get("object"))
assert r["choices"][0]["message"]["role"] == "assistant"
assert "content" in r["choices"][0]["message"]
print("content:", r["choices"][0]["message"]["content"][:60])
' || fail "non-stream chat check"
pass "non-stream chat through host is OpenAI-shaped"

# 6. Real streaming call.
log "POST /v1/chat/completions (stream:true)"
STREAM_FILE=$(mktemp)
curl -s -N "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"model":"mimo2api/mimo-v2.6-flash","messages":[{"role":"user","content":"Say OK"}],"max_tokens":64,"stream":true}' \
  "http://$LISTEN/v1/chat/completions" > "$STREAM_FILE"
python3 - "$STREAM_FILE" <<'EOF'
import sys
data = open(sys.argv[1], "rb").read().decode("utf-8", "replace")
lines = [l for l in data.splitlines() if l.startswith("data: ")]
assert lines, "no SSE frames received"
first = lines[0]
assert '"object":"chat.completion.chunk"' in first.replace(" ", ""), "first frame not a chunk"
assert any(l.strip() == "data: [DONE]" for l in data.splitlines()), "stream did not end with [DONE]"
print(len(lines), "SSE frames; first frame is chat.completion.chunk; ends with [DONE]")
EOF
rm -f "$STREAM_FILE"
pass "stream: first chunk arrived, frames end with [DONE]"

# 7a. Negative: unknown model through host → 404 body.
log "negative: unknown model"
CODE=$(curl -s -o /tmp/oxcost-e2e-404.json -w '%{http_code}' "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"mimo2api/not-a-real-model","messages":[{"role":"user","content":"hi"}]}' \
  "http://$LISTEN/v1/chat/completions")
[[ "$CODE" == "404" ]] || fail "unknown model: HTTP $CODE"
python3 -c '
import json
e = json.load(open("/tmp/oxcost-e2e-404.json"))
assert e["error"]["code"] == "model_not_found", e
print("404 body:", e["error"]["code"])
' || fail "unknown model body"
rm -f /tmp/oxcost-e2e-404.json
pass "unknown model → 404 model_not_found"

# 7b. Negative: bad host key → 401.
log "negative: bad host key"
CODE=$(curl -s -o /tmp/oxcost-e2e-401.json -w '%{http_code}' \
  -H "Authorization: Bearer wrong-key" -H 'Content-Type: application/json' \
  -d '{"model":"mimo2api/mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}' \
  "http://$LISTEN/v1/chat/completions")
[[ "$CODE" == "401" ]] || fail "bad key: HTTP $CODE"
python3 -c '
import json
e = json.load(open("/tmp/oxcost-e2e-401.json"))
assert e["error"]["code"] == "invalid_api_key", e
print("401 body:", e["error"]["code"])
' || fail "bad key body"
rm -f /tmp/oxcost-e2e-401.json
pass "bad host key → 401 invalid_api_key"

# 8. Teardown (trap also covers failure paths).
log "teardown"
kill "$HOST_PID" 2>/dev/null || true
wait "$HOST_PID" 2>/dev/null || true
HOST_PID=""
pass "host stopped"

printf '\nE2E: ALL CHECKS PASSED\n'
