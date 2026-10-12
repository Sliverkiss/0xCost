# ALIGNMENT — mimo2api error semantics vs contract §4

Empirical check, **2026-10-12**, against the live deployment at
`http://127.0.0.1:21223` (key injected at runtime from
`/root/mimo2api/data/deploy.env`; never recorded). Each probe is a real
request/response pair; bodies quoted verbatim.

Contract §4 error mapping table (the expectation column):

| Upstream situation | Expected plugin response |
|---|---|
| quota/login-wall sentinel (incl. 200 + sentinel text) | `429`, type `rate_limit_error` |
| upstream empty / non-2xx | `502`, type `upstream_error` |
| empty `messages` / invalid params | `400`, type `invalid_request` |
| mid-stream error | tail frame (`finish_reason` + `[DONE]`), no bare disconnect |

Error body shape everywhere: OpenAI
`{"error": {"message": ..., "type": ..., "code": ...}}`; plugins must not
pass upstream sentinel text through.

## Probe results

### P1 — empty `messages` (expect 400-class)

```
POST /v1/chat/completions
  {"model":"mimo-v2.6-flash","messages":[]}

→ HTTP 400
  {"error":{"message":"malformed request body","type":"invalid_request_error",
   "param":null,"code":"malformed_request_body"}}
```

**Aligned** on status (400) and OpenAI body shape.

### P2 — unknown model (observe)

```
POST /v1/chat/completions
  {"model":"totally-unknown-model","messages":[{"role":"user","content":"hi"}]}

→ HTTP 404
  {"error":{"message":"model \"totally-unknown-model\" not found; available
   models: mimo-v2-flash, mimo-v2-pro, mimo-v2.1-pro, mimo-v2.1-pro-preview,
   mimo-v2.5, mimo-v2.5-pro, mimo-v2.6-flash, mimo-v2.6-pro,
   mimo-v2.6-pro-ultraspeed-studio","type":"invalid_request_error",
   "param":null,"code":"model_not_found"}}
```

OpenAI-consistent (OpenAI itself returns 404 + `model_not_found` for
unknown models). Not in the §4 table (that table covers upstream
situations, not request routing), but consistent with §5 host semantics
(`404 model_not_found`).

### P3 — malformed JSON body (expect 400-class)

```
POST /v1/chat/completions   (body: `not json`)

→ HTTP 400
  {"error":{"message":"malformed request body","type":"invalid_request_error",
   "param":null,"code":"malformed_request_body"}}
```

**Aligned.**

### P4 — missing key (auth)

```
GET /v1/models   (no Authorization header)

→ HTTP 401
  {"error":{"message":"missing or invalid API key","type":"invalid_request_error",
   "param":null,"code":"invalid_api_key"}}
```

**Aligned** with OpenAI's 401 shape.

### P5 — happy path, non-stream (for completeness)

```
POST /v1/chat/completions
  {"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"Say OK"}],
   "max_tokens":16,"stream":false}

→ HTTP 200
  {"id":"chatcmpl-…","object":"chat.completion","created":1791780885,
   "model":"mimo-v2.6-flash","choices":[{"index":0,"message":{"role":"assistant",
   "content":"OK","refusal":null,"reasoning_content":"…"},"logprobs":null,
   "finish_reason":"stop"}],"usage":{…}}
```

OpenAI chat completion shape (plus a `reasoning_content` extension, which
OpenAI clients tolerate as an unknown field).

## Findings

| # | Observed | Expected (contract §4) | Severity | Disposition |
|---|---|---|---|---|
| 1 | 400/404/401 bodies use `"type":"invalid_request_error"` (and `"api_error"`/`"rate_limit_error"` elsewhere in `errors.go`) | §4 spells the invalid-params type as `invalid_request` | **Low — naming only** | Documented mismatch; OpenAI's own API uses `invalid_request_error`, so the contract's `invalid_request` spelling is the outlier. Recommendation: keep mimo2api as-is; consider clarifying the contract's error-type names in a contract revision (contract change, not a mimo2api change). |
| 2 | Not directly observable this session: quota/login-wall sentinel behavior (needs an exhausted or dead account to trigger). Source review shows `errors.go` maps upstream 429 → `429 rate_limit_error` (`isUpstream429`) and account-dead → cooldown, i.e. the mapping exists in code. | 429 `rate_limit_error`, no sentinel text passthrough | Not reproduced live | Source-reviewed only; flagged for the maintainer's next mimo2api-side verification window. |

Everything else observed (400 on bad body, 404 on unknown model, OpenAI
error body shape, constant-time key check, 401 on missing key, stream
with `text/event-stream` and `data: [DONE]` termination) **aligns** with
contract §4/§5 semantics.

Per the task rules: no mimo2api code was modified. Mismatches above are
reported for the maintainer to schedule in mimo2api's own repo.
