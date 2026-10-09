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

# 2. one key, one address (ADR 0012). The key lives in an untracked env file —
#    data/ is gitignored, and the mirror scan fails on a key inside the repo.
printf 'MIMO2API_TOOL_KEY=%s\n' "your-key" > data/deploy.env

# The runtime is distroless `nonroot` (uid 65532): hand it the accounts file
# without making it world-readable. Skipping this is the classic first-boot
# failure — `open /data/accounts.json: permission denied`, restart loop.
chown 65532:65532 data data/accounts.json && chmod 750 data && chmod 600 data/accounts.json

# 3. run — publishes host 21223 on the LOOPBACK ONLY → the tool layer, which serves
#    /v1/chat/completions with function calling and proxies every other path to the
#    gateway verbatim. TLS and the public hostname are the reverse proxy's job
#    (Caddy terminates :443 and proxies to 127.0.0.1:21223), so this port never
#    needs to be reachable from outside — an exposed API port is an exposed key.
docker compose --env-file data/deploy.env up -d --build

# 4. use — same key and same base_url for chat, audio and the admin surface
curl http://localhost:21223/v1/chat/completions \
  -H "Authorization: Bearer your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mimo-v2.6-pro",
    "stream": true,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

If the very first `up` prints `dependency failed to start: container mimo2api
is unhealthy`, run `up -d` once more: the gateway's health window (30 s
`start_period`) can outlast the dependency wait on a cold start. The gateway's
own 8080 is deliberately **not** published — the compose network is its only
route — and 21223 is bound to `127.0.0.1`, so the only public path in is the
reverse proxy. Verify with `ss -ltn | grep 21223` (want `127.0.0.1`) and
`curl --max-time 5 http://<public-ip>:21223/healthz` (want a refusal).

The image is a multi-stage build: `golang:1.25-alpine` compile
(`CGO_ENABLED=0`, static) into `distroless/static-debian12:nonroot` (~10 MB).
No shell, no curl — the binary's own `mimo2api healthcheck` subcommand
performs `GET /healthz` for the container healthcheck.

## Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/v1/chat/completions` | Bearer | OpenAI chat completions (streaming + non-streaming). Extra fields: `web_search_status` (`disabled`\|`auto`\|`enabled`) and `include_citations`; the model suffix `:online` means `web_search_status: enabled` |
| POST | `/v1/raw/chat/completions` | Bearer | Internal (ADR 0012): the same chat handler, minus the tool layer — the path the middleware calls back on. Never forwarded onward; tools are reported as dropped here |
| GET | `/v1/models` | Bearer | Model catalog: every account's live Studio catalog (`bot/config`) unioned per id with the static floor as fallback, **minus the ids the upstream has superseded** (`redirectTo`) — those it refuses outright on the audio surface and answers with the busy frame on chat. Rows carry `capabilities{web_search,thinking,audio}`, `page_type`, `tasks`, `context_size` and `source` (`account`/`static`). Dynamic: no model name is hardcoded, so a retirement needs no deploy (ADR 0011) |
| GET | `/v1/models/{id}` | Bearer | One model entry. A superseded id returns its successor's entry under the id you asked for, with `redirect_to` naming the model that will actually serve it (ADR 0011) |
| POST | `/v1/audio/speech` | Bearer | Text-to-speech: `{input, voice?, response_format?, model?, instructions?, speed?}` → audio bytes (`audio/wav` or `audio/mpeg`) |
| POST | `/v1/audio/transcriptions` | Bearer | Speech-to-text (multipart: `file`, `model?`, `language?`, `response_format?`) → `{text}` / plain text / `verbose_json` |
| GET | `/v1/audio/voices` | Bearer | The account's TTS voice catalog (`{data:[{id,name,description,avatar,preview_audio}]}`) |
| GET | `/healthz` | none | Liveness + pool state summary (no credentials) |
| GET | `/v1/admin/pool` | Bearer | The pool as an operator sees it: state, in-flight, EWMA latency, cooldown end **and its cause**, disabled reason, per-class failure counters, last error, egress label, credential age, plus the recorded risk-control events and per-egress signal counts. Masked ids; no credential material ever (SDD §4.9) |
| GET | `/v1/admin/otp/pending` | Bearer | In-flight new-device OTP challenges (id, method, masked notify, account, expiry) |
| POST | `/v1/admin/otp/submit` | Bearer | Complete a login with `{id, code}` — persists the session, re-enables the account |
| POST | `/v1/admin/consent/agree` | Bearer | Agree to the cross-region transfer demand for one account (`{account_id}`) and continue its login — the operator override for `MIMO2API_CROSS_REGION_AGREE=false` (ADR 0001) |

Models: `mimo-v2.6-pro`, `mimo-v2.6-pro-ultraspeed-studio` (fastchat
channel), plus alias resolution (`mimo`, `ultraspeed`, missing model →
configured default). `/v1/models` reports what this deployment can actually
serve, read from the account's own Studio catalog (`bot/config`
`modelConfigListNg`, `pageType: chat` rows) with the static list as the floor —
on the live test account that is 15 ids, including `clawm-alpha`/`clawl-alpha`
and the omni variants. Reasoning streams when `enable_thinking: true` or
`reasoning: {"type": "enabled"}` (reasoning deltas carry
`reasoning_content`).

Audio: `input` is required; `response_format` is `wav` (default) or `mp3` —
the upstream produces no other container. `voice` takes a Studio voice id/name
(`冰糖`, `茉莉`, `Chloe`, … from `/v1/audio/voices`); the OpenAI voice names
(`alloy`, `nova`, …) have no Studio equivalent, so they resolve to the account's
default voice (first catalog entry, or `MIMO2API_TTS_VOICE`) and any other
unknown name is a `400 unknown_voice` listing the alternatives. `speed` is
accepted and ignored (the upstream has no rate control). Transcriptions accept
files up to 25 MB (gateway-side limit), `language` is passed through
(upstream accepts `auto|zh|en`), and `verbose_json` reports only what the
upstream actually returns (`text`, plus `language`) — no invented durations.
**Long text**: the upstream refuses any single synthesis over 1024 characters,
and that is the contract — `input` over the limit is refused with a 400
(`input_too_long`) before any upstream work, so the client learns immediately
instead of waiting out a doomed synthesis. Splitting longer text and rejoining
the audio was implemented and removed: measured live, a ~1020-rune piece makes
the upstream fail the task itself after ~278 s (ADR 0010). Segment long passages
client-side; `MIMO2API_SPEECH_MAX_CHARS` can lower the cap but not raise it. Note
that 1024 is a hard limit rather than a dependable length — reliability falls off
well before it (384 characters: 17.8 s; 700: no answer in 220 s), so keep
requests in the low hundreds of characters.
Synthesis costs roughly real time upstream (live timings: 128 characters ≈ 13 s,
384 ≈ 18 s), so a long passage takes minutes; the per-piece deadline is
`MIMO2API_AUDIO_TTS_TIMEOUT_S` plus 100 ms per character, capped by
`MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S`, and `MIMO2API_SPEECH_MAX_CHARS` bounds the
total.

**Web search**: with `web_search_status: enabled` (or the `:online` suffix) the
upstream searches and returns results inline. The gateway surfaces them as
OpenAI `annotations` on the message plus a top-level `citations` array; on a
stream they arrive as one extra chunk with `choices: []` before the finish
chunk. The page bodies the upstream sends (tens of KB each) are deliberately not
forwarded. `include_citations: false` keeps the search but drops the payload.
Citations are best-effort: the upstream also answers with `(citation:N)` markers
when it returns no result set at all.

**Sessions (streaming only)**: a streaming request continues an upstream
conversation when it can prove it owns one. The gateway resolves the session
from `X-Mimo-Session-Id` (header) or `session_id` (body field), else derives one
from the model, system preamble, tools and first message — scoped to your API
key. Every streaming response returns `X-Mimo-Session-Id` (pin it next turn to
own the session) and `X-Mimo-Session-Mode` (`chain`, `replay` or `fork`).
Reuse requires the stored frontier to verify against your history, so a
rewritten history replays rather than chaining onto a lookalike, and a second
concurrent request for a busy session waits `MIMO2API_SESSION_WAIT_MS` and then
forks onto its own session instead of interleaving. `stream: false` is
one-shot by design: no session, no store read or write, no header (ADR 0009).

Upstream task kinds are cached per account, so a steady stream of audio requests
does not create a conversation row per request.

## Published mirror

`.gitattributes` marks the test suite `export-ignore`, so `make push-mirror`
publishes the code and the docs as one fresh commit and never the local history.
The working repository — tests included — stays with the operators; `go build ./...`
works in the mirror without the test-only packages.

Real account identifiers (phone, user id, password, tokens) are never written into
the repository: they live in the git-ignored `data/accounts.json`, and `make hygiene`
reads its values from there and fails if any of them appears in a tracked file.

## Function calling (ADR 0012)

MiMo Studio has no native function calling, so `tools` is served by a middleware that
is the deployment's **single entry point**: it strips `tools`, injects an XYML prompt
protocol, parses the call back out of the model's text, and calls this gateway's
ordinary chat surface. Everything it does not implement is relayed to us verbatim:

```
client :21223 --(Bearer KEY)--> toolforge:8080
                              ├── /v1/chat/completions → tools handled here, then
                              │   the model call runs on the gateway's own path
                              └── anything else (/v1/models, /v1/audio/*, /v1/admin/*)
                                  → relayed unchanged, same key
```

- **One address, one key.** Clients keep a single `base_url` (published host port
  `21223`; the container's own 8080 stays on the compose network) and a single
  key; audio, models and admin behave exactly as if the gateway were called
  directly, because they are — the relay passes method, query, body (multipart
  uploads, binary audio) and the caller's headers through, and `/healthz` there
  reports the gateway's health too.
- **One key, both hops.** `MIMO2API_TOOL_KEY` is the key the client sends, the
  middleware's `allowed_keys` entry, and an entry in this gateway's
  `MIMO2API_API_KEYS`. Our copy of the middleware passes the caller's own
  `Authorization` and `X-Mimo-*` headers through to us (`key_passthrough`,
  `passthrough_headers`) so the model call stays in the caller's session namespace.
- **The gateway knows nothing about it.** There is no forwarding code, no internal
  path and no recursion to guard: by the time a chat request reaches us the body has
  no `tools` in it. Reaching the gateway directly (`8080`, on the compose network, or
  published if you uncomment the mapping) gives today's behaviour for tool-bearing
  requests: `tools` reported as dropped in `X-Mimo-Warnings`.
- **The middleware never runs tools.** The client executes them and sends the result
  back as a `role:"tool"` turn, which the middleware folds into the prompt protocol.
- **The middleware is vendored** (`deploy/toolforge/`, see its `PROVENANCE.md`) and
  built from this repo: no clone, no GitHub access, no credentials at build time.
  `docker compose up -d --build` with `MIMO2API_TOOL_KEY` set starts both; without
  it the middleware is inert and nothing changes.

Live (real account, real model, real containers, one published port): `get_weather(city)`
through `:8081` came back as `finish_reason: tool_calls` with `{"city":"上海"}` and the
result fed back produced the final answer; `/v1/models` returned the gateway's real
7-model catalog; `/v1/audio/speech` returned real WAV bytes and `/v1/audio/transcriptions`
a real transcript — both through the relay, multipart upload included. Details in
`docs/live-findings.md` (LIVE-TOOLS-001).

## Superseded models (ADR 0011)

The upstream retires model ids and says so in its own catalog: the row keeps a
`redirectTo` naming the id the platform serves instead (for the current account, 18
of 25 rows, including the chat models `mimo-v2-flash`, `mimo-v2-pro`, `mimo-v2.5`
and the TTS/VoiceDesign/VoiceClone v2.1 family). It then refuses the retired id —
`400 model: [x] not support` on the audio surface, the busy frame on chat.

- `/v1/models` lists only what the upstream will run (7 models for the current
  account).
- A request naming a retired id is resolved to its successor **before** it leaves,
  transitively (`mimo-v2-flash-studio` → `mimo-v2-flash` → `mimo-v2.6-flash`), and
  the response still echoes the id you asked for.
- If the upstream retires a model between two refreshes, the refusal itself
  rebuilds the map and the request is retried once on the successor.
- Cadence: the list follows the catalog TTL (`MIMO2API_CATALOG_TTL_S`, default
  600s), the resolution map the janitor tick (`MIMO2API_PROBE_INTERVAL_S`, default
  60s). `MIMO2API_LOG_LEVEL=debug` logs every substitution.

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
| `MIMO2API_SESSION_REFRESH_HOURS` | `12` | Background STS keepalive sweep (ADR 0014: it refreshes the session cookie, never the credential) |
| `MIMO2API_STICKY_TTL_S` | `1800` | Session idle TTL |
| `MIMO2API_STICKY_MAX` | `500` | Session LRU cap |
| `MIMO2API_SESSION_WAIT_MS` | `2000` | How long a second streaming request waits for its own session before forking |
| `MIMO2API_SESSION_FORK_POLICY` | `wait` | `wait` (then fork) or `fork` (never wait) when a session is already streaming |
| `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT` | `2` | Per-account concurrency semaphore |
| `MIMO2API_QUEUE_WAIT_S` | `15` | Acquire queue wait bound |
| `MIMO2API_KEEPALIVE_INTERVAL_MS` | `15000` | SSE keepalive comment interval |
| `MIMO2API_REPLAY_CAP` | `20` | Max replayed user turns per request |
| `MIMO2API_PROBE_INTERVAL_S` | `60` | Cooldown recovery probe sweep |
| `MIMO2API_EGRESS_<LABEL>` | — | Endpoint for an egress **label** (`hk1` → `MIMO2API_EGRESS_HK1`, dashes become underscores). Accounts name labels in `accounts.json` (`"egress": "hk1"`) and the endpoint plus any credentials live only here. A label with no endpoint is a startup error naming it — never a silent direct connection. `http`, `https`, `socks5` and `socks5h` are accepted |
| `MIMO2API_COOLDOWN_TRANSPORT_S` | `15` | Base cooldown for the transport class (connection error, timeout, SSE stall, EOF mid-body) |
| `MIMO2API_COOLDOWN_TRANSPORT_CAP_S` | `300` | Cap for the transport ladder — a dead connection is less likely to be the account's fault, so it escalates less far than 5xx/429 |
| `MIMO2API_POOL_STATS_WINDOW_S` | `300` | Window inside which risk-control signals are correlated across accounts behind one egress |
| Cookie / credential lifetimes (measured 2026-10-09) | — | `passToken` 30 d, `cUserId`/`userId` 30 d, `deviceId` 390 d, `pass_ua` 89 d, `uLocale` 15 d; renewal **echoes** the passToken, only a full login rotates it (docs/live-findings.md DEVICE-004) |
| Keepalive / self-healing (ADR 0014) | — | Every 12 h (`MIMO2API_SESSION_REFRESH_HOURS`) the session cookie is re-minted from `userID + passToken`; a **revoked credential** schedules an asynchronous single-flight password re-login that rotates the passToken and puts the account back in rotation. A request that discovers the revocation answers `account_action` ("retry in a few seconds") instead of blocking; a login that lands on a human gate persists the device identity, disables the account with the reason and dispatches nothing. `/v1/admin/pool` reports `recovery{in_flight,attempts,last_error}` |
| `MIMO2API_OTP_AUTO` | `false` | Allow a login to send a **real SMS** for the new-device OTP challenge (complete via `/v1/admin/otp/submit`). Off by default: the dispatch is an explicit operator decision (ADR 0013) |
| `MIMO2API_CROSS_REGION_AGREE` | `true` | Agree to the cross-region personal-data-transfer demand on login (non-mainland egress IPs). `false` refuses automatically and leaves the account for an operator, who can still approve one login with `POST /v1/admin/consent/agree` (ADR 0001) |
| `MIMO2API_X_TIMEZONE` | `Asia/Shanghai` | Upstream `x-timeZone` header |
| `MIMO2API_USER_AGENT` | *(Chrome UA)* | Upstream User-Agent |
| `MIMO2API_SEC_CH_UA` | *(matching Chrome)* | Upstream `sec-ch-ua` header |
| `MIMO2API_AUDIO_POLL_MS` | `1000` | TTS/ASR task status poll interval (the Studio's own cadence is 1s) |
| `MIMO2API_COOLDOWN_TRANSPORT_S` | `15` | Base cooldown for the `transport` class (connection error, timeout, SSE stall, EOF mid-body) — its own ladder and counter, so a network blip no longer escalates an account's 5xx cooldown (SDD §4.5.1) |
| `MIMO2API_POOL_STATS_WINDOW_S` | `300` | Window for correlating risk-control (451/461) signals across accounts on one egress (SDD §4.5.2) |
| `MIMO2API_EGRESS_<LABEL>` | — | Endpoint (with credentials) for an egress label referenced by `accounts.json`. Only the label lives in the file; an unknown label fails startup (SDD §4.8) |
| `MIMO2API_AUDIO_TTS_TIMEOUT_S` | `60` | **Base** synthesis deadline; the effective budget grows with the spoken text plus 500 ms per character: measured live at 12.8 s for 128 characters, 17.8 s for 384 and >220 s for 700, so the budget is deliberately generous |
| `MIMO2API_AUDIO_ASR_TIMEOUT_S` | `120` | Transcription task deadline |
| `MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S` | `900` | Cap on the synthesis budget |
| `MIMO2API_MAX_QUERY_CHARS` | `0` | Operator override for the chat character guardrail (ADR 0015). `0` = per-channel defaults: **50,000** characters for ultraspeed/fastchat (the Studio's own `ultraspeedQueryLimitLength: 5e4`, and the platform refuses more) and a **150,000**-character guardrail elsewhere. The other channel's real ceiling is content-dependent (~100k chars measured: 100,100 served, 105,000 refused), so it is not guessed locally — an upstream refusal is translated into `400 context_too_long` with an actionable message instead |
| `MIMO2API_SPEECH_MAX_CHARS` | `1024` | `/v1/audio/speech` input cap, clamped to the upstream's **1024 character** per-synthesis limit ("播报内容长度不能超过1024"). Lower it to refuse earlier; a higher value cannot take effect (ADR 0010) |
| `MIMO2API_TTS_VOICE` | *(empty)* | Default voice id/name; empty = the account catalog's first voice |
| `MIMO2API_CATALOG_TTL_S` | `600` | How long an account's Studio catalog (`bot/config`) is cached for `/v1/models` |
| `MIMO2API_LOG_LEVEL` | `info` | `info` or `debug`. `debug` adds a per-request routing trace (session decision with the resolved id/source/frontier, the conversation/dialog id and delta size, or a full replay with the replay cap), plus the upstream event names of each turn (which is how the empty-citations bug was found) |

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
- **Two chat channels.** `mimo-v2.6-pro-ultraspeed-studio` (the ultraspeed /
  "极速" model) is routed to `/fastchat/open-apis/bot/chat`; every other model
  goes to `/open-apis/bot/chat`. Sending the ultraspeed model to the normal
  channel is rejected upstream with `模型名称错误` (live-verified), so the
  routing rule is not cosmetic. Its per-request cap is 50,000 chars
  (`ultraspeedQueryLimitLength: 5e4` in the Studio) versus 90,000 otherwise.
  The channel is stateful and keeps more than the three rounds the Studio's own
  UI feeds for ultraspeed (`ultraspeedContextRoundLimit`), so the gateway keeps
  server-side chaining plus its own replay budget instead of copying that
  truncation — see `docs/web-surface-reverse.md` §11 and LIVE-011.
- **Error mapping** follows the OpenAI error shape with `type`/`code`
  (`pool_parked` 429 + `Retry-After`, `upstream_auth_expired` 401,
  `upstream_failure` 502, `content_filter` 400, `context_too_long` 400, …).
  Mid-stream upstream failures terminate honestly: an error-text delta,
  `finish_reason: "stop"`, `[DONE]` — never a hang, never restarted deltas.
- **Client disconnects** propagate upstream immediately; sticky context
  advances when a dialogId was already received.

## Documentation map

| Document | What it holds |
|---|---|
| `docs/SDD.md` | Design: protocol chain, pool/session/sticky model, error mapping, deployment |
| `docs/TDD.md` | Test design: §2 fixtures, §3 case-by-case IDs (API/RENEW/STICKY/ERRMAP/LOGIN/DOCKER), §6 gates |
| `docs/login-chain-reverse.md` | The live-verified login chain, our code vs the real traffic, open questions |
| `docs/usercross-reverse.md` | Cross-region consent page reverse engineering (with corrections) |
| `docs/live-findings.md` | Per-conclusion live evidence (LIVE-001..010, LIVE-SESS-001/002, LIVE-TTS-003) |
| `docs/web-surface-reverse.md` | Live-verified web-surface protocol: TTS/ASR/search/claw + chat-vs-ultraspeed channels |
| `docs/specs/session-identity.md` | Spec (accepted): how a request resolves to a session — identity ladder, isolation invariants, fork policy, one-shot non-streaming |
| `docs/specs/account-pool.md` | Spec (proposed): pool failure attribution, the form of 451/461, per-account observability, per-account egress |
| `deploy/toolforge/` | ToolForge (tool-calling middleware), vendored: its source + our change, `PROVENANCE.md`, and the stack's `config.yaml` |
| `docs/decisions/` | ADRs: consent policy, renew-before-login, landing-chain session, no auto-solving human gates, fixtures, acceptance gates, per-field search parsing, long-speech split (superseded by 0010: the cap is refused, not split), session identity, speech as a refused limit, superseded models resolved and not advertised |

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
