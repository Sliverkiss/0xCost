# mimo2api

OpenAI-compatible API gateway for Xiaomi MiMo AI Studio. Go + stdlib only,
single static binary, Docker delivery. Point any OpenAI SDK / client at it
and it translates requests into MiMo web-chat sessions across an account
pool with stickiness, transparent session renewal, and rate-limit-aware
routing.

## Quick start (Docker)

```bash
# 1. accounts file (see format below)
mkdir -p data
$EDITOR data/accounts.json   # mode 0600; never commit it

# 2. run
MIMO2API_API_KEYS=sk-your-key docker compose up -d

# 3. use
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mimo-v2.6-pro",
    "stream": true,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

The image is a multi-stage build: `golang:1.25-alpine` compile
(`CGO_ENABLED=0`, static) into `distroless/static-debian12:nonroot` (~10 MB).
No shell, no curl — the binary's own `mimo2api healthcheck` subcommand
performs `GET /healthz` for the container healthcheck.

## Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/v1/chat/completions` | Bearer | OpenAI chat completions (streaming + non-streaming) |
| GET | `/v1/models` | Bearer | Model catalog |
| GET | `/v1/models/{id}` | Bearer | One model entry |
| GET | `/healthz` | none | Liveness + pool state summary (no credentials) |
| GET | `/v1/admin/otp/pending` | Bearer | In-flight new-device OTP challenges (id, method, masked notify, account, expiry) |
| POST | `/v1/admin/otp/submit` | Bearer | Complete a login with `{id, code}` — persists the session, re-enables the account |

Models: `mimo-v2.6-pro`, `mimo-v2.6-pro-ultraspeed-studio` (fastchat
channel), plus alias resolution (`mimo`, `ultraspeed`, missing model →
configured default). Reasoning streams when `enable_thinking: true` or
`reasoning: {"type": "enabled"}` (reasoning deltas carry
`reasoning_content`).

## Configuration (environment)

All variables are `MIMO2API_`-prefixed. Defaults:

| Variable | Default | Description |
|---|---|---|
| `MIMO2API_HOST` | `0.0.0.0` | Bind host; empty `API_KEYS` forces `127.0.0.1` |
| `MIMO2API_PORT` | `8080` | Listen port |
| `MIMO2API_API_KEYS` | *(empty)* | Comma-separated Bearer keys; empty = open auth + bind-local |
| `MIMO2API_UPSTREAM_BASE` | `https://aistudio.xiaomimimo.com` | MiMo AI Studio base URL |
| `MIMO2API_ACCOUNTS_FILE` | `/data/accounts.json` | Accounts + persisted runtime state |
| `MIMO2API_DEFAULT_MODEL` | `mimo-v2.6-pro` | Model for unknown/missing ids |
| `MIMO2API_REQUEST_TIMEOUT_MS` | `300000` | Upstream request timeout |
| `MIMO2API_SSE_IDLE_TIMEOUT_S` | `180` | No-SSE-event watchdog (stream stall abort) |
| `MIMO2API_SESSION_REFRESH_HOURS` | `6` | Background STS renewal sweep |
| `MIMO2API_STICKY_TTL_S` | `1800` | Sticky-session idle TTL |
| `MIMO2API_STICKY_MAX` | `500` | Sticky-session LRU cap |
| `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT` | `2` | Per-account concurrency semaphore |
| `MIMO2API_QUEUE_WAIT_S` | `15` | Acquire queue wait bound |
| `MIMO2API_KEEPALIVE_INTERVAL_MS` | `15000` | SSE keepalive comment interval |
| `MIMO2API_REPLAY_CAP` | `20` | Max replayed user turns per request |
| `MIMO2API_PROBE_INTERVAL_S` | `60` | Cooldown recovery probe sweep |
| `MIMO2API_OTP_AUTO` | `true` | Auto-start the new-device OTP challenge on login demands (complete via `/v1/admin/otp/submit`) |
| `MIMO2API_CROSS_REGION_AGREE` | `true` | Agree to the cross-region personal-data-transfer demand on login (non-mainland egress IPs). `false` disables the account instead of agreeing |
| `MIMO2API_X_TIMEZONE` | `Asia/Shanghai` | Upstream `x-timeZone` header |
| `MIMO2API_USER_AGENT` | *(Chrome UA)* | Upstream User-Agent |
| `MIMO2API_SEC_CH_UA` | *(matching Chrome)* | Upstream `sec-ch-ua` header |
| `MIMO2API_LOG_LEVEL` | `info` | Log level |

Security: with no API keys configured the server binds `127.0.0.1` only —
open auth never reaches the network (SDD §5.1).

## Accounts file format

JSON, one flat array, mode `0600`, git-ignored. Login credentials are
needed once; runtime state is persisted back automatically (sessions,
cooldowns, disabled reasons survive restarts):

```json
{
  "accounts": [
    {
      "id": "acc-1",
      "enabled": true,
      "phone": "+8613800138000",
      "password": "your-password",
      "session": null
    }
  ]
}
```

- `id` — stable identifier used in logs and healthz (never the phone).
- `session` — after the first successful login the gateway fills in
  `cookie`, `ph`, `fetched_at` (plus `user_id`/`pass_token` on the account)
  and keeps them updated via transparent STS renewal. You can pre-seed a
  captured session instead of storing a password.
- Multiple accounts form the pool; selection is score-banded round-robin
  with per-account inflight limits, 429/5xx cooldown ladders, and recovery
  probes.

**Never commit this file.** It contains live credentials.

## Behavior notes

- **Streaming** responses follow the OpenAI SSE shape: role chunk →
  content/reasoning deltas → finish chunk → usage chunk (`choices: []`) →
  `data: [DONE]`, with `: keepalive` comments during long generation gaps.
- **Sticky sessions** (streaming only): a follow-up request whose history
  extends a recent conversation reuses the same account + upstream
  conversation and sends only the new messages. Non-streaming requests
  always start fresh conversations.
- **Error mapping** follows the OpenAI error shape with `type`/`code`
  (`pool_parked` 429 + `Retry-After`, `upstream_auth_expired` 401,
  `upstream_failure` 502, `content_filter` 400, `context_too_long` 400, …).
  Mid-stream upstream failures terminate honestly: an error-text delta,
  `finish_reason: "stop"`, `[DONE]` — never a hang, never restarted deltas.
- **Client disconnects** propagate upstream immediately; sticky context
  advances when a dialogId was already received.

## Development

```bash
make help          # list every target
make build vet fmt # compile, vet, gofmt gate
make test          # go test ./... (LIVE-*/DOCKER-* skips are expected)
make test-race     # required gate: the whole suite under -race
make cover-check   # per-package coverage floors (docs/TDD.md §6.2)
make hygiene       # §6.7: no real credentials anywhere outside data/
make test-docker   # DOCKER-001..006 (skips when docker is absent)
```

Live protocol suite (real upstream, real account, opt-in):

```bash
MIMO2API_LIVE=1 go test ./internal/live -v -count=1 -timeout 30m
```

It runs against a **copy** of the operator's `data/accounts.json`, paces its
upstream calls, and never auto-solves a challenge — a new-device OTP demand
skips unless `MIMO2API_LIVE_OTP_CODE` is supplied. Switches and the last
run's findings: `internal/live/README.md`, `docs/live-findings.md`.

Stdlib only — `go.mod` carries zero dependencies (the docker harness adds a
throwaway Python mock upstream on the *host* side; the image stays distroless).
