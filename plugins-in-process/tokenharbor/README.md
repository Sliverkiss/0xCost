# oxcost-plugin-tokenharbor

TokenHarbor: the first **real** OxcostPlugin (contract §10 host-sdk form)
— tokenharbor.ai web chat behind the OpenAI chat-completions protocol,
compiled into the host process. Wire-protocol details and design
rationale: [PROTOCOL.md](PROTOCOL.md).

## What it does

- OpenAI `/v1/chat/completions` (stream and non-stream) against
  tokenharbor.ai's web chat, mapped to/from its SSE frame protocol.
- Supabase password login, single-use refresh-token rotation
  (persisted immediately — see PROTOCOL.md §2), cookie-only app auth.
- Model resolution from the live `/models` catalog + per-account
  free-tier entitlements. **No hardcoded model ids** — the free table
  churns weekly, so every id is resolved at runtime.
- Account pool with quarantine: suspended accounts are marked, skipped,
  and surface in `/healthz` — never crash the plugin.
- Paced (1.5s), browser-shaped HTTP; no quota-burning retries.

## Layout

```
src/
  lib.rs     plugin assembly, manifest, models() view, seeding
  auth.rs    login / refresh rotation / cookie assembly
  client.rs  paced HTTP client (browser headers, no cookie jar)
  models.rs  /models RSC parsing + free-tier policy
  chat.rs    consent gate → session → stream → OpenAI mapping + §4 errors
  sse.rs     upstream SSE frame parser
  store.rs   plugin-private SQLite (accounts, catalog, free-tier)
fixtures/    RSC + SSE captures from the recon dossier (no secrets)
tests/
  mock_upstream.rs  offline integration suite (axum mock upstream)
  live_e2e.rs       live E2E — #[ignore]d, opt-in, never in gates
```

## Seeding accounts (TEST accounts only)

```bash
export TH_ACCOUNT_1_EMAIL=…   # a TEST account you control
export TH_ACCOUNT_1_PASSWORD=…
export TH_STORE_PATH=/var/lib/oxcost/tokenharbor.db   # optional
```

The host registers the plugin when `TH_ACCOUNT_1_EMAIL` +
`TH_ACCOUNT_1_PASSWORD` are set (up to `TH_ACCOUNT_10_*`). Credentials
never appear in code, commits, or logs — only in the SQLite store and
env. The recon account is quarantined and must not be seeded. A
suspended/quarantined account never fails host startup; it shows up as
`Unhealthy` in `/healthz` and requests return 429 until an account
recovers (operator clears the quarantine or re-seeds).

## Running tests

```bash
cargo test -p oxcost-plugin-tokenharbor        # offline: 44 unit + 9 integration
cargo clippy -p oxcost-plugin-tokenharbor --all-targets -- -D warnings
cargo fmt -p oxcost-plugin-tokenharbor -- --check
```

Live E2E (opt-in, manual, **never part of gates**):

```bash
TH_LIVE_E2E=1 TH_ACCOUNT_1_EMAIL=… TH_ACCOUNT_1_PASSWORD=… \
  cargo test -p oxcost-plugin-tokenharbor --test live_e2e -- --ignored
```

## Env var contract

| Var | Meaning |
|---|---|
| `TH_ACCOUNT_N_EMAIL` / `TH_ACCOUNT_N_PASSWORD` (N=1..10) | TEST account credentials |
| `TH_STORE_PATH` | SQLite store path (default `data/tokenharbor.db`) |
| `TH_LIVE_E2E` | must be `1` for the live E2E test to run |

## Known limitations (v1)

- **Chat only**: no tool calls, no image understanding (image parts are
  dropped during message flattening). `web_search` is reserved and
  default-unsupported (`webSearch: "off"` is always sent).
- **Buffered streaming**: the whole upstream stream is read before the
  client sees a byte (M3 open question #1 — see PROTOCOL.md §6 for the
  full reasoning; first-token latency is dominated by upstream pacing
  anyway).
- **Single account serves each request** (first non-quarantined); no
  multi-account load balancing yet.
- **Quarantine is terminal** until an operator intervenes.
- **One flat prompt** per request: multi-turn history is flattened into
  a single labeled transcript; tool-result roles are labeled but have
  no upstream semantics.
- **Catalog refresh is startup + manual** (`refresh_catalog` /
  `connect`): no background refresher yet; the manifest is a
  registration-time declaration, and post-registration churn is
  absorbed by `unknown_model = Pass` + honest upstream errors.
