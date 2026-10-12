# EVIDENCE — mimo2api plugin manifest derivation

Live captures taken **2026-10-12** against the running deployment
`http://127.0.0.1:21223` (docker compose, container up ~42h at capture
time). Key material read at runtime from `/root/mimo2api/data/deploy.env`
via the `MIMO2API_API_KEYS` environment variable; **no key values are
recorded here**.

## 1. `GET /healthz` (no auth)

```json
{
  "status": "ok",
  "accounts": {
    "total": 1,
    "available": 1,
    "cooldown": 0,
    "disabled": 0
  }
}
```

200 OK. `accounts` is pool-internal state (contract §6: the host does not
interpret it); captured verbatim as evidence of a healthy probe target.
No account-identifying fields present.

## 2. `GET /v1/models` (Bearer, runtime-sourced key)

Timestamp: 2026-10-12 (local). Raw response, structure preserved — the
deployment's account catalog reported 7 models (source: "account"); no
account-identifying fields were present in the response, so nothing
required redaction beyond the auth header itself:

```json
{
  "object": "list",
  "data": [
    {
      "id": "mimo-v2.5-asr",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.5-ASR",
      "capabilities": { "web_search": 0, "thinking": false, "audio": "asr" },
      "page_type": "asr",
      "tasks": ["asr"],
      "source": "account"
    },
    {
      "id": "mimo-v2.5-tts",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.5-TTS",
      "capabilities": { "web_search": 0, "thinking": false, "audio": "tts" },
      "page_type": "tts",
      "tasks": ["tts"],
      "source": "account"
    },
    {
      "id": "mimo-v2.5-tts-voiceclone",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.5-TTS-VoiceClone",
      "capabilities": { "web_search": 0, "thinking": false, "audio": "tts" },
      "page_type": "tts-vc",
      "tasks": ["tts"],
      "source": "account"
    },
    {
      "id": "mimo-v2.5-tts-voicedesign",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.5-TTS-VoiceDesign",
      "capabilities": { "web_search": 0, "thinking": false, "audio": "tts" },
      "page_type": "tts-vd",
      "tasks": ["tts"],
      "source": "account"
    },
    {
      "id": "mimo-v2.6-flash",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.6-Flash",
      "capabilities": { "web_search": 2, "thinking": true, "audio": "none" },
      "page_type": "chat",
      "tasks": ["chat"],
      "context_size": "1M",
      "source": "account"
    },
    {
      "id": "mimo-v2.6-pro",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.6-Pro",
      "capabilities": { "web_search": 2, "thinking": true, "audio": "none" },
      "page_type": "chat",
      "tasks": ["chat"],
      "context_size": "1M",
      "source": "account"
    },
    {
      "id": "mimo-v2.6-pro-ultraspeed-studio",
      "object": "model",
      "created": 1700000000,
      "owned_by": "xiaomi-mimo",
      "name": "MiMo-V2.6-Pro-Ultraspeed-Studio",
      "capabilities": { "web_search": 2, "thinking": true, "audio": "none" },
      "page_type": "chat",
      "tasks": ["chat"],
      "source": "account"
    }
  ]
}
```

### Manifest derivation from this response

| Live field | Manifest field | Value |
|---|---|---|
| `data[].id` (7 ids) | `routing.models` (with `mimo2api/` prefix per contract §3) | 7 entries, order as listed |
| `capabilities.web_search = 2` on chat models + `chat.go` handling of `web_search_options`/`web_search_status`/`:online` | `capabilities.web_search = true` | set |
| `capabilities.audio = "tts"` (3 TTS models) + `POST /v1/audio/speech` routed in `server.go` | `capabilities.audio_speech = true` | set |
| `capabilities.audio = "asr"` (mimo-v2.5-asr) + `POST /v1/audio/transcriptions` routed in `server.go` | `capabilities.audio_transcription = true` | set |
| — | `capabilities.audio_translations` | unset (no live evidence; mimo2api does not route the endpoint) |
| — | `capabilities.embeddings/images/…` | unset (no live evidence) |

Note: the catalog is dynamic (per-account Studio catalog unioned with a
static floor — see `internal/api/server.go` `handleModels`). The 7 ids
above are the account-sourced live view at capture time; mimo2api's static
`DefaultModels` floor contains 9 ids (`mimo-v2-pro`, `mimo-v2-flash`,
`mimo-v2.5`, `mimo-v2.5-pro`, `mimo-v2.1-pro(-preview)` are floor-only
today). The manifest records the **live** account view per the task
directive ("derive from the live endpoint"); floor-only ids are excluded
until they surface in a live catalog.

## 3. `unknown_model` decision: `reject`

Observed behavior (live probe, 2026-10-12):

```
POST /v1/chat/completions  {"model":"totally-unknown-model", ...}
→ HTTP 404
  {"error":{"message":"model \"totally-unknown-model\" not found; available
   models: …","type":"invalid_request_error","param":null,"code":"model_not_found"}}
```

mimo2api **itself rejects** unknown models (source:
`internal/adapter/models.go` `NormalizeModel` → `ErrModelNotFound`; the
chat path maps it to 404). With `pass`, the host would forward the request
upstream and relay a 404 with a mimo2api-shaped body — no benefit, plus a
double-hop failure and an upstream-dependent error shape. With `reject`,
the host refuses early with its own contract-§4-shaped 404, deterministic
and independent of upstream wording. **`reject` chosen.**

## 4. Capability cross-check against source (read-only)

- Routes (`internal/api/server.go` `Handler`): `GET /healthz`,
  `GET /v1/models`, `GET /v1/models/{id}`, `POST /v1/chat/completions`,
  `POST /v1/audio/speech`, `POST /v1/audio/transcriptions`,
  `GET /v1/audio/voices` (plus admin endpoints, out of plugin scope).
- Auth: Bearer check in `server.go` `auth()` — constant-time compare,
  401 `invalid_api_key` when keys are configured; key injected via env
  (`deploy.env`, `MIMO2API_API_KEYS`).
- Error shape: OpenAI `{"error":{message,type,param,code}}` (see
  ALIGNMENT.md for the §4 cross-check).
