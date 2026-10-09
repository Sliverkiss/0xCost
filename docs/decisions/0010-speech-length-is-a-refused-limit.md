# 0010 — Long speech is a refused limit, not a split

- **Status**: accepted (supersedes ADR 0008)
- **Date**: 2026-10-09
- **Deciders**: operator (decision), gateway (evidence)
- **Supersedes**: [0008](0008-long-speech-split-and-rejoined.md)

## Context

The Studio refuses any single synthesis whose spoken text exceeds 1024
characters (`播报内容长度不能超过1024，当前长度为：N` — verified live). ADR 0008
answered that by splitting longer input on sentence boundaries with pieces of up
to the cap, synthesising each piece and rejoining the audio, with the per-piece
deadline scaled by length (60 s + 500 ms per character) so a long piece would not
be cut off early.

That workaround was implemented, unit-tested and then measured against the real
service:

| input | behaviour |
|---|---|
| 128 runes | 12.8 s, audio returned |
| 384 runes | 17.8 s, audio returned |
| 700 runes | no answer within our 220 s client timeout |
| 1110 runes → 2 pieces (piece 1 ≈ 1020 runes) | **502 after 278.2 s**: `the upstream audio task failed: piece 1/2: upstream audio task failed (task 16075873, audit PASS)` |

Three things follow from that run. The split itself worked — the error names
`piece 1/2`, so the gateway did cut the text and submit pieces in order. The
failure was the upstream's own: it returned a failed task state (with its content
audit passing), not our deadline — the piece's budget was ~570 s and the failure
arrived at 278 s. And the cause is piece size: the splitter packs up to the cap,
so a 1110-rune request becomes one ~1020-rune piece, which sits far outside the
reliable region this service shows (384 runes is comfortable; 700 already is
not).

Raising `MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S` therefore does not help: the upstream
gives up on a near-cap piece regardless. Only a much smaller piece target (~350
runes, i.e. one synthesis per sentence group) would land inside the reliable
region — at the cost of four upstream tasks for a 1110-rune input, four chances
to fail, and a latency that is no better than the client sending shorter
requests itself.

## Decision

The cap is the contract. `> SpeechTextLimit` (1024) input is refused with a 400
before any upstream work, naming the limit and the actual length
(`code: input_too_long`). Splitting is removed: `SplitSpeech`, `JoinAudio`,
`JoinWAV`, `JoinMP3` and their tests are deleted rather than left as an
unreachable path, and `MIMO2API_SPEECH_MAX_CHARS` (default now 1024) is clamped
to the upstream limit so a larger configured value cannot accept requests that
cannot succeed. The length-scaled synthesis deadline stays — a single legal
request can still be slow.

## Consequences

- A client that sends more than 1024 characters learns it immediately and for
  free, instead of waiting minutes for a 502. The old behaviour cost ~278 s of
  upstream time per over-length request.
- Long passages are the client's problem to segment. That is honest: the client
  knows its own text, can pick boundaries that make sense for speech, and can
  stream the resulting requests.
- `MIMO2API_SPEECH_MAX_CHARS` can lower the cap (operator preference) but never
  raise it; the clamp is asserted (TTSLIMIT-003).
- The cap is a hard limit, not a reliable target: 384 runes synthesised in
  17.8 s, while 700 runes had not answered within 220 s and a ~1020-rune piece
  failed outright. Clients that want dependable audio should keep requests in
  the low hundreds of characters, well under the 1024 they are allowed to send.
- If long-form speech is ever wanted again, re-adding the splitter is not enough:
  the target piece size must be ~350 runes, and that measurement is the reason.
  The removed code is in the history of this commit's parent.
