# 0008 — Long speech is split on sentence boundaries and rejoined, never truncated

Date: 2026-10-09
Status: superseded by 0010 (the split was implemented and measured live: a
piece near the cap makes the upstream fail the task itself, so the cap is now
refused up front and the splitter is gone)

## Context

The upstream refuses a single synthesis whose spoken text exceeds 1024
characters (`播报内容长度不能超过1024，当前长度为：N`, verified live). The gateway's
own cap was 10,000 characters and it forwarded `input` unchanged, so any passage
over 1024 characters could only fail — and it failed late, as a `502
upstream_failure` that said nothing about length (measured: 1500-character input
rejected in 0.2s after a wasted upstream round trip).

Synthesis cost per request also grows faster than the text: 128 characters took
12.8s, 384 took 17.8s, 700 took over 220s, and the flat 60-second task deadline
expired before a 1024-character piece finished.

## Decision

1. `input` longer than the upstream limit is split on sentence terminators
   (`。！？；!?;` and newlines), keeping each piece within the limit; a single
   sentence over the limit is hard-split. Runes are never split mid-character.
2. Pieces are synthesised sequentially with the requested voice, format and
   instruction, then joined: WAV is rebuilt from the first piece's own `fmt`
   chunk with the data chunks concatenated (mismatched encodings are refused),
   MP3 is a frame concatenation. A single piece is relayed byte-for-byte.
3. Any failing piece fails the request. Partial audio is never returned: a
   half-spoken passage is worse than an error the caller can retry.
4. The per-piece deadline is `MIMO2API_AUDIO_TTS_TIMEOUT_S` plus 500 ms per
   character, capped by `MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S`. The multiplier is
   deliberately ~5x the best observed throughput because the upstream's cost is
   superlinear and load-dependent, and it publishes no rate.
5. The upstream's own length rejection is classified as a client error (400
   `input_too_long`) rather than a 502, so a limit change cannot resurface as an
   opaque gateway failure.

## Consequences

- A 1500-character request now costs two upstream syntheses and returns one
  playable file; the caller never needs to know the upstream's limit.
- Long passages are slow (minutes) because the upstream is slow; `README.md`
  documents the measured timings rather than promising a latency.
- Splitting at sentence boundaries keeps prosody sane, but a passage with no
  punctuation at all gains audible seams at the hard-split points — accepted,
  since the alternative is no audio at all.
- `MIMO2API_SPEECH_MAX_CHARS` (10,000) remains the gateway's total guard; the
  upstream limit is no longer a surprise.

## Evidence

- `internal/mimo/speech_chunk.go` (`SplitSpeech`, `JoinWAV`, `JoinMP3`),
  `internal/api/audio.go` (`ttsTimeoutFor`, `classifyAudioUpstream`).
- Tests: SPLIT-001…SPLIT-007, JOIN-001…JOIN-007, TTSCHUNK-001…TTSCHUNK-004.
- Live: `docs/web-surface-reverse.md` §3 (formats and the 1024-character cap).
