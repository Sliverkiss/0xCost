#!/usr/bin/env bash
# live-seed-mimo.sh (R1-W2a): fetch ONE account row from the maintainer's
# Upstash Redis (REST, GET only) and export the plugin's env family so the
# OxCost host registers the mimo-rust SDK plugin with a live session.
#
# Secrets are env-only: this script never writes them to any file and
# never echoes them. Source it:
#   set -a; source scripts/live-seed-mimo.sh; set +a
# (or run it as a command prefix: eval "$(scripts/live-seed-mimo.sh env)")
#
# Env it consumes (injected by the operator/orchestrator):
#   UPSTASH_REDIS_REST_URL   e.g. https://known-mudfish-135209.upstash.io
#   UPSTASH_REDIS_REST_TOKEN injected, never committed
#   MIMO_LIVE_KEY            Upstash row key (default the one smoke account)
#
# Env it exports:
#   MIMO_ACCOUNT_1_ID / _PHONE / _PASSWORD / _EGRESS   (account seeding)
#   MIMO_RUST_SESSION_1_USER_ID / _PASS_TOKEN /        (live session,
#     _COOKIE / _PH / _DEVICE                           store columns)
#   MIMO_RUST_DB_PATH                                   (plugin store path)
#
# The row's `session` field carries cookie+ph; when absent the plugin
# falls back to pass_token renewal (user_id+pass_token still exported).

set -euo pipefail

MIMO_LIVE_KEY="${MIMO_LIVE_KEY:-mimo2api:accounts:18278724616}"
DB_DIR="${DB_DIR:-/tmp/oxcost-mimo-live}"

need() { [[ -n "${!1:-}" ]] || { echo "live-seed-mimo: $1 is not set" >&2; return 1; }; }
need UPSTASH_REDIS_REST_URL
need UPSTASH_REDIS_REST_TOKEN

RESP=$(curl -sf -m 15 -X POST \
  -H "Authorization: Bearer ${UPSTASH_REDIS_REST_TOKEN}" \
  "${UPSTASH_REDIS_REST_URL}/get/${MIMO_LIVE_KEY}") || {
  echo "live-seed-mimo: Upstash GET failed" >&2
  return 1 2>/dev/null || exit 1
}

mkdir -p "$DB_DIR"

# Row → env mapping (pure, offline-tested shape; values stay in env).
# Emits `export K=...` lines the caller evals (source mode) or prints
# for `eval "$(scripts/live-seed-mimo.sh env)"`.
emit_exports() {
  RESP="$RESP" DB_DIR="$DB_DIR" python3 - <<'PY'
import json, os, shlex
row = json.loads(json.loads(os.environ["RESP"])["result"])
db = os.environ["DB_DIR"]
def out(k, v):
    print(f"export {k}={shlex.quote(str(v))}")
out("MIMO_ACCOUNT_1_ID", row["id"])
out("MIMO_ACCOUNT_1_PHONE", row["phone"])
if row.get("password"):
    out("MIMO_ACCOUNT_1_PASSWORD", row["password"])
out("MIMO_ACCOUNT_1_EGRESS", "direct")
out("MIMO_RUST_DB_PATH", os.path.join(db, "mimo-rust.db"))
sess = row.get("session") or {}
if row.get("user_id"):
    out("MIMO_RUST_SESSION_1_USER_ID", row["user_id"])
if row.get("pass_token"):
    out("MIMO_RUST_SESSION_1_PASS_TOKEN", row["pass_token"])
if sess.get("cookie") and sess.get("ph"):
    out("MIMO_RUST_SESSION_1_COOKIE", sess["cookie"])
    out("MIMO_RUST_SESSION_1_PH", sess["ph"])
if row.get("device"):
    out("MIMO_RUST_SESSION_1_DEVICE",
        json.dumps(row["device"], separators=(",", ":")))
PY
}

case "${1:-source}" in
  env)    emit_exports ;;                      # eval "$(scripts/live-seed-mimo.sh env)"
  source) eval "$(emit_exports)" ;;
  *) echo "usage: scripts/live-seed-mimo.sh [env|source]" >&2; exit 2 ;;
esac

echo "live-seed-mimo: exported MIMO_ACCOUNT_1_* + MIMO_RUST_SESSION_1_* (values not shown)" >&2
