# simple-chat plugin (in-process, OxCost host-sdk)

`deepseek-flash` web chat (DeepSeek android-app wire), default-on
reasoning + opt-in web search, ported from the Go `simple-chat` gateway
into the host process as an `OxcostPlugin` (contract §10 host-sdk form).

- **Manifest**: `dev.oxcost.simple-chat` · model `simple-chat/deepseek-flash`
  (single, like Go) · capability `chat`.
- **Web search**: opt-in per request (`"search": {"type": "enabled"}`,
  or OpenAI-native `web_search_options` / `web_search_status: "enabled"`);
  citations passthrough on the completion. See OQ-1 below for the
  standalone `/v1/web_search` decision.

## Exclusive ownership (R1 — CRITICAL)

**This plugin assumes it is the ONLY process operating the upstream
accounts.** Two processes sharing one DeepSeek account trip the
per-account parallel-generation limit, race session tokens, and distort
the risk-device heuristics. At cutover:

1. **STOP the Go `simple-chat` binary first** (maintainer action).
2. Then start/reload the host carrying this plugin.

The `sc-seed` CLI prints a concurrency warning when importing (per the
dossier's 6-step order) — it is a reminder, not a guard.

## Environment contract

All optional; unset = default.

| Var | Meaning | Default |
|---|---|---|
| `SC_DB_PATH` | plugin-private SQLite path (accounts + park state, R3) | none → memory-only (tests) |
| `SC_MAX_PROMPT_CHARS` | prompt char cap; `-1` disables | `2000000` |
| `SC_MAX_INFLIGHT` | concurrent requests per account | `2` |
| `SC_QUEUE_WAIT_SECS` | queue wait when all accounts busy | `30` |

Seed-only (test-time, never in gates):

| Var | Meaning |
|---|---|
| `UPSTASH_REDIS_REST_URL` / `UPSTASH_REDIS_REST_TOKEN` | read-only Upstash import (PC-41); GET only, never SET/DEL |
| `SC_LIVE_UPSTASH=1` | opts into a live import (otherwise the CLI refuses) |

No credentials are ever logged; park/token fields never appear in
traces (PC-44).

## Seeding accounts

```
cargo run -p oxcost-plugin-simple-chat --bin sc-seed -- --help
```

Imports Upstash JSON fixtures / live Upstash (read-only) into the
SQLite store the plugin reads at boot. Parked/banned state carries
over; expired parks are stripped at load.

## Cutover checklist (maintainer)

1. `cargo build -p oxcost-plugin-simple-chat` (host wires the plugin).
2. Seed the store: run `sc-seed` against your Upstash export (offline
   fixtures) or live with `SC_LIVE_UPSTASH=1`. Verify the import
   summary (`imported/parked/updated` counts).
3. **Stop the Go gateway** (R1): confirm no process is still holding
   DeepSeek sessions for these accounts.
4. Set `SC_DB_PATH` to the seeded store path; start/reload the host.
5. Verify: `/v1/models` shows `simple-chat/deepseek-flash`; a small
   `chat` round-trip answers with `reasoning_content`; the host's
   health endpoint reports the plugin `ok` (a parked/empty ring shows
   `unhealthy` — probe fails, plugin stays registered but unrouted).
6. Optional live smoke (R5): one streaming + one non-streaming
   completion against production upstream.

## OQ-1 — standalone `/v1/web_search` capability

**Decision: not declared in v1; `web_search` stays reserved-unsupported
on the manifest, search rides `chat`.**

Justification: the SDK trait's `web_search` hook is reserved in M3
(no required method — `plugin.rs` documents the default
"capability not declared" fail-closed body). Declaring the capability
without the hook would advertise an endpoint the host cannot route.
The Go `/v1/web_search` is implementable entirely from existing
pieces (a search-enabled completion + the interpreter's
`search_queries`/`search_results` state), so the follow-up is cheap:
add the SDK hook, flip the manifest entry, port the thin wrapper.
Per-request search (`search` switch → citations) is fully served.

## OQ-2 / M3 open questions

- **#1 streaming variant**: `ChatOutcome::Sse(Vec<u8>)` is
  buffer-based. The Go retry/commit boundary (no client bytes before
  the first delta) is preserved exactly: the buffer exists only when
  the call returns `Ok`. A chunked-stream variant would need a commit
  callback in the trait; deliberately deferred, not worked around.
- **#2 Retry-After on `PluginError`**: the SDK error type has no
  header field. The §6 table's Retry-After values (pool_parked,
  pool_busy, parallel_chat_limit, account_muted) are computed on
  `errmap::WireError` and carried to the facade boundary; the host
  will project them once the SDK grows a header side-channel. Status,
  type, code, and message are complete on `PluginError` today.

## Module map (Go → Rust)

| Go | Rust |
|---|---|
| `accountstore` | `store.rs` (SQLite, R3 latch + generation fencing) |
| `upstream.Account` | `account.rs` |
| `upstream/client.go` (wire) | `wire.rs` + `client.rs` (R2 watchdog) |
| `upstream/pow` | `pow.rs` (HashV1, Keccak rounds 1..23) |
| `upstream` pool/manager | `pool.rs` + `manager.rs` + `lease.rs` |
| `internal/sse` | `sse.rs` |
| `internal/openai` (parsing) | `openai.rs` + `image_fetch.rs` (SSRF guard) |
| `internal/openai/response.go` | `oai_response.rs` |
| `server.go` §6 error writers | `errmap.rs` |
| `server.go` handler/ladder | `chat_flow.rs` |
| `main.go` wiring | `plugin.rs` (`OxcostPlugin` facade) |

Out of scope (v1): session cleanup/purge schedulers, admin API,
async session deleter — the app-like housekeeping tasks from the Go
server's config surface; none affect correctness of the chat path.
