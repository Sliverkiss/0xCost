# E2E_RUN — `scripts/e2e-mimo2api.sh` acceptance run

Run date: **2026-10-12**, repo root `/root/oxcost`, branch
`m2-mimo2api-plugin`, host binary `./target/debug/oxcost` (debug build).
Upstream: live mimo2api deployment at `127.0.0.1:21223`.

The script sources the plugin key at runtime from
`/root/mimo2api/data/deploy.env` (value never printed, never persisted);
the host key is a local test value so the 401 negative case exercises a
*wrong* key. Full captured output follows, verbatim:

```text
$ bash scripts/e2e-mimo2api.sh

=== key sourced (value not shown), host key set ===
OxCost host — plugin registration table
host supports manifestVersion 0.1, apiVersion v1

ID                           VERSION    STATE          MODE                     CAPABILITIES
dev.oxcost.example-remote    0.1.0      Registered     Remote                   chat,web_search
dev.oxcost.mimo2api          1.0.0      Registered     External                 chat,audio_speech,audio_transcription,web_search

=== GET /healthz ===
mimo2api state: Serving
[PASS] healthz shows mimo2api Serving

=== GET /v1/models ===
7 mimo2api models, e.g. ['mimo2api/mimo-v2.6-flash', 'mimo2api/mimo-v2.6-pro', 'mimo2api/mimo-v2.6-pro-ultraspeed-studio']
[PASS] /v1/models contains mimo2api/ models

=== POST /v1/chat/completions (stream:false, max_tokens:64) ===
content: OK
[PASS] non-stream chat through host is OpenAI-shaped

=== POST /v1/chat/completions (stream:true) ===
11 SSE frames; first frame is chat.completion.chunk; ends with [DONE]
[PASS] stream: first chunk arrived, frames end with [DONE]

=== negative: unknown model ===
404 body: model_not_found
[PASS] unknown model → 404 model_not_found

=== negative: bad host key ===
401 body: invalid_api_key
[PASS] bad host key → 401 invalid_api_key

=== teardown ===
[PASS] host stopped

E2E: ALL CHECKS PASSED
```

Checks exercised, in order:

1. Plugin key sourced from `deploy.env` at runtime; host key set to a
   local test value (`OXCOST_API_KEYS`).
2. Host started with `--plugins plugins --listen 127.0.0.1:8080`;
   startup probing transitions mimo2api Registered → Probing → Serving.
3. `GET /healthz` — mimo2api reported **Serving**.
4. `GET /v1/models` — 7 `mimo2api/`-prefixed models present.
5. Real non-stream chat (`max_tokens: 64`) through the host →
   OpenAI-shaped `chat.completion`, assistant content "OK".
6. Real streaming chat → SSE frames, first frame
   `chat.completion.chunk`, stream ends `data: [DONE]`.
7. Negative: unknown model → HTTP 404, body `code: model_not_found`.
8. Negative: bad host key → HTTP 401, body `code: invalid_api_key`.
9. Teardown: host stopped (EXIT trap also covers failure paths).

Exit status: **0** — `E2E: ALL CHECKS PASSED`.
