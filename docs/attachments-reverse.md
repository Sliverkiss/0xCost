# Attachments: the one path that lifts the query ceiling

Status: **verified end to end against the live platform** (2026-10-09, `cmd/attachprobe`).
Everything below was read out of the live Studio bundle and the live
`GET /open-apis/bot/config`, then exercised with real credentials: a document
larger than the query ceiling is uploaded, parsed, attached, and read by the
model.

## Why it matters

The chat channel counts the query text the platform receives and refuses above
**102,400 characters** (ADR 0015). Attachments are accounted for separately:
the parse call answers with a `tokenUsage` per file, and the catalog advertises
file limits in megabytes.

## Surface (from the live bundle, `main.911869f9.chunk.js`)

Upload:

1. `POST /open-apis/resource/genUploadInfo` body `{fileName, fileContentMd5?}` →
   `{uploadUrl, resourceUrl, objectName}` (the SPA calls it with the MD5 of the
   bytes; our audio path already does the same for ASR).
2. `PUT <uploadUrl>` with the raw bytes, `Content-Type: application/octet-stream`.
3. `POST /open-apis/resource/parse` with **query parameters**
   `{fileUrl, objectName, model}` and an empty body → the parse result, whose
   `id` is the media handle the chat request references, plus `tokenUsage`.
   `code === 6002` inside `context.responseData` means the file parsed empty.
4. `GET /open-apis/resource/genNewUrl?oldFileUrl=` re-signs an expired URL.

The chat request (same bundle, the send path):

```js
{
  msgId, conversationId, query, isEditedQuery, previousDialogueId,
  sceneType, params,
  modelConfig: { enableThinking, webSearchStatus, model, temperature?, topP? },
  multiMedias: [ /* attachments; resourceSource is stripped client-side */ ]
}
```

A media item carries at least `name`, `mediaType`, `fileUrl`, `objectName`,
`url` (the parse result id), `tokenUsage`, `status`, `size`. Web search is
disabled whenever `multiMedias` is non-empty.

## Limits (live `bot/config`, 2026-10-09)

Every entry of `modelConfigList` (and `modelConfigListNg`) advertises:

```json
"fileUpload": {"enabled": true,
  "acceptTypes": ["text/plain", "application/pdf", "image/jpeg", "image/png"],
  "maxFileSize": 100, "maxFileCount": 50}
```

(the `mimo-v2.6-pro` entry additionally accepts `application/json`,
`text/markdown`, `text/xml`, `application/xml`, `text/html`, `application/x-sh`,
`text/x-python`). `maxFileSize` is megabytes, so the ceiling per file is 100 MB
with up to 50 files — orders of magnitude past the 102,400-character query cap.

The bundle's own static fallback is stricter and should not be trusted over the
live config: it marks `file.enabled: false` for `mimo-v2.6-pro-ultraspeed-studio`
and disables everything for `mimo-v2-flash`.

## What is verified, and what is not

- Verified: the endpoints exist and are what the SPA calls; the live catalog
  advertises the limits above; the chat payload's field is `multiMedias`.
- Not verified: whether the server accepts an attachment on a chat request we
  compose ourselves (the SPA sends extra fields, and the media item's exact
  required subset is unknown), whether a parsed text file really reaches the
  model, and what the effective per-account token ceiling for attachments is.

The check that settles it: upload a file whose text is impossible to guess
(`仓库编号 ZQ-7788`), ask for that number, and see whether the answer contains it.
`cmd/attachprobe` does exactly that — and it does pass.

### Measured (2026-10-09, live account)

| file | marker position | result |
| --- | --- | --- |
| 60,022 chars (180 KB) | last line | **PASS** — the model answered `ZQ-7788` |
| 150,033 chars (450 KB) | first line | **PASS** — 1.5× the query ceiling, read in full |
| 150,034 chars (450 KB) | last line | NO-MARKER — the attachment was delivered but truncated before its tail |
| 200,032 chars (600 KB) | last line | NO-MARKER — same, the model reported the document exists but lacks the number |

So the query ceiling is genuinely out of the picture: `resource/parse` accepted a
200,032-character file and reported `tokenUsage=200032`, i.e. accounted it
separately, and the model answered from a 150,033-character document. What bounds
a single attachment is the model's own ingest window (between 60,022 and 150,034
characters of CJK), not the 102,400-character query cap.

Two more findings from the same runs:

- A **minimal** media item (`name`, `fileUrl`, `objectName`, `url`) is refused
  every time with the upstream's busy frame (`服务器繁忙，请稍后再试`), while the
  full item (`name`, `mediaType`, `fileUrl`, `objectName`, `url`, `tokenUsage`,
  `status: "completed"`) is accepted. The field set matters, and this is the
  evidence for it.
- The parse handle is what the chat request needs: the same `objectName` with a
  different `url` is a different attachment.
