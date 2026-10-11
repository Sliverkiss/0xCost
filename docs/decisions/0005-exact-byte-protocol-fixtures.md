# 0005 — Protocol fixtures live as committed exact-byte files

Date: 2026-10-09
Status: accepted

## Context

The upstream wire format has byte-level quirks that decide whether the parser
works or silently corrupts output: SSE fields use a colon **without** a space
(`event:message`), and the thinking markers are the literal bytes `<think>`
plus a NUL byte. Inline Go string literals made those bytes hard to inspect,
easy to "fix" by an editor, and impossible to diff against a real capture.

## Decision

Recorded upstream bytes live in `internal/testutil/testdata/` as committed
files (`sse_*.txt`, `xssi_*.txt`, `openai_stream_expected.txt`), loaded through
a loader that fails loudly when a fixture is missing. Tests reference the
fixtures by the names the TDD's "Used by" column assigns them.

## Consequences

- A fixture can be produced from a live capture and compared byte-by-byte with
  `od -c`; a stray `\0` written as two characters is caught by a dedicated
  test instead of by a puzzling parser failure.
- Fixture bytes are protocol inputs, never credentials: no capture is
  committed until it is checked for cookies/tokens.
- Costs one indirection in tests; buys reviewable evidence.

## Evidence

- `docs/TDD.md` §2.1 (fixture table), `internal/testutil/testdata/`,
  the byte-exactness test in `internal/testutil/`.
