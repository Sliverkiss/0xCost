# 0007 — Search results are parsed per field from live evidence, and their absence is not a failure

Date: 2026-10-09
Status: accepted

## Context

The upstream returns web-search results inline as an `event:web_search` frame.
Two documented field shapes were wrong, and each wrongness had the same
consequence: the whole 16-result payload failed to deserialise and was dropped
silently, so a searched answer came back with zero citations while the model had
already emitted `(citation:N)` markers. Documentation said `usedContent` was the
string `"True"` and `metadata` was the string `"{}"`; the live frame sends
`usedContent: false` (a JSON boolean) and `metadata: {}` (an object).

A strict struct made one odd field fatal for the entire array, and the parse
failure was swallowed — the turn still succeeded, so nothing surfaced the loss.

## Decision

1. Decode each result item into `map[string]json.RawMessage` and read every field
   with a tolerant helper. A field with an unexpected type costs that field
   only: it never drops the citation, and never fails the turn.
2. A payload that is not JSON at all is reported as an error and recorded on the
   result (`CitationWarnings`), surfaced in the debug log. Silent skips are how
   the original bug survived review.
3. Citations are best-effort by contract: the upstream also returns answers with
   `(citation:N)` markers and no result set, and the gateway must not invent
   sources, block the answer, or fail the request in that case.
4. The upstream's page bodies (`text`, tens of KB per result) are not forwarded;
   only title, URL, display URL, snippet and the used flag travel.

## Consequences

- Live behaviour is now verified end to end: 13 citations non-streaming and 11 in
  a streaming chunk on a real search turn.
- Tolerating odd fields means a genuinely broken upstream payload degrades into
  sparse citations instead of an error; the debug trace is the place to notice,
  which is why the warnings are recorded rather than discarded.
- The reverse-engineering document was corrected where it disagreed with the
  live capture, since it is cited as authoritative by future work.

## Evidence

- `internal/mimo/sse.go` (`parseWebSearch`, `rawString`, `rawBool`),
  `internal/adapter/citations.go`, `internal/api/chat.go`.
- Tests: SSE-015…SSE-018, CITE-001…CITE-003, WS-001…WS-009.
- Live: `docs/web-surface-reverse.md` §5/§5.6; the frame it corrected is captured
  in the same document (id/text/metadata/name/url/displayUrl/snippet/usedContent).
