# PROTOCOL — mimo2api upstream-facing contract archive (contract §7)

Archive of mimo2api's sidecar surface as OxCost consumes it (external
mode, `127.0.0.1:21223`). Sources: mimo2api's own repo (read-only:
`internal/api/server.go`, `request.go`, `chat.go`, `errors.go`,
`stream.go`, `internal/adapter/models.go`, `docs/`) plus live captures
from 2026-10-12 (see EVIDENCE.md / ALIGNMENT.md). Structure and key facts
only — no wholesale copying; no account data; no keys.

## 1. Endpoints (client = OxCost host)

| Endpoint | Method | Auth | Purpose |
|---|---|---|---|
| `/healthz` | GET | none | liveness + account-pool counters |
| `/v1/models` | GET | Bearer | model catalog (account catalog ∪ static floor) |
| `/v1/models/{id}` | GET | Bearer | one model; 404 + available-ids message when unknown |
| `/v1/chat/completions` | POST | Bearer | chat; `stream: true/false` |
| `/v1/audio/speech` | POST | Bearer | TTS |
| `/v1/audio/transcriptions` | POST | Bearer | STT (multipart) |
| `/v1/audio/voices` | GET | Bearer | TTS voice listing |
| `/v1/admin/*` | GET/POST | Bearer | pool/OTP/consent admin (out of plugin scope) |

Routing: Go 1.22 `method+pattern` ServeMux (`server.go` `Handler`).

## 2. Auth

- `Authorization: Bearer <key>`; keys configured via env
  (`MIMO2API_API_KEYS`, comma-separated; `deploy.env`).
- Constant-time comparison (`crypto/subtle`); 401
  `{"error":{"message":"missing or invalid API key","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`
  on failure.
- No keys configured → open (loopback bind is enforced at config level).

## 3. Chat request body (fields the host relies on)

Standard OpenAI chat completion body. Notable handling (`request.go`):

- `model` — resolved through `NormalizeModel`: exact id, `-studio` suffix
  alias, `""`/`auto` → default model; unknown → 404 `model_not_found`.
- `messages` — must be a non-empty array; otherwise 400
  `malformed_request_body`.
- `stream` — `true` → SSE; `false` → JSON.
- `max_tokens`, `temperature`, etc. — relayed per OpenAI semantics.
- **Web search** (`request.go` / `chat.go`): `web_search_options`
  (OpenAI standard spelling; presence ⇒ "search this turn"),
  `web_search_status` (`disabled|auto|enabled`), and the model-id
  `:online` suffix shorthand. Unsupported `search_context_size` values →
  400 with `param: "web_search_options.search_context_size"`.

## 4. Chat response bodies

**Non-stream** — OpenAI chat completion shape (live capture 2026-10-12):

```json
{
  "id": "chatcmpl-…",
  "object": "chat.completion",
  "created": 1791780885,
  "model": "mimo-v2.6-flash",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "…", "refusal": null,
                "reasoning_content": "…"},
    "logprobs": null,
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": …, "completion_tokens": …, "total_tokens": …,
            "prompt_tokens_details": {"cached_tokens": …},
            "completion_tokens_details": {"reasoning_tokens": …}}
}
```

Extensions beyond the OpenAI baseline: `reasoning_content` on the message
and `reasoning_tokens` in usage (reasoning models). Keys OpenAI sends as
null are sent as null (`refusal`, `logprobs`), not omitted.

**Stream** — `Content-Type: text/event-stream; charset=utf-8` (`stream.go`).
Frames: `data: {chunk}\n\n` terminating in `data: [DONE]\n\n`. The chunk
shape is `chat.completion.chunk` with `delta` carrying `role`, `content`,
or `reasoning_content`. Keepalive comments are emitted mid-stream while
the upstream is still generating. Mid-stream upstream failures are
re-emitted in-band as an SSE data frame (an OpenAI-shaped error object
as a final frame before `[DONE]`), not a bare disconnect.

## 5. Models list

`GET /v1/models` → OpenAI list shape with per-model capability metadata
(raw response archived in EVIDENCE.md). Fields: `id`, `object`, `created`
(constant 1700000000), `owned_by` ("xiaomi-mimo"), `name`,
`capabilities{web_search,thinking,audio}`, `page_type`, `tasks`,
`context_size`, `source` ("account" | "fallback"). The catalog is the
union of the per-account Studio catalog with a static floor
(`internal/adapter/models.go` `DefaultModels`, 9 ids); `Usable` filters
what the list exposes. Superseded ids get redirect handling in
`/v1/models/{id}`.

## 6. Error mapping (summary; full cross-check in ALIGNMENT.md)

All error bodies: `{"error":{"message","type","param","code"}}` with
`param`/`code` always present (null when not applicable) —
OpenAI-compatible. From `errors.go`:

| Situation | Status | type | code |
|---|---|---|---|
| missing/invalid key | 401 | invalid_request_error | invalid_api_key |
| malformed body / empty messages | 400 | invalid_request_error | malformed_request_body |
| unknown model | 404 | invalid_request_error | model_not_found |
| upstream 429 | 429 | rate_limit_error | upstream_rate_limited (+Retry-After when known) |
| pool parked/busy | 429 | rate_limit_error | pool_parked / pool_busy |
| no accounts available | 503 | api_error | no_accounts |
| chat deadline | 504 | api_error | chat_timeout |
| upstream 5xx / other upstream-shaped | 502 | api_error | upstream_failure |
| context too long (upstream prose detected) | 400 | invalid_request_error | context_too_long |

Upstream body text is never passed through to clients (logged at debug
level only, truncated) — the §4 "no sentinel passthrough" rule holds in
code.

## 7. Live capture (2026-10-12)

Real request/response pairs against the running deployment; redactions
noted. Chat ids and content abbreviated; no account-identifying fields
were present in any captured response, so nothing else required redaction.

1. **`GET /healthz`** → 200 `{"status":"ok","accounts":{...pool counters...}}`
   (verbatim in EVIDENCE.md §1).
2. **`GET /v1/models`** → 200, 7 models (verbatim in EVIDENCE.md §2).
3. **`POST /v1/chat/completions`** (non-stream, max_tokens 16) → 200, OpenAI
   chat completion with `content:"OK"`, `finish_reason:"stop"` (abbreviated
   in ALIGNMENT.md P5).
4. **`POST /v1/chat/completions`** (stream) → 200 `text/event-stream`, first
   frame `data: {"id":"chatcmpl-…","object":"chat.completion.chunk",...}`
   with `delta.role`, then `reasoning_content`/`content` deltas, ending
   `data: [DONE]`.

## 8. Error-mapping alignment check

See ALIGNMENT.md — one low-severity naming mismatch (contract's
`invalid_request` vs OpenAI's `invalid_request_error` spelling) and one
not-reproduced-live item (quota sentinel path, source-reviewed).

## 9. Rate-limit sentinels

Rate limiting is account-pool-internal (contract §6: the host stays
unaware). The client-visible surface is the 429 table in §6 plus
`Retry-After` headers (seconds) when a local recovery deadline is known
(`writeError`'s retryAfter). Upstream 200-but-sentinel-text cases are
classified in `failure_class.go` (pool failure classes: risk control,
upstream 429, transport, upstream 5xx) and map to the same error table —
no sentinel text escapes.
