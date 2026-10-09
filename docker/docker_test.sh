#!/usr/bin/env bash
# DOCKER-001..006 delivery checks for mimo2api (docs/TDD.md §3.11).
#
# Runnable directly (`bash docker/docker_test.sh`) or via `make test-docker`.
# Skips cleanly (exit 0, "SKIP: docker unavailable") when docker is absent or
# its daemon is unreachable. Every fixture uses FAKE credentials; the repo's
# real data/accounts.json is never read, copied, or mounted.
#
# Evidence (image tag, container id, raw /healthz body, `stat -c '%a %n'`)
# is printed as the checks run.
set -u

IMAGE="${IMAGE:-mimo2api:test}"
APP_PORT="${APP_PORT:-18080}"       # host port for the docker-run container
COMPOSE_PORT="${COMPOSE_PORT:-18081}" # host port for the compose service
MOCK_PORT="${MOCK_PORT:-18099}"
CONTAINER="mimo2api-dtest"
COMPOSE_NAME="mimo2api-dtest-compose"
FAKE_PHONE="13000000000"
FAKE_PASSWORD="fake-password"
FAKE_TOKEN="FAKE-session-token-ccc000"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/mimo2api-docker.XXXXXX")"
DATA_DIR="$WORK/data"
BUILD_LOG="$WORK/build.log"
MOCK_PID=""

pass=0
fail=0
say()  { printf '%s\n' "$*"; }
PASS() { say "PASS: $1"; pass=$((pass + 1)); }
FAIL() { say "FAIL: $1"; fail=$((fail + 1)); }

cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" >/dev/null 2>&1
  docker rm -f "$CONTAINER" "$COMPOSE_NAME" >/dev/null 2>&1
  if [ -f "$WORK/override.yml" ]; then
    ( cd "$REPO_ROOT" && docker compose -f docker-compose.yml -f "$WORK/override.yml" down -v ) >/dev/null 2>&1
  fi
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT

# --- docker availability gate ---------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  say "SKIP: docker unavailable"
  exit 0
fi
if ! docker info >/dev/null 2>&1; then
  say "SKIP: docker unavailable (daemon not reachable)"
  exit 0
fi

# Small HTTP status helper: curl when present, else python3.
http_code() {
  if command -v curl >/dev/null 2>&1; then
    curl -s -o /dev/null -w '%{http_code}' "$1" 2>/dev/null
  else
    python3 -c 'import sys,urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1],timeout=3).status)
except Exception:
    print("000")' "$1" 2>/dev/null
  fi
}
http_body() {
  if command -v curl >/dev/null 2>&1; then
    curl -s "$1" 2>/dev/null
  else
    python3 -c 'import sys,urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1],timeout=3).read().decode())
except Exception:
    pass' "$1" 2>/dev/null
  fi
}

say "mimo2api docker delivery harness — image=$IMAGE repo=$REPO_ROOT"
say "workspace=$WORK"
say ""

# ===========================================================================
say "=== DOCKER-001: image builds (multi-stage, distroless nonroot, CGO_ENABLED=0) ==="
if docker build --progress=plain -t "$IMAGE" "$REPO_ROOT" >"$BUILD_LOG" 2>&1; then
  ok=1
  grep -q 'CGO_ENABLED=0' "$BUILD_LOG" || { say "  ! CGO_ENABLED=0 not found in build log"; ok=0; }
  nfrom="$(grep -cE '^FROM ' "$REPO_ROOT/Dockerfile")"
  [ "$nfrom" -ge 2 ] || { say "  ! Dockerfile has $nfrom FROM stage(s), want >=2 (multi-stage)"; ok=0; }
  user="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
  [ "$user" = "nonroot" ] || { say "  ! Config.User=$user (want nonroot)"; ok=0; }
  if docker run --rm --entrypoint /bin/sh "$IMAGE" true >/dev/null 2>&1; then
    say "  ! /bin/sh runs inside the image (not distroless)"; ok=0
  fi
  cid="$(docker create "$IMAGE")"
  docker cp "$cid":/mimo2api "$WORK/mimo2api.bin" >/dev/null 2>&1
  docker rm "$cid" >/dev/null 2>&1
  ldd "$WORK/mimo2api.bin" 2>&1 | grep -q 'not a dynamic executable' \
    || { say "  ! embedded binary is not statically linked"; ok=0; }
  if [ "$ok" = 1 ]; then
    PASS "DOCKER-001 (image '$IMAGE' built; multi-stage; distroless-nonroot; static, CGO_ENABLED=0)"
  else
    FAIL "DOCKER-001 (build ok but image assertions failed)"
  fi
else
  FAIL "DOCKER-001 (docker build failed)"
  tail -n 25 "$BUILD_LOG"
fi

# --- shared runtime: fixture, mock upstream, container ---------------------
mkdir -p "$DATA_DIR"
cat >"$DATA_DIR/accounts.json" <<JSON
{
  "accounts": [
    {
      "id": "acct-docker-003",
      "phone": "$FAKE_PHONE",
      "password": "$FAKE_PASSWORD",
      "enabled": true,
      "label": "docker-test",
      "user_id": "88888888",
      "pass_token": "FAKE-pass-token",
      "session": {"cookie": "userId=88888888; xiaomichatbot_serviceToken=\"STALE-TOKEN\"", "ph": "STALE-ph", "fetched_at": 1},
      "state": "cooldown",
      "cooldown_until": 1
    }
  ]
}
JSON
chmod 0644 "$DATA_DIR/accounts.json"

MOCK_UP=0
if command -v python3 >/dev/null 2>&1; then
  python3 "$SCRIPT_DIR/mock_upstream.py" "$MOCK_PORT" >"$WORK/mock.log" 2>&1 &
  MOCK_PID=$!
  sleep 0.5
  MOCK_UP=1
fi

# /data must be writable by the image's non-root user (DOCKER-004).
RUNUID="$(docker create "$IMAGE" 2>/dev/null)"
RUNUID="$(docker cp "$RUNUID":/etc/passwd - 2>/dev/null | tr -d '\000' | awk -F: '$1=="nonroot"{print $3}')"
[ -n "${RUNUID:-}" ] || RUNUID=65532
if ! chown -R "$RUNUID":"$RUNUID" "$DATA_DIR" 2>/dev/null; then
  chmod -R 0777 "$DATA_DIR" 2>/dev/null || true
fi

docker rm -f "$CONTAINER" >/dev/null 2>&1
docker run -d --name "$CONTAINER" \
  -p "$APP_PORT":8080 \
  --add-host account.xiaomi.com:host-gateway \
  -e MIMO2API_API_KEYS=test-key \
  -e MIMO2API_ACCOUNTS_FILE=/data/accounts.json \
  -e MIMO2API_UPSTREAM_BASE="http://account.xiaomi.com:$MOCK_PORT" \
  -e MIMO2API_PROBE_INTERVAL_S=1 \
  -v "$DATA_DIR":/data \
  "$IMAGE" >/dev/null
CID="$(docker ps -q --filter "name=^/${CONTAINER}$")"
say "container id: $CID"

# wait for the server to answer /healthz (start_period analogue).
ready=0
for _ in $(seq 1 60); do
  [ "$(http_code "http://127.0.0.1:$APP_PORT/healthz")" = "200" ] && { ready=1; break; }
  sleep 0.5
done
say ""

# ===========================================================================
say "=== DOCKER-002: in-binary healthcheck subcommand (exit 0 running, non-zero stopped) ==="
if [ "$ready" = 1 ]; then
  docker exec "$CONTAINER" /mimo2api healthcheck; running_ec=$?
else
  running_ec=1
fi
say "  running:  docker exec $CONTAINER /mimo2api healthcheck -> exit $running_ec"
docker run --rm --entrypoint /mimo2api "$IMAGE" healthcheck >/dev/null 2>&1; stopped_ec=$?
say "  stopped:  docker run --entrypoint /mimo2api $IMAGE healthcheck -> exit $stopped_ec"
curl_present=0; wget_present=0
docker run --rm --entrypoint /usr/bin/curl "$IMAGE" >/dev/null 2>&1 && curl_present=1
docker run --rm --entrypoint /usr/bin/wget "$IMAGE" >/dev/null 2>&1 && wget_present=1
say "  curl in image=$curl_present  wget in image=$wget_present (must both be 0)"
if [ "$running_ec" = 0 ] && [ "$stopped_ec" != 0 ] && [ "$curl_present" = 0 ] && [ "$wget_present" = 0 ]; then
  PASS "DOCKER-002 (running exit 0, stopped exit $stopped_ec, no curl/wget in image)"
else
  FAIL "DOCKER-002 (running=$running_ec stopped=$stopped_ec curl=$curl_present wget=$wget_present)"
fi
say ""

# ===========================================================================
say "=== DOCKER-003: /data volume round-trips accounts.json at mode 0600 ==="
if [ "$MOCK_UP" = 0 ]; then
  say "  SKIP: python3 unavailable — cannot script the session renewal"
else
  before_md5="$(md5sum "$DATA_DIR/accounts.json" | awk '{print $1}')"
  say "  before: $(stat -c '%a %n' "$DATA_DIR/accounts.json") md5=$before_md5"
  read_ok=0
  docker logs "$CONTAINER" 2>&1 | grep -q 'accounts: 1 enabled' && read_ok=1
  after_md5="$before_md5"
  deadline=$((SECONDS + 45))
  while [ "$SECONDS" -lt "$deadline" ] && [ "$after_md5" = "$before_md5" ]; do
    sleep 1
    after_md5="$(md5sum "$DATA_DIR/accounts.json" 2>/dev/null | awk '{print $1}')"
  done
  mode_after="$(stat -c '%a' "$DATA_DIR/accounts.json")"
  say "  after:  $(stat -c '%a %n' "$DATA_DIR/accounts.json") md5=$after_md5"
  say "  container read the account: $read_ok; file rewritten: $([ "$after_md5" != "$before_md5" ] && echo 1 || echo 0); mode=$mode_after"
  if [ "$read_ok" = 1 ] && [ "$after_md5" != "$before_md5" ] && [ "$mode_after" = "600" ] \
     && grep -q "$FAKE_TOKEN" "$DATA_DIR/accounts.json"; then
    PASS "DOCKER-003 (/data read; runtime state persisted; mode 0600 on host)"
  else
    FAIL "DOCKER-003 (read=$read_ok rewritten=$([ "$after_md5" != "$before_md5" ] && echo 1 || echo 0) mode=$mode_after)"
    docker logs "$CONTAINER" 2>&1 | tail -n 5
  fi
fi
say ""

# ===========================================================================
say "=== DOCKER-004: non-root runtime (uid != 0) ==="
img_user="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
top_out="$(docker top "$CONTAINER" -o pid,uid,user,comm 2>&1)"
run_uid="$(printf '%s\n' "$top_out" | sed -n '2p' | awk '{print $2}')"
say "  image Config.User=$img_user"
say "  docker top $CONTAINER -o pid,uid,user,comm:"
printf '%s\n' "$top_out" | sed 's/^/    /'
if [ "$img_user" = "nonroot" ] && [ -n "${run_uid:-}" ] && [ "$run_uid" != 0 ]; then
  PASS "DOCKER-004 (user=nonroot, runtime uid=$run_uid; distroless nonroot is uid 65532)"
else
  FAIL "DOCKER-004 (user=$img_user runtime_uid=${run_uid:-<none>})"
fi
say ""

# ===========================================================================
say "=== DOCKER-005: docker compose config validity ==="
if ( cd "$REPO_ROOT" && docker compose config >"$WORK/compose_config.yml" 2>"$WORK/compose_config.err" ); then
  ok=1
  grep -q 'healthcheck' "$WORK/compose_config.yml" || { say "  ! healthcheck block missing"; ok=0; }
  grep -q -- '/mimo2api' "$WORK/compose_config.yml" || { say "  ! healthcheck command missing"; ok=0; }
  grep -q 'MIMO2API_ACCOUNTS_FILE' "$WORK/compose_config.yml" || { say "  ! accounts-file env missing"; ok=0; }
  grep -q '/data' "$WORK/compose_config.yml" || { say "  ! /data volume mount missing"; ok=0; }
  if [ "$ok" = 1 ]; then
    PASS "DOCKER-005 (docker compose config valid; healthcheck + /data mount + env present)"
  else
    FAIL "DOCKER-005 (compose config valid but expected keys missing)"
  fi
else
  FAIL "DOCKER-005 (docker compose config failed)"
  cat "$WORK/compose_config.err"
fi
say ""

# ===========================================================================
say "=== DOCKER-006: container serves GET /healthz (200) and reaches healthy ==="
body="$(http_body "http://127.0.0.1:$APP_PORT/healthz")"
code="$(http_code "http://127.0.0.1:$APP_PORT/healthz")"
say "  /healthz (docker run, :$APP_PORT) HTTP $code body=$body"

cat >"$WORK/override.yml" <<YAML
services:
  mimo2api:
    image: $IMAGE
    container_name: $COMPOSE_NAME
    ports: !override
      - "$COMPOSE_PORT:8080"
    volumes: !override
      - $DATA_DIR:/data
    environment:
      - MIMO2API_API_KEYS=test-key
      - MIMO2API_ACCOUNTS_FILE=/data/accounts.json
    extra_hosts:
      - "account.xiaomi.com:host-gateway"
    healthcheck:
      test: ["CMD", "/mimo2api", "healthcheck"]
      interval: 2s
      timeout: 3s
      retries: 5
      start_period: 3s
YAML
( cd "$REPO_ROOT" && docker compose -f docker-compose.yml -f "$WORK/override.yml" up -d ) >/dev/null 2>&1
health=""
cbody=""; ccode=000
for _ in $(seq 1 40); do
  health="$(docker inspect --format '{{.State.Health.Status}}' "$COMPOSE_NAME" 2>/dev/null)"
  ccode="$(http_code "http://127.0.0.1:$COMPOSE_PORT/healthz")"
  [ "$health" = "healthy" ] && break
  sleep 1
done
cbody="$(http_body "http://127.0.0.1:$COMPOSE_PORT/healthz")"
say "  compose service health: ${health:-<none>}"
say "  /healthz (compose, :$COMPOSE_PORT) HTTP $ccode body=$cbody"
( cd "$REPO_ROOT" && docker compose -f docker-compose.yml -f "$WORK/override.yml" down -v ) >/dev/null 2>&1
if [ "$code" = "200" ] && [ "$health" = "healthy" ] && [ "$ccode" = "200" ]; then
  PASS "DOCKER-006 (/healthz 200 on mapped port; compose healthcheck reached 'healthy')"
else
  FAIL "DOCKER-006 (docker-run=$code compose-health=${health:-<none>} compose-healthz=$ccode)"
fi
say ""

# ===========================================================================
say "================ summary ================"
say "PASS=$pass FAIL=$fail"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
exit 0
