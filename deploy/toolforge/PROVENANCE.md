# Provenance

ToolForge — tool-calling middleware — vendored into this repository so the
deployment is self-contained: no clone at build time, no credentials needed to
build, and the change we depend on lives in our tree like the rest of our code.

- Upstream: `https://github.com/Sliverkiss/toolcall` (a fork of
  `YuJunZhiXue/toolforge`)
- Base commit: `758a2c42da409390245674999c7819fbee8b0f00`
  ("SPEC-21: shrink close tag regex to prevent HTML/XML tag false match", 2026-08-19)
- Vendored: 2026-10-09, files: `app/`, `tests/`, `requirements.txt`, `Dockerfile`,
  `LICENSE`, `README.md`, `config.example.yaml`, `pytest.ini`
- Not vendored: the base repo's design notes and tickets (`ANALYSIS-*`, `LOOP-*`,
  `SPEC-*`, `DESIGN-*`, `tickets/`), which stay upstream.

## Our changes

### 1. Caller identity on the upstream hop (8 files, +93/−8)

Function calling needs the *caller's* identity on the hop back into our gateway:
session state is scoped per credential, and the derived identity includes the
model, the system prompt and the tools (spec REQ-3/INV-2, ADR 0012). The base
version authenticates upstream with this middleware's own key, which would put
every caller in one namespace — two callers sending the same first message with
the same tools would derive the same session.

- `app/config.py` — `features.passthrough_headers` (exact names, trailing `*` for
  a prefix). `features.key_passthrough` was already declared in the config schema
  but nothing read it.
- `app/main.py`, `app/adapters/openai_chat.py`, `app/adapters/openai_responses.py`
  — the caller's headers reach the canonical request.
- `app/models/canonical.py` — a dedicated `inbound_headers` field. It is *not* in
  `extra`: `extra` is merged into the upstream request body, and putting headers
  there shipped the caller's credential to the model upstream as JSON (caught
  live while testing this integration).
- `app/engine/wire.py` — defence: a body field named `inbound_headers` can never
  be forwarded upstream as a body field.
- `app/engine/orchestrator.py` — resolves what to forward (`forwarded_headers`)
  and passes it on both the streaming and non-streaming upstream calls.
- `app/upstream/openai.py` — merges the forwarded headers over the configured
  ones, for both calls.

### 2. Single entry point (3 files + 1 new, +215/−6)

`features.catch_all_proxy` makes this service the deployment's only published port:
paths it does not implement (`/v1/models`, `/v1/audio/*`, `/v1/admin/*`) are relayed
to the upstream gateway verbatim — method, path, query, body and headers, including
the caller's own `Authorization` — and `/healthz` reports the gateway's health too.
Without it one address could not serve the whole API: a client would have to point
`tools` calls at one port and everything else at another.

- `app/upstream/proxy.py` (new) — the relay: hop-by-hop headers stripped, body
  forwarded as bytes (`multipart/form-data` uploads and binary audio included),
  response streamed back raw with the upstream's status and headers; an unreachable
  gateway is `502 upstream_error`, never an empty answer. No read timeout: this
  carries `/v1/audio/speech`, whose synthesis budget is minutes.
- `app/config.py` — the `catch_all_proxy` flag. Note the loader builds
  `FeaturesConfig` field by field, so a flag missing from that call is silently off;
  it is listed there first.
- `app/main.py` — the catch-all route registered last (specific routes win), the
  middleware's own `/v1/models` disabled under this flag so the gateway's catalog is
  what clients see, and `/healthz` extended with `upstream_health`.
- `tests/test_catch_all_proxy.py` (new, 6 cases) — verbatim request/response
  relay, binary bodies, upstream error passthrough, unreachable upstream, the flag
  loading from YAML.

Run the suite with `make test-toolforge` (containerised, `live` test excluded).

## Re-syncing with upstream

Fetch the upstream commit you want, diff `app/` against this folder, and re-apply
our change (the eight files above). Then rebuild: `docker compose up -d --build`.

## Tests

`pytest` runs the base suite (`pytest.ini`, `tests/`), with one network test
marked `live`. Our own acceptance is end-to-end instead: `docs/live-findings.md`
(LIVE-TOOLS-001) plus `internal/api/toollayer_test.go`.
