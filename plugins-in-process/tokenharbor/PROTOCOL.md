# TokenHarbor Plugin — Wire Protocol Notes

Working notes for `plugins-in-process/tokenharbor/` — what the plugin
implements, where each wire fact comes from, and the reasoning behind
non-obvious decisions. Evidence pointers reference the recon dossier
(`/root/th-recon/dossier/PROTOCOL.md`, read-only, not committed). No
secrets here: credentials arrive via env vars only.

## 1. Hosts and endpoints

| Purpose | Method + Path | Auth |
|---|---|---|
| Login / refresh | `POST https://auth.tokenharbor.ai/auth/v1/token?grant_type=password\|refresh_token` | `apikey` + `authorization: Bearer <ANON_KEY>` |
| Privacy consent gate | `GET`/`POST /api/me/privacy` | cookie |
| Free-tier entitlements | `GET /api/me/free-tier` | cookie |
| Session create | `POST /api/direct-chat/sessions` | cookie |
| Chat stream | `POST /api/direct-chat/stream` | cookie |
| Session delete (cleanup) | `DELETE /api/direct-chat/sessions/{id}` | cookie |
| Model catalog | `GET /models` | anonymous works |

App endpoints live on `https://tokenharbor.ai` with `origin` header
required; the Supabase auth host is separate. All requests carry the
honest browser UA (Chrome/151 desktop) and pass a 1.5s global pacing
gate (task brief: 1-2s between upstream calls; no quota-burning
retries — none on the stream endpoint, the 401-refresh-retry in chat
is bounded to one attempt).

**No cookie jar by design**: cookies are assembled per account in the
`Cookie` header (the account pool rotates credentials; a shared jar
would cross-contaminate sessions).

## 2. Auth state machine (per account, store-backed)

```
logged-out (no access_token)
  └─ login: POST password grant → persist → cookie
access-valid (expires_at - 60s > now)
  └─ serve with cookie from stored grant
access-expired, refresh_token present
  └─ refresh (single-flight) → persist ROTATED token immediately → cookie
refresh dead (single-use token already consumed)
  └─ login from stored password (recovery path)
quarantined (account_suspended observed)
  └─ skipped: never touched again until un-quarantined by an operator
```

- **Cookie assembly** (`sb-auth-auth-token.0`): value is
  `base64-<URL_SAFE_NO_PAD(JSON)>` of
  `{access_token, token_type, expires_in, expires_at, refresh_token, user}`.
  Only the `.0` cookie is needed (`.1`, `th_sid` not required; bearer
  auth is rejected). Dossier §1, live-verified.
- **Refresh rotation is SINGLE-USE** (dossier top risk #1): each
  refresh consumes the old refresh token server-side. The plugin
  persists the rotated token to SQLite *before* returning from
  `refresh()`, and `save_tokens` is the dedicated writer that never
  clobbers quarantine state. A crash between grant and persist recovers
  via password re-login.
- **Single-flight**: per-account lock (BTreeSet keyed by email); racing
  callers wait, then re-read the store — if the winner already rotated,
  the fresh token is reused (a second refresh with the consumed token
  would kill the session).
- **Supabase anon key**: a public browser constant shipped in the
  site's JS bundle (verified byte-identical against the bundle) —
  committed as `auth::ANON_KEY`. Not a secret.

## 3. Chat flow

```
pick first non-quarantined account (none → 429 rate_limit_error)
ensure_session → cookie
consent gate: if !consent_accepted → POST /api/me/privacy
  {"free_models_enabled": true} (idempotent) → set_consent
POST /api/direct-chat/sessions {"model": <full id>}
  → 201 {"session":{"id": ...}}   (any model string accepted, echoed)
POST /api/direct-chat/stream
  {"sessionId", "content": <flattened prompt>, "model", "webSearch":"off", "tz":"UTC+0000"}
  → SSE stream (chunked delivery; see §6 below)
best-effort DELETE /api/direct-chat/sessions/{id} (spawned, ignored)
```

- **Message flattening**: the upstream takes ONE text prompt, not a
  message array. System message is prefixed; each turn is labeled
  (`User:` / `Assistant:` / `Tool result:`); content parts are
  text-joined (images dropped — no image understanding in v1); empty
  result → 400 invalid_request_error.
- **401 mid-flow** → refresh + retry the failed call once (bounded).
- **Privacy gate** is real and default-off (dossier §2); the POST is
  idempotent, done once per account, persisted in the store.
- **Session lifetime**: one session per request, deleted after
  (best-effort) — the plugin is a stateless translator, not a
  conversation mirror.

## 4. Error mapping (SSE frames with non-200 status)

The central wire fact (dossier §6): upstream errors arrive INSIDE the
SSE body with a non-200 HTTP status — the body must be parsed before
mapping. `event: error` / `data: {"code": "...", "message": "..."}`.

| Upstream code | HTTP (live) | Mapped to | Extra effect |
|---|---|---|---|
| `account_suspended` | 403 | 429 `rate_limit_error` | account quarantined, health → Unhealthy |
| `model_unavailable` | 403 | 404 `invalid_request_error` / `model_not_found` | — |
| `session_unknown`, `missing_session` | 404/400 | 502 `upstream_error` | — |
| unknown code | — | 502 `upstream_error` | code kept in message as `th_<code>` |

Per contract §4, no upstream sentinel text leaks into client-facing
messages (asserted in tests).

## 5. Model resolution (dossier risk #2: never hardcode free ids)

Two sources, layered — the free table churns weekly:

1. **`/models` RSC snapshot** (anonymous, cached with 5-min TTL):
   scanned for `{"surface"...}` objects (tolerant of both capture
   shapes: line-per-row and all-rows-on-one-line). Free rows carry
   `isFree: true`, `:free`-suffixed `surface`, `freeUntil` (promo
   expiry ISO-8601; null = permanent), and a page-level `serverNow`.
2. **`/api/me/free-tier` `model_ids`** (per account, authoritative):
   surface ids WITHOUT `:free` suffix. A model not in this set will
   not stream free.

Serving view = public free rows (window-valid per `serverNow`)
∩ entitlements (union across non-quarantined accounts; base-surface
match). With no tier fetched yet, the public free table stands in —
advertise-and-let-upstream-decide beats hiding everything. Full ids
follow `{family}/{surface}`, except deepseek free ids which ride the
`vercel-ai-gateway/deepseek/` sub-path.

**Manifest is a registration-time declaration** (SDK contract,
validated once; the host bakes it into its catalog at startup), so
`TokenHarbor::connect` refreshes the catalog before building the
manifest. Post-registration churn is absorbed at request time via
`unknown_model = Pass` + honest upstream `model_unavailable` errors.

## 6. M3 open question #1 — chunked streaming: answered

**Decision: buffered (whole-body) SSE, not chunked.** The plugin reads
the upstream stream to completion, then hands the host a single
`ChatOutcome::Sse(Vec<u8>)` buffer. Rationale:

1. `ChatOutcome::Sse` is already a buffer type by M3 design — the host
   forwards it as one response body, so chunked relaying would require
   a new `ChatOutcome` variant (an SDK contract change).
2. The upstream is 1.5s-paced, low-concurrency by design (a web chat,
   not an API) — first-token latency is dominated by upstream pacing,
   not buffering.
3. `reqwest`'s `bytes().await` on the stream response already yields
   chunked transport internally; the buffer is assembled as chunks
   arrive, and one pacing slot covers the whole exchange.

**Known cost**: the client sees nothing until the upstream finishes
(long generations delay the first visible token). If first-token
latency matters later, the fix is a streaming `ChatOutcome` variant in
the SDK contract — deliberately deferred (v1 keeps the SDK surface
minimal). The non-stream path reuses the same buffered frames and
assembles one `chat.completion` JSON.

## 7. Account store (contract §3: plugin-private)

SQLite at `TH_STORE_PATH` (default `data/tokenharbor.db`), WAL mode,
sync access behind a std `Mutex` — adequate for a paced
low-concurrency upstream. Tables: `accounts` (email, password, tokens,
consent, quarantine), `catalog` (single-row snapshot + `serverNow`),
`free_tier` (per-account entitlement set). Re-seeding from env on host
restart never wipes live tokens (`upsert_account` keeps them on
conflict).

**Quarantine is terminal in v1**: `account_suspended` marks the
account; subsequent requests skip it (429 when none left); health
flips Unhealthy. Recovery = operator clears the row / re-seeds.
Never crash the plugin on account failure — mark, skip, surface.

## 8. Env var contract

| Var | Meaning |
|---|---|
| `TH_ACCOUNT_1_EMAIL` / `TH_ACCOUNT_1_PASSWORD` | TEST account credentials (seeds the store; registration requires both) |
| `TH_ACCOUNT_2_EMAIL`/`_PASSWORD` ... up to `TH_ACCOUNT_10_*` | additional accounts |
| `TH_STORE_PATH` | SQLite store path (default `data/tokenharbor.db`) |
| `TH_LIVE_E2E=1` | enables the live end-to-end test (also needs the account vars; run with `-- --ignored`) |

Never committed: any `TH_ACCOUNT_*` value, tokens, cookies. The anon
key is public (see §2).
