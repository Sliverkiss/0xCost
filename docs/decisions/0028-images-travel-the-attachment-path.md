# 0028. Images travel the attachment path — the query budget is not their budget

Date: 2026-10-09
Status: accepted
Related: ADR 0015 (request size limits are measured), ADR 0016 (no tools), ADR 0027
(OpenAI-standard surface), docs/attachments-reverse.md

## Context

The owner's ruling: *"我们的模型应该是支持图片识别的。把这个对齐了安排上吧，兼容面这些
不需要"* — wire image input, and skip the extra compatibility surfaces (Anthropic
Messages, the Responses API) that the sibling projects ship.

The state before this record:

1. `internal/api/request.go` **rejected** `image_url` content parts with
   `400 unsupported_content`. A client sending the single most common OpenAI
   multimodal shape got a refusal.
2. `internal/mimo/attach.go` already implemented the whole upstream chain
   (`resource/genUploadInfo` → signed PUT → `resource/parse`) and
   `cmd/attachprobe` had machine-verified it **for documents** — but nothing in the
   request path called it.
3. Whether the vision models read images at all was unverified. The account's
   `modelConfigListNg` carries no capability flags, so the catalogue could not
   answer it.

## Decision

**An `image_url` part is an attachment: it rides the turn that carried it, and it
never touches the query budget.**

1. `content` parts accept `image_url` — a `data:image/...;base64,…` URL or an
   http(s) URL. Anything else (`file://`, a non-image data URL) is a named 400
   (`unsupported_image_url`), never a silent drop.
2. The bytes are fetched (data URL decoded, or downloaded with the gateway's browser
   identity and a byte cap applied **while reading** — a `Content-Length` is a claim,
   not a fact) and uploaded through the attachment chain, then referenced from **the
   turn whose message carried the image** (`TurnPlan.Media` is index-aligned with
   `TurnPlan.Queries`). An image with a second user message is uploaded once, not
   with the first turn.
3. The `multiMedias` entry carries the **full** field set
   (`mediaType:"image"`, `fileUrl`, `objectName`, `url`, `tokenUsage`, `size`,
   `status`, plus the two empty video/audio fields). This is not guesswork twice
   over: our probe had already shown a stripped item is refused, and the sibling
   reverse-engineered client sends exactly this shape.
4. `resource/parse` is **retried** (3 attempts, 0.5 s / 1 s, context-aware; business
   codes and 5xx only — never 4xx/401). The parse step is the flaky one, and by the
   time it fails the upload bytes are already spent. The sibling project retries it
   five times.
5. Uploads happen **per attempt and are never cached across attempts**: a parse
   result belongs to the session (account) that created it, and a retry runs on a
   different account (ADR 0019's lease).
6. Failures are named 400s — `unsupported_image_url`, `image_fetch_failed`,
   `unsupported_image_type`, `image_too_large`, `too_many_images` — and
   `isContentErr` covers all of them, so **a bad URL never cools an account down**.
7. A user message that carries images and no text is a legal request. The channel
   requires a `query`, so `adapter.DefaultImageOnlyQuery` is sent; that synthesis is
   documented in SDD §5.2 rather than hidden, because the alternative — refusing a
   request OpenAI clients consider legal — is worse.
8. `MIMO2API_MAX_IMAGES` (default 10) and `MIMO2API_MAX_IMAGE_BYTES` (default
   20 MiB) are **guards and are documented as guards, not measurements** (ADR 0015
   covers the measured numbers). 20 MiB is OpenAI's own vision-input limit; 10
   images bounds one request's fan-out, because every image costs three upstream
   calls. The channel advertises 100 MB per file and 50 files per message, and the
   operator can raise both variables to use that.

## Consequences

- An image is **cheap in query budget and expensive in calls**: 3 upstream calls per
  image (upload info, PUT, parse) plus the parse's own `tokenUsage`, which lands in
  the prompt count. Measured: a 5.7 KB marker PNG with a 20-character question →
  `prompt_tokens: 3104`, 9.6 s, answer read back exactly.
- The two budgets stay separate: the 102,400-character query limit still applies to
  the text, and images do not spend it.
- A retry re-uploads the images (accepted: correctness over the extra calls).
- The 20 MiB / 10-image guards can refuse what the channel would have accepted. That
  is deliberate and reversible by environment variable, and it is written down as a
  guard rather than presented as a channel limit.
- Vision quality is the model's, not the gateway's: we forward the bytes and the
  parse result, and nothing in the path inspects the image.

## Evidence

- OpenAI-alignment amendment: `image_url` is an object in OpenAI's schema, and its
  second field, `detail` (`auto`/`low`/`high`), was the one part of the standard
  shape that this work initially ignored. It is now **validated** (a value outside
  the set is a 400 `unsupported_image_detail` before any account work) and, because
  the channel's parse call has no level parameter (verified shape; the sibling client
  sends none either), **reported as dropped** in `X-Mimo-Warnings`. A knob that looks
  applied but is not would be worse than one that announces itself (IMG-009/010).
- Tests: `internal/api/image_test.go` IMG-001..010 (the mock upstream already serves
  the resource surface; the tests assert the recorded `multiMedias`, the per-turn
  attachment, the retry count, the byte-for-byte upload, and the named 400s).
- `cmd/attachprobe` had already machine-verified the document half of this chain
  (ADR 0015).
- Live, against the real channel through the running gateway, with a PNG whose only
  content is the unguessable string `ZQ-7788`:
  - `image_url` data URL + "图里的编号是什么？只回答编号本身" → **`ZQ-7788`** (200,
    9.6 s, `prompt_tokens: 3104`);
  - the same PNG with **no text** → a full description of the image (the synthesised
    neutral query works);
  - the same PNG with `reasoning_effort: "low"` → `ZQ-7788` with
    `reasoning_content` present (images and thinking compose);
  - `https://example.invalid/nope.png` → **400 `image_fetch_failed`** naming the
    fetch failure.
- Gate: `make fmt vet hygiene test-race cover-check` — exit 0.
