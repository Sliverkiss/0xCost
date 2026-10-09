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

## Our change (8 files, +93/−8)

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

## Re-syncing with upstream

Fetch the upstream commit you want, diff `app/` against this folder, and re-apply
our change (the eight files above). Then rebuild: `docker compose up -d --build`.

## Tests

`pytest` runs the base suite (`pytest.ini`, `tests/`), with one network test
marked `live`. Our own acceptance is end-to-end instead: `docs/live-findings.md`
(LIVE-TOOLS-001) plus `internal/api/toollayer_test.go`.
