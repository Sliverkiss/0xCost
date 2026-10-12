# TASK M2 — mimo2api as the first OxCost plugin (external mode)

> Executor: Claude Code CLI, invoked by the maintainer's orchestrator (Hermes).
> Working directory: `/root/oxcost` (local-only git repo; NEVER push to any
> remote). Model: `dashscope/glm-5.3` via `--model`.
>
> **Read these first, in order**:
> 1. `CLAUDE.md` (repo root)
> 2. `docs/PLUGIN_CONTRACT.md` — THE authoritative spec (v0.1, 11 sections).
>    Every requirement below traces to it.
> 3. `TASK_M1.md` — context for the M1 skeleton you are extending (already
>    merged to `main`: 4 crates, 52 tests green).
>
> **Language**: think and write in English. All code comments, commit messages,
> docs, and your final summary MUST be English.

---

## 1. Mission

Ship the first real plugin integration: **mimo2api** (an existing Go gateway
for Xiaomi MiMo Studio, already deployed and healthy at `127.0.0.1:21223`)
joins OxCost as an **external-mode plugin** — zero changes to mimo2api's
business code. Deliverables: its manifest, host-side contract-layer
extensions required to serve an external plugin over real HTTP (M1 stopped at
a registry stub), an alignment report, and a verified end-to-end request
through the host.

Hard boundary: mimo2api lives in its OWN repo (`/root/mimo2api`, branch
`mimo2api` in this repo holds a snapshot). Do NOT rewrite it, do NOT vendor
its code into OxCost, do NOT modify anything under `/root/mimo2api`. The only
artifact touching mimo2api semantics is documentation + its manifest.

## 2. Context facts (verified, rely on them)

- mimo2api endpoint: `http://127.0.0.1:21223` — `GET /healthz` returns
  `{"status":"ok","accounts":{...}}` (verified live, container up 42h).
- Endpoints (from its source): `GET /v1/models`, `GET /v1/models/{id}`,
  `POST /v1/chat/completions`, `POST /v1/audio/speech`,
  `POST /v1/audio/transcriptions`. Web-search capability is embedded in chat
  (contract §4 `web_search` semantics).
- Auth: gateway key lives in `/root/mimo2api/data/deploy.env` as
  `MIMO2API_API_KEYS=...` (untracked file; NEVER read, copy, or commit its
  contents — the test harness reads it at runtime via env).
- Its model ids and capability matrix are discoverable via
  `GET /v1/models` at runtime — do NOT hardcode guesses into the manifest;
  derive the `models` list from the live endpoint and record evidence.

## 3. Deliverables

### A. `plugins/mimo2api/plugin.toml` (the manifest)

Per contract §3, mode `external`:
- `id = "dev.oxcost.mimo2api"`, `name = "mimo2api"`, `apiVersion = "v1"`,
  `manifestVersion = "0.1"`, `license = "AGPL-3.0"` (its license).
- `[entry] mode = "external"`, `port = 21223`.
- `[health] path = "/healthz"`.
- `[routing] models = [...]` — from the LIVE `/v1/models` output (record the
  raw response + timestamp in `plugins/mimo2api/EVIDENCE.md`), prefix rule:
  `mimo2api/<model>`; `unknown_model` — decide `pass` vs `reject` from
  observed behavior, justify the choice in EVIDENCE.md.
- `[capabilities]` — set `chat = true`; set `audio_speech` /
  `audio_transcription` / `web_search` per what `/v1/models` capability data
  actually reports; anything unverified stays unset.
- `[auth] keys = "env:MIMO2API_API_KEYS"` (env reference only).
- `[source] repository = "..."` pointing at the maintainer's mimo2api repo.

### B. Host runtime extension: serve external plugins over HTTP (the M1→M2 gap)

M1 built registry + lifecycle + manifest parsing only (axum declared but
unused). Extend `crates/` so the host actually proxies:

1. **`oxcost-core`**: `PluginInstance` runtime view (resolved endpoint URL,
   resolved auth key source, capability set, state handle). Keep pure.
2. **`oxcost-contract`**: resolve `env:` auth references at load time via an
   injected `Fn(&str) -> Option<String>` env-resolver (no direct
   `std::env` in the crate — testability). Missing env var = registration
   error with a clear message.
3. **`oxcost` (bin)**: axum server implementing contract §5 external surface:
   - `POST /v1/chat/completions` — resolve model → route to the owning
     plugin → forward (headers: `Authorization: Bearer <plugin key>`)
     → stream SSE back byte-faithfully for `stream:true`; JSON pass-through
     for `stream:false`. Timeout: 300s per request.
   - `GET /v1/models` — aggregate from healthy registered plugins
     (`routing.enabled` respected; Disabled excluded).
   - `GET /healthz` — host health + per-plugin state table.
   - Host auth: single key from env `OXCOST_API_KEYS` (comma-separated,
     Bearer). If unset → bind loopback, allow all (dev mode), log a warning.
   - Config: extend CLI with `--listen <addr>` (default `127.0.0.1:8080`).
   - Errors follow contract §4/§5 mapping (`404 model_not_found`, `502
     upstream_error` on connection failure, `400 invalid_request` on bad
     body) with OpenAI error bodies.
   - Load balancing / circuit breaker: OUT OF SCOPE for M2 (single plugin).
     Structure the forwarding path so LB and the state machine's
     CircuitOpen/HalfOpen hooks can slot in at M3 without rework (document
     the seams in code comments). Probing: on startup, health-check each
     plugin and move Registered→Probing→Serving (or record failure); a
     plugin not in Serving is excluded from routes/models.

### C. `plugins/mimo2api/PROTOCOL.md` (contract archive, §7)

Archive mimo2api's upstream-facing contract per contract §7: endpoints,
methods, headers, request/response bodies, auth, rate-limit sentinels — with
evidence. You cannot capture new upstream traffic (no browser); instead:
- Cite mimo2api's own docs: `/root/mimo2api/docs/` (read-only reference; copy
  structure and key facts, NOT file contents wholesale) and its
  `internal/` source (from the `mimo2api` branch in THIS repo — read-only).
- Include a live-capture section: real request/response pairs you generate
  NOW against `127.0.0.1:21223` (models list, one non-stream chat call with
  max_tokens small, the /healthz body). Redact any account-specific data;
  note redactions explicitly.
- Document the error-mapping alignment check (deliverable D).

### D. Alignment report: `plugins/mimo2api/ALIGNMENT.md`

Check mimo2api's actual error responses against contract §4 semantics
(429/502/400 shapes, OpenAI error body, no sentinel passthrough). Verify
empirically: send it a malformed request (empty messages → expect 400-class),
an unknown model (observe behavior), and record actual status codes + bodies.
If mismatches exist, do NOT fix mimo2api — list each mismatch with observed
vs expected; the maintainer will schedule the fix in mimo2api's own repo.

### E. Tests & quality gates (all MUST pass before you report done)

```
cargo build --workspace
cargo clippy --workspace --all-targets -- -D warnings
cargo fmt --check
cargo test  --workspace          # existing 52 + new tests, all offline-green
```

New tests (offline; mock upstream with a local axum/hyper test server or
wiremock — no real network in unit/integration tests):
- env-resolver: present/missing/failing refs (~4)
- forwarding: model routing to plugin URL, Bearer key injection, stream
  pass-through (SSE bytes identical), JSON pass-through, upstream connection
  refused → 502 body, unknown model → 404 body, host auth 401 (~10)
- aggregation: /v1/models merges across plugins, Disabled excluded (~3)
- CLI: --listen parse, --plugins scan already covered in M1 (extend if
  behavior changed) (~2)

### F. Live end-to-end verification (the money shot)

Script `scripts/e2e-mimo2api.sh` (bash, set -euo pipefail):
1. source the plugin key from `/root/mimo2api/data/deploy.env` into env
2. start the host with `--plugins plugins --listen 127.0.0.1:8080`
3. assert `GET /healthz` shows mimo2api `Serving`
4. assert `GET /v1/models` contains at least one `mimo2api/` model
5. one real `POST /v1/chat/completions` (stream:false, max_tokens 64) through
   the host → assert OpenAI-shaped response
6. one real streaming call (stream:true) → assert first SSE chunk arrives
   and frames end with `[DONE]`
7. negative: unknown model through host → 404 body; bad host key → 401
8. teardown: kill host
Run it, capture output to `plugins/mimo2api/E2E_RUN.md`, and quote the tail
in your final summary. THIS is the acceptance artifact — a green script run
is worth more than any description.

## 4. Working rules

1. Atomic commits, one concern each, Conventional Commits, English.
2. Branch `m2-mimo2api-plugin`; keep `main` clean.
3. NEVER commit secrets: no keys from deploy.env, no account data. The live
   E2E script reads the key from the file at runtime. EVIDENCE.md must show
   the /v1/models response with any account-identifying fields redacted
   (redact, do not truncate structure).
4. NEVER push to any remote.
5. Do not modify `docs/PLUGIN_CONTRACT.md`, `CLAUDE.md`, `/root/mimo2api/**`.
   Contract gaps go in your final summary.
6. No turns limit — run until gates in E pass and F is green.
7. Final summary (English): what shipped, file inventory, gate outputs
   (paste four command results), the live E2E tail, alignment findings,
   contract observations (explicit "none" if none).

## 5. Acceptance (maintainer verifies independently)

- Re-run E gates fresh on branch `m2-mimo2api-plugin`.
- Re-run F script personally; check secrecy (no key material in git).
- Manifest ↔ live /v1/models consistency; alignment report honesty.
