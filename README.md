# mimo2api

OpenAI-compatible API gateway for Xiaomi MiMo AI Studio. Go + stdlib only,
single static binary, Docker delivery. Point any OpenAI SDK / client at it
and it translates requests into MiMo chat turns across an account pool with
transparent session renewal and rate-limit-aware routing. Chat is one
non-streaming answer per request: every client message is replayed into a fresh
upstream conversation, and nothing about a conversation is stored (ADR 0017).

## Quick start (Docker)

```bash
# 1. accounts file (see format below)
mkdir -p data
$EDITOR data/accounts.json   # mode 0600; never commit it

# 2. one key, one address (ADR 0016). The key lives in an untracked env file —
#    data/ is gitignored, and the mirror scan fails on a key inside the repo.
printf 'MIMO2API_API_KEYS=%s\n' "your-key" > data/deploy.env

# The runtime is distroless `nonroot` (uid 65532): hand it the accounts file
# without making it world-readable. Skipping this is the classic first-boot
# failure — `open /data/accounts.json: permission denied`, restart loop.
chown 65532:65532 data data/accounts.json && chmod 750 data && chmod 600 data/accounts.json

# 3. run — one container, and it publishes host 21223 on the LOOPBACK ONLY → the
#    gateway itself (container port 3000, ADR 0018). TLS and the public hostname
#    are the reverse proxy's job (Caddy terminates :443 and proxies to
#    127.0.0.1:21223), so this port never needs to be reachable from outside —
#    an exposed API port is an exposed key.
docker compose --env-file data/deploy.env up -d --build

# 4. use — same key and same base_url for chat, audio and the admin surface
curl http://localhost:21223/v1/chat/completions \
  -H "Authorization: Bearer your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mimo-v2.6-pro",
    "stream": false,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

Chat is **non-streaming** and there is no switch: `stream: true` is refused with
`400 stream_unsupported` rather than answered in a shape an SSE client cannot
parse (ADR 0017 — the SSE emission path and the conversation store it existed for
are deleted, not disabled). Set `stream: false`, or use a client that can read one
JSON body.

If the very first `up` prints `dependency failed to start: container mimo2api
is unhealthy`, run `up -d` once more: the health window (30 s `start_period`)
can outlast the dependency wait on a cold start. Port 21223 is bound to
`127.0.0.1`, so the only public path in is the reverse proxy — verify with
`ss -ltn | grep 21223` (want `127.0.0.1`) and
`curl --max-time 5 http://<public-ip>:21223/healthz` (want a refusal).

The image is a multi-stage build: `golang:1.25-alpine` compile
(`CGO_ENABLED=0`, static) into `distroless/static-debian12:nonroot` (~10 MB).
No shell, no curl — the binary's own `mimo2api healthcheck` subcommand
performs `GET /healthz` for the container healthcheck.

## Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/v1/chat/completions` | Bearer | OpenAI chat completions — **always one JSON body** (ADR 0017: no streaming path; `stream: true` → `400 stream_unsupported`, unconditionally, before any account work). `tools` is reported as dropped in `X-Mimo-Warnings`. Extra fields: `web_search_status` (`disabled`\|`auto`\|`enabled`) and `include_citations`; the model suffix `:online` means `web_search_status: enabled` |
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
OpenAI `annotations` on the message plus a top-level `citations` array, in the one
JSON body (search was never affected by the streaming change). The page bodies the
upstream sends (tens of KB each) are deliberately not forwarded. `include_citations: false` keeps the search but drops the payload.
Citations are best-effort: the upstream also answers with `(citation:N)` markers
when it returns no result set at all.

**Conversations (no server-side state)**: the gateway keeps nothing about your
conversation — no session header, no store, no chaining across requests
(ADR 0017, superseding ADR 0009's streaming-only sessions). Send the history you
want the model to see: every request replays it into a **fresh** upstream
conversation, so two byte-identical requests are two independent answers. Inside
one request the replayed turns do chain, which is what makes a multi-turn history
one conversation, and at most `MIMO2API_REPLAY_CAP` user turns are sent. The
practical consequence: to "continue" a conversation, send the earlier messages
again (and mind the 102,400-character ceiling on the composed first query,
ADR 0015).

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

## No function calling (ADR 0016)

MiMo Studio has no native function calling, and the middleware that emulated it is
**no longer deployed** — the owner's call, and the measurement backs it: the
injected XYML prompt spent 1,355 characters for one tool, 1,760 for three and
5,222 for thirty, out of the same 102,400-character query budget the caller's own
text is measured against (ADR 0015). Live proof: 102,000 characters with thirty
tools was served on the raw chat path and refused through the tool layer. On a
channel that short the tools were not worth the context they cost ("上下文太短，
做不了什么事").

```
client :21223 --(Bearer KEY)--> mimo2api:8080     (one container, one key)
```

- **`tools` in a request is not an error**: it is decoded, ignored and reported in
  `X-Mimo-Warnings`. Explicit, never a silent downgrade — a client that wants
  tools runs them itself and sends the results back as ordinary messages.
- **One key, one address.** `MIMO2API_API_KEYS` is the only credential, and the
  published port (21223, loopback only) reaches the gateway directly, so models,
  audio and admin need no relay and no second credential.
- **The middleware source stays in the repository** (`deploy/toolforge/`, with its
  `PROVENANCE.md`), but nothing builds or runs it: `docker compose up -d --build`
  starts one container. Reviving it is a compose service plus an env var, and
  `git log` holds the configuration that worked.
- **It did work when it ran** (live, before removal): `get_weather(city)` came back
  as `finish_reason: tool_calls` with `{"city":"上海"}`, and feeding the result back
  produced the final answer (`docs/live-findings.md`, LIVE-TOOLS-001). It worked,
  it just cost more context than it was worth here.

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

## Deploying to Belmo (no volume, Upstash store)

The binary is a plain Go program with no cgo and no sidecars, and the repository
ships a `nixpacks.toml`, so a Belmo deploy is: connect the repo, set the
environment, deploy.

```
MIMO2API_API_KEYS=<your key>
DS_REDIS_HOST=<your Upstash endpoint>      # bare host, host:port, or a full rediss:// URL
DS_REDIS_TOKEN=<your Upstash token>
MIMO2API_OTP_AUTO=false                    # never spend an SMS automatically (ADR 0013)
```

- **Port**: the app binds `MIMO2API_PORT` if set, else the platform's own `PORT`,
  else **3000** (ADR 0018) — nothing to configure on Belmo.
- **Accounts live in Upstash** (one JSON key per account plus an index set, ADR
  0018): a redeploy keeps the pool, its device identities and its refresh tokens.
  Seed it once from a local file with `mimo2api -accounts import data/accounts.json`
  (run locally against the same `DS_REDIS_HOST`; it verifies the write by reading
  the store back). Day-to-day changes — enable, disable, remove, re-pin an egress
  label — are the same flag with a different subcommand; see
  [Managing accounts](#managing-accounts-file-or-upstash).
- **Or keep the file backend** by leaving `DS_REDIS_HOST` unset: the accounts file
  defaults to `data/accounts.json` (relative, directory created on demand), so an
  ephemeral filesystem costs a re-login per deploy rather than a crash. The
  container deployment on this host does exactly that with `./data` mounted.
- **Health**: `GET /healthz` for the platform's HTTP check, or
  `mimo2api healthcheck` on a host without curl.
- **Root directory must be the repository root.** The Go module, the `cmd/`
  packages and `nixpacks.toml` all live at the top of this repository, so a
  platform configured with a subdirectory (e.g. the `app/` layout a multi-language
  project uses) finds no `go.mod` and never builds. Build command, if the platform
  asks for one instead of reading `nixpacks.toml`:
  `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o mimo2api ./cmd/mimo2api`;
  start command: `./mimo2api`; port: leave it to the platform (the binary listens on
  `$PORT` when `MIMO2API_PORT` is unset, and on 3000 when neither is set).
- **`MIMO2API_API_KEYS` is mandatory here, not optional.** With the platform's own
  `PORT` supplied and no key, startup **fails with an error naming the variable**
  instead of quietly binding `127.0.0.1` — a loopback bind is unreachable behind the
  platform's proxy, which is what makes a misconfigured deploy look like a build that
  never finishes (ADR 0021).
- **If the platform keeps showing its own "starting up" page**, read its runtime log
  and look for one of two lines: the `MIMO2API_API_KEYS is required when the platform
  supplies PORT` error, or `DS_REDIS_HOST is set but the store is unusable — refusing
  to start on a stale file fallback` (a store that cannot be reached is fatal by
  design, ADR 0018). Both are deliberate; a build failure instead means the root
  directory or the build command is wrong.

## Managing accounts (file or Upstash)

`mimo2api -accounts <command>` manages whichever store the environment selects —
`DS_REDIS_HOST` set means Upstash, otherwise `MIMO2API_ACCOUNTS_FILE`. It is the
same binary and the same environment as the server, so there is no file to edit
and no Redis console to open (ADR 0020). Each command reports the backend it
acted on.

```
mimo2api -accounts list                     # id, enabled, egress, credential/session/device state
mimo2api -accounts show <id>                # full record; secrets shown as presence and length
mimo2api -accounts enable <id>
mimo2api -accounts disable <id> [reason]    # e.g. after a passport gate or a 461
mimo2api -accounts set-egress <id> <label>  # pin/clear the network path label (ADR 0012)
mimo2api -accounts rm <id>
mimo2api -accounts import <accounts.json>   # upsert every row into the active store
mimo2api -accounts export <file>            # whole store → file, mode 0600 (a backup that holds credentials)
mimo2api -accounts verify                   # connect, count, report
```

```
$ DS_REDIS_HOST=… DS_REDIS_TOKEN=… mimo2api -accounts list
1 account(s) in redis (<db>.upstash.io:6379)
  <id>  enabled  egress=direct credential=present(347 chars) | session=present(age 4h9m) | device=present
```

- **Where the credentials come from**: the same `DS_REDIS_HOST`/`DS_REDIS_TOKEN`
  the server uses, or `MIMO2API_ACCOUNTS_FILE` for the file backend. With docker
  compose, `--env-file data/deploy.env` plus the `DS_REDIS_*` passthrough in
  `docker-compose.yml` is what reaches the container (a container does not
  inherit your shell).
- **What takes effect when**: reads are immediate. `disable`/`rm`/`import` change
  what the next start loads — the store is read once at boot and the request path
  never calls Redis (ADR 0018) — so restart the container to apply a membership
  change. Runtime state that heals on its own (session renewal, credential
  rotation, cooldowns) already writes through without a restart.
- **Moving between stores**: `-accounts export` a file from one store, then
  `-accounts import` it into the other.
- **Never automated**: starting a re-login over SMS still needs the human gate
  (`MIMO2API_OTP_AUTO=false`, ADR 0013) — including when the account is a row in
  Upstash rather than a line in a file.

## Configuration (environment)

All variables are `MIMO2API_`-prefixed. Defaults:

| Variable | Default | Description |
|---|---|---|
| `MIMO2API_HOST` | `0.0.0.0` | Bind host; empty `API_KEYS` forces `127.0.0.1` |
| `MIMO2API_PORT` | `3000` | Listen port; when unset the platform's own `PORT` is honoured (Belmo/Railway/Heroku), then this default (ADR 0018) |
| `MIMO2API_API_KEYS` | *(empty)* | Comma-separated Bearer keys; empty = open auth + bind-local |
| `MIMO2API_UPSTREAM_BASE` | `https://aistudio.xiaomimimo.com` | MiMo AI Studio base URL |
| `MIMO2API_ACCOUNTS_FILE` | `data/accounts.json` | Accounts + persisted runtime state. Relative by default so a hosted build with no volume can start; the directory is created on demand. Ignored when `DS_REDIS_HOST` is set |
| `DS_REDIS_HOST` | *(empty)* | Upstash/Redis host for the account store (ADR 0018). Set → Redis **is** the account store (a file fallback would silently serve nobody); a bare host or `https://…` URL is assembled with the token, a full `rediss://`/`redis://` string is used as-is. Note the name has no `MIMO2API_` prefix: the reference deployment uses the same pair |
| `DS_REDIS_TOKEN` | *(empty)* | Token for the assembled connection string (required unless `DS_REDIS_HOST` is already a full redis URL) |
| `DS_REDIS_INSECURE` | *(unset)* | Set to `1` to allow a plaintext `redis://` to a **non-loopback** host (local dev only). The store refuses it by default: credentials and session cookies live in Redis |
| `DS_REDIS_CA_FILE` | *(unset)* | PEM bundle of extra root CAs for `rediss://` (a private/self-hosted Redis). Verification is never disabled; an unreadable or non-PEM file fails startup |
| `MIMO2API_DEFAULT_MODEL` | `mimo-v2.6-pro` | Model for unknown/missing ids |
| `MIMO2API_REQUEST_TIMEOUT_MS` | `120000` | The whole client-facing budget for one chat request (**wired in ADR 0019**; until then it was read by tests alone). Exceeded → our own `504 chat_timeout` naming the knob, kept under a CDN's ~100s origin read timeout so the client sees our error rather than a 524 |
| `MIMO2API_LEASE_TTL_S` | `120` | Per-upstream-turn deadline and the in-flight slot's TTL (ADR 0019). A slot past its TTL is force-released by the pool reaper and its upstream call is cancelled |
| `MIMO2API_SLOT_HOLD` | `last-byte` | When a request gives its in-flight slot back: `last-byte` (the slot spans the generation) or `first-byte` (released as soon as the upstream starts answering — more per-account concurrency while generating, an account-risk trade, so it is opt-in) |
| `MIMO2API_SSE_IDLE_TIMEOUT_S` | `180` | Upstream no-SSE-event watchdog: MiMo streams its answer to us, so this aborts a stalled upstream read (it is not a client-facing keepalive) |
| `MIMO2API_SESSION_REFRESH_HOURS` | `12` | Background STS keepalive sweep (ADR 0014: it refreshes the session cookie, never the credential) |
| `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT` | `2` | Per-account concurrency semaphore |
| `MIMO2API_QUEUE_WAIT_S` | `15` | Acquire queue wait bound |
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
| `MIMO2API_MAX_QUERY_CHARS` | `0` | Operator override for the chat character guardrail (ADR 0015). `0` = per-channel defaults: **50,000** characters for ultraspeed/fastchat (49,500 served / 52,000 refused; the Studio's own `ultraspeedQueryLimitLength: 5e4`) and **102,400** for every other model — the platform's measured ceiling for one query text (exactly 102,400 served, 102,415 refused, identical for ASCII and CJK, so it counts characters). The cap counts the **query text the platform receives**, not each message: the system messages the planner splices into the first user query spend the same budget (that is also why the tool layer's injected protocol prompt — 1,355 chars for one tool, 5,222 for thirty — had to go, ADR 0016), and the error says how much of it was preamble. Anything above is refused upstream too, so rejecting locally only saves a wasted call; an upstream refusal is still translated into `400 context_too_long` with an actionable message |
| `MIMO2API_SPEECH_MAX_CHARS` | `1024` | `/v1/audio/speech` input cap, clamped to the upstream's **1024 character** per-synthesis limit ("播报内容长度不能超过1024"). Lower it to refuse earlier; a higher value cannot take effect (ADR 0010) |
| `MIMO2API_TTS_VOICE` | *(empty)* | Default voice id/name; empty = the account catalog's first voice |
| `MIMO2API_CATALOG_TTL_S` | `600` | How long an account's Studio catalog (`bot/config`) is cached for `/v1/models` |
| `MIMO2API_LOG_LEVEL` | `info` | `info` or `debug`. `debug` adds a per-request routing trace (session decision with the resolved id/source/frontier, the conversation/dialog id and delta size, or a full replay with the replay cap), plus the upstream event names of each turn (which is how the empty-citations bug was found) |

Security: with no API keys configured the server binds `127.0.0.1` only —
open auth never reaches the network (SDD §5.1).

## Account storage format

Two interchangeable backends (ADR 0018); the runtime keeps the pool in memory and
writes through, so this is storage, not a request path.

- **File** (default): `MIMO2API_ACCOUNTS_FILE`, JSON, one flat array, mode `0600`,
  git-ignored, atomic write, directory created on demand.
- **Upstash** (`DS_REDIS_HOST`): one JSON key per account
  (`mimo2api:accounts:<id>`, the same record shape) plus an index set
  (`mimo2api:accounts:index`) — no volume, survives a redeploy.

Login credentials are
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
| `docs/TDD.md` | Test design: §2 fixtures (§2.1.3 lists what retired with the streaming path), §3 case-by-case IDs (API/RENEW/ERRMAP/LOGIN/DOCKER), §6 gates |
| `docs/login-chain-reverse.md` | The live-verified login chain, our code vs the real traffic, open questions |
| `docs/usercross-reverse.md` | Cross-region consent page reverse engineering (with corrections) |
| `docs/live-findings.md` | Per-conclusion live evidence (LIVE-001..010, LIVE-SESS-001/002, LIVE-TTS-003) |
| `docs/web-surface-reverse.md` | Live-verified web-surface protocol: TTS/ASR/search/claw + chat-vs-ultraspeed channels |
| `docs/specs/session-identity.md` | Spec (accepted): how a request resolves to a session — identity ladder, isolation invariants, fork policy, one-shot non-streaming |
| `docs/specs/account-pool.md` | Spec (proposed): pool failure attribution, the form of 451/461, per-account observability, per-account egress |
| `deploy/toolforge/` | ToolForge (tool-calling middleware), vendored with its `PROVENANCE.md` — **no longer deployed** (ADR 0016); kept as the working copy that reviving it would start from |
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
