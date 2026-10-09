# 0030 — Client strings are data, not structure

Status: accepted (2026-10-10)

## Context

A review of the input layer for defects of the kind this project keeps producing —
a client-controlled string reaching somewhere that expects something with structure —
turned up three of them, each with a reproduction and a measurement rather than a
suspicion.

## The three defects

### 1. The warning header was built from client keys with no bound

`X-Mimo-Warnings` lists the field names a request used that we ignore, and those
names are whatever the client wrote. A request carrying 3000 unknown top-level
fields produced a warning header **past 64 KiB**, and the caller could not parse the
response at all: Python's `http.client` raises `LineTooLong: got more than 65536
bytes when reading header line`. A client that sent extra metadata lost its answer.

Also, the list came out of a JSON object, so the same request produced a different
order on every call.

Now: the header lists at most 8 names, each shortened to 40 characters, sorted and
deduplicated, with the remainder as `+N more`. Measured after: **392 bytes** for the
same 3000-field request, answer delivered.

### 2. The declared media type decided the file name's extension

`imageExt` appended the declared subtype verbatim, so `data:imAge//;BAse64,000A` — the
input the fuzzer found — produced `image-1./`. `image/../..` travelled as path syntax
into the name handed to the platform's upload API. The type gate was a prefix test
(`strings.HasPrefix(mime, "image/")`) which `image//` satisfies.

Now: three layers. A declared type must be `image/` plus a token-shaped subtype
(`imageMimeToken`, ≤24 chars, no separators, no `..`), the bytes are sniffed and win
when they name a different image type, and the platform-facing name is rebuilt from
the type the bytes are — so an image served from `/photo.php` arrives as `.png`.
Bytes the sniffer positively identifies as something else (text, JSON, HTML) are
refused: `data:image/png` over text used to be uploaded as an image.

### 3. Refusals named the wrong reason

Every `unsupported_image_type` answered *"the fetched URL did not answer an image
(content-type must be image/*)"*, which is untrue for an inline image and says nothing
about which of the three rules was broken. The early URL gate answered the bare
sentinel `unsupported image_url`, so `data:text/plain;base64,...` was refused without a
word about the type.

Now: refusals carry the wrapped detail (`the bytes are text/plain, not an image`,
`image// is not an image type`, `scheme "file" is not fetchable, use https:, http: or
data:image/...`). Codes are unchanged. The shape gate now only judges the scheme; the
declared type is judged by the content gate, so one input no longer gets two different
codes depending on how far it travelled.

## Mechanism

Both classes were found by tooling, not by reading:

- **Fuzz targets** (Go's built-in fuzzer, no dependency) over the three client-driven
  entry points: `decodeChatBody`, `applyImageDetail`, `decodeDataImage`. They run as
  unit tests without `-fuzz`, so their invariants — no panic, bounded warning header,
  no path syntax in an upload name, byte cap respected — are checked on every gate run.
  Defect 2 was found by `FuzzDataImageURL` inside 30 seconds; the failing input is
  committed as a regression seed under `internal/api/testdata/fuzz/`.
- **A stdlib-only AST pass** (`staticaudit`) for discarded error results, `strings.Cut`
  results used without their "found" flag, and `defer`/`time.After` inside loops. It
  flagged 5 sites worth reading; all five were correct as written, three of them
  deliberately (the `defer lease.Release()` backstop, idempotent by design).

## Consequences

- A client can no longer make the response unparseable, or put path syntax into a name
  another system stores.
- Refusals are actionable: the caller is told which rule broke.
- `data:` URLs are accepted at the shape gate whatever their type, and judged by the
  content gate — one code per cause.
