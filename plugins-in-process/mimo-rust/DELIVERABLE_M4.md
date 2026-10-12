# M4 Deliverable Summary — mimo-rust (mimo2api Go port as an in-process OxcostPlugin)

Branch `plugin-mimo-rust`, head `579b68b`. All four gates green
(`cargo build`, `cargo clippy -- -D warnings`, `cargo fmt --check`,
`cargo test`): **233 tests, 0 failed**; every file <800 lines; no
secrets committed (credentials env-only).

## 1. De-risk experiment results (R1/R2/R3)

| Experiment | Result | Numbers |
|---|---|---|
| R1 — pool state machine staged port + TDD vector replay | PASS | 31 pool tests replaying 31 Go TDD vector rows; caught one real bug (band round-robin indexed `accounts[band_position]` instead of `accounts[band[pick]]`) |
| R2 — TLS three-client replay (reqwest+rustls vs aistudio frontend) | PASS | 401 + `loginUrl` redirect byte-identical in shape to Go's client across runs; round-trips ~585–639 ms (network variance, not dialect); no TLS/JA3-shaped fingerprinting gate |
| R3 — live SSE transcript fixture + parser | PASS | 15 frames, 3224 bytes, LF-only; role → 12×reasoning → content → finish → usage (both `prompt_tokens_details` and `completion_tokens_details`) → `[DONE]`; parser equality held |

## 2. Trait impl map (`MimoPlugin` → `OxcostPlugin`)

| Trait method | Implementation |
|---|---|
| `manifest()` | `SdkManifest::new("dev.oxcost.mimo-rust")`, version = crate version, 3 models prefixed `mimo-rust/`, `CapabilitySet::from_caps([Chat])`, `UnknownModelPolicy::Pass`, `enabled(true)` |
| `models()` | `ModelDescriptor::owned(m, "mimo-rust")` from the manifest's model list (registry consistency holds) |
| `chat()` | `Box::pin(async)` over `ChatPipeline::handle`: `ChatAnswer::Completion` → `ChatOutcome::Json`, `ChatAnswer::Sse` → `ChatOutcome::Sse`, `MappedError` → `PluginError` (field-for-field) |
| `health()` | `Unhealthy` iff the store holds no account rows; otherwise `Ok` (a full-but-cooling pool still answers 429/503 cleanly) |

Construction: `MimoPlugin::new(Config)` opens the SQLite store, seeds
accounts from `MIMO_ACCOUNT_n_*` env (Go deployment names), builds the
pool and login client. The host strips the `mimo-rust/` prefix before
the body reaches `chat()` (contract §5).

## 3. Pool state machine notes vs Go

- Structure ported as-is: bands by EWMA quality, strict round-robin
  within band, cursor pre-increments from 0 (first band pick is index
  1), cooldown expiry alone is not selectable (probe gate), band
  threshold `best*1.15+1`.
- **Single-account retry after exclusion**: Go's `tryAcquireLocked`
  skips excluded ids before the enabled count, so an excluded sole
  account → `ErrNoAccounts` → **503 `no_accounts`**, not 429
  `pool_parked`. Verified against `pool.go` source; tests assert it.
- Cooldown ladders class-separated (429 / 5xx / transport; separate
  caps), escalation time-owned, reaper force-releases silent leases at
  slot TTL (0 disables).

## 4. SSE parser coverage

`sse.rs` (18 tests) against the R3 fixture dialect: colon-no-space
fields, multi-line data joins, `event:NAME\ndata:{...}` framing,
in-band error classification, usage last-wins, web_search tolerance.
Content-filter frames arrive as `event:sensitive_query` /
`event:sensitive_title` (typed events, not error prose); busy is
Chinese prose in error-event content ("繁忙"/"稍后再试").

## 5. Parity test matrix

- **Classification (§7.1 rows)** — `chat_tests.rs`, 21 tests via
  wiremock: happy path, stream sequence, risk-control 451/461,
  upstream 429, model rejected, content filter (sensitive_query),
  busy-retries-once, busy-twice, empty-retry, upstream 500 → 503
  `no_accounts` (single account), transport failure → 503, auth 401
  refresh-fails, malformed/unknown-model/too-long/no-user-message
  pre-pool rejections (0 upstream calls), multi-turn dialogId
  chaining, thinking-off strips reasoning, model echo, two-account
  fail-over, creds-without-session.
- **Error mapping** — `errmap_tests.rs`, 23 tests: every Go `MapError`
  row → §4 status/type/code/param/message; chat_timeout naming
  (MIMO2API_LEASE_TTL_S vs MIMO2API_REQUEST_TIMEOUT_MS, `div_ceil`
  seconds).
- **Shapers** — `openai.rs` 13 tests: R3 fixture equality (frame
  order role → reasoning → content → finish → citations → usage →
  `[DONE]`).
- **Upstream client** — 17 tests: payloads, channel routing
  (fastchat/ultraspeed vs normal), watchdogs (idle stall, silence
  window), error taxonomy, 401→auth-expired, 5xx keeps Retry-After.
- **Plugin surface** — `plugin_tests.rs`, 8 tests: manifest
  validation, models/manifest consistency, Json + Sse delivery, 429
  error relay, 404 with zero upstream calls, health empty vs seeded,
  empty-pool 503.
- **Pool** — 31 vector-replay tests; **login chain** — 54 tests
  (auth 2, chain 8, otp 17, xssi 10, tests 17 incl. mock-passport);
  **turnplan** 18; **store** 8; **config** 4; **models** 7;
  **thinking** 11.
- **Live parity harness**: behind `MIMO_LIVE_E2E=1`, the M2 six-step
  protocol (models deep-equal, non-stream JSON diff, stream frame
  diff, over-cap, unknown-model 404, bad-key 401) against the
  read-only oracle at 127.0.0.1:21223 — not in gates.

## 6. Env var contract (PORT_DESIGN §6.2)

`MIMO_RUST_*` = gateway knobs; account credentials stay
`MIMO_ACCOUNT_n_*` (Go deployment names, env-only, never committed):

- `MIMO_RUST_DB_PATH` (default `mimo-rust.db`),
  `MIMO_RUST_UPSTREAM_BASE` (default aistudio),
  `MIMO_RUST_DEFAULT_MODEL` (default `mimo-v2.6-pro`),
  `MIMO_RUST_THINKING_DEFAULT_ON` (default true)
- Browser identity: `MIMO_RUST_USER_AGENT`, `MIMO_RUST_SEC_CH_UA`,
  `MIMO_RUST_X_TIMEZONE`
- Login behavior: `MIMO_RUST_OTP_AUTO` (default false),
  `MIMO_RUST_CROSS_REGION_AGREE` (default true)
- Pool: `MIMO_RUST_MAX_INFLIGHT_PER_ACCOUNT` (2),
  `MIMO_RUST_QUEUE_WAIT_S` (15), `MIMO_RUST_COOLDOWN_429_S` (60),
  `MIMO_RUST_COOLDOWN_5XX_S` (30), `MIMO_RUST_COOLDOWN_TRANSPORT_S`
  (15), `MIMO_RUST_COOLDOWN_CAP_S` (600),
  `MIMO_RUST_COOLDOWN_TRANSPORT_CAP_S` (300),
  `MIMO_RUST_LEASE_TTL_S` (120; 0 disables the reaper)
- `MIMO_RUST_SSE_IDLE_TIMEOUT_S` (180)
- Egress: `MIMO_RUST_EGRESS_LABELS` declares labels; each proxy URL
  read from `MIMO_RUST_EGRESS_<LABEL>` (uppercased label)

## 7. Deviations from dossier (justified)

1. **Generic upstream 5xx → 502 `upstream_error`** (Go: 500) — pinned
   dossier §5.3 deviation, carried from task #5: the §4 vocabulary
   has no upstream-caused 500 row.
2. **Silence-window deadline → 504 `chat_timeout`** — NOT a
   deviation: aligns with Go's requestTimeoutError (PARITY §3.19,
   ADR 0036/0019); fixed as `488c5e9` before the pipeline landed.
3. **No deviations beyond the pinned §5.3 row.** Pool semantics,
   retry ladder, error table, and SSE dialect are port-parity,
   verified row-by-row against Go source.

## 8. M3 open questions — answers

1. **Streaming variant**: `ChatOutcome::Sse(Vec<u8>)` is a buffer by
   M3 design — the plugin accumulates the whole upstream stream and
   hands the host one byte buffer. Same decision as tokenharbor's
   PROTOCOL.md §6 (chunked relaying would require a new ChatOutcome
   variant). Consequence: first-token latency through the host equals
   upstream completion time — acceptable v1; a streaming variant is a
   contract change, not a plugin change.
2. **Retry-After on PluginError**: the SDK's `PluginError` has no
   header field (status/err_type/message/param/code only) and the
   host relay (forward.rs) maps it verbatim onto the §4 body,
   ignoring headers. Where Go attaches Retry-After (429/pool_parked),
   the seconds ride in the mapped message/code. No contract change
   made — documented here as the answer.

## Out of scope (per task §3)

tts/asr/voiceclone implementation, image input, registration
automation (operator-driven SMS), LB beyond Go's own algorithm.
