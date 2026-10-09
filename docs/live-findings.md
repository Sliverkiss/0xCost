# Live findings (real upstream, real account)

Run: `MIMO2API_LIVE=1 go test ./internal/live -v` (see `internal/live/README.md`
for the env switches). Credentials come from the operator's
`data/accounts.json`; the suite works on a **copy** in a temp dir, so a run
never mutates the live session.

Last run: **2026-10-09 14:29 CST**, account `182******16`, egress IP outside
mainland China (which is why the login chain traverses the cross-region
consent page — see `docs/login-chain-reverse.md`).

## Status of LIVE-001..010

| ID | Case | Result | Evidence |
|---|---|---|---|
| LIVE-001 | Cold-start login | **SKIP** (human gate) | With `MIMO2API_OTP_AUTO=false` the cold login reaches the new-device verification demand in 1.6 s, is reported as an operator action and leaves the account out of rotation. Completing it needs an SMS code (`MIMO2API_LIVE_OTP_CODE`) — the code was already burned twice on login bugs that are now fixed (`cfc286c`), so the run skips instead of dispatching another SMS. |
| LIVE-002 | Model catalog | **PASS** | `GET /v1/models` → 200, contains `mimo-v2.6-pro` and `mimo-v2.6-pro-ultraspeed-studio`; no upstream call. |
| LIVE-003 | Non-streaming echo | **PASS** | 4.4 s, `content="pong"`, `finish_reason="stop"`, `usage={prompt:2230 completion:11 total:2241}`. |
| LIVE-004 | Streaming | **PASS** | 5.0 s, 5 frames: role-only delta → 2 content deltas (`"1\n2\n3\n4\n"`,`"5"`) → finish frame → usage frame (`choices:[]`) → `[DONE]` — the documented SDD §5.4 order. |
| LIVE-005 | Sticky two-turn | **PASS** | 27.0 s: turn 2 answered "Your favorite number is 42 — you told me earlier in this conversation", session store hit (`lookups=1`, `len=1`) → the delta path was taken. |
| LIVE-006 | Transparent refresh | **PASS** | 7.8 s with the cached session deleted: one STS renewal re-minted and persisted a 886-byte cookie with `ph` set, account stayed available. No password login, no OTP. |
| LIVE-007 | 429 semantics | **PASS (inconclusive, recorded)** | See below. |
| LIVE-008 | Captcha probability | **SKIP** | Needs 5 one-time codes (`MIMO2API_LIVE_OTP_CODES`); each cold login sends an SMS, so the run refuses to start without them. |
| LIVE-009 | Fastchat usage merge | **PASS (recorded)** | See below. |
| LIVE-010 | Conversation GC | **phase 1 PASS** | Phase-1 snapshot written (24 h gap required); phase 2 runs with `MIMO2API_LIVE_PHASE=2`. |
| LIVE-SESS-001 | Session chaining vs replay | **PASS** | See below — pinned turn chained in 8.2 s, replay control 47.4 s, context preserved across turns. |
| LIVE-SESS-002 | One-shot stays stateless | **PASS** | `stream:false` with a live session id: 200, no session headers, store untouched (`sessions 1→1`, `lookups 1→1`). |
| LIVE-TTS-003 | Long speech beyond the cap | **FAIL (by design, then removed)** | 1110 runes → 2 pieces → 502 after 278.2 s: piece 1/2 failed upstream. The split worked; a ~1020-rune piece is beyond what the service synthesises. Cap is now refused up front (ADR 0010). |

## LIVE-007 — does the upstream 429 carry a structured deadline?

```
burst=30 requests, no pacing, model=mimo-v2.6-pro
no non-200 within 30 unpaced requests — no 429 observed
```

**Finding:** a sequential burst of 30 requests against one account did **not**
reach the upstream rate limiter (each request took ~5 s, so the burst ran at
roughly 0.2 req/s). Appendix B Q1 therefore stays **open**: the fixed cooldown
ladder in SDD §4.5 remains untested against a real 429. Answering it needs
either concurrency (N in-flight requests) or a much longer run — deliberately
deferred, because tripping the limiter cools the account down for real.

`UpstreamError` now retains the response headers (`mimo.UpstreamError.Headers`)
so that a future run can read `Retry-After` / `x-ratelimit-*` straight off the
429; those headers are still never echoed to API clients (TDD ERRMAP-011).

## LIVE-009 — fastchat repeated usage events

```
model=mimo-v2.6-pro-ultraspeed-studio frames=4 done=true usage_events=1
usage[0] prompt=2241 completion=177 total=2418
last total >= all earlier totals: true
VERDICT: a single usage event — no merge question for this model/run
```

**Finding:** the fastchat channel emitted exactly one usage event for a short
answer, so the "multiple usage, last-wins?" question (Appendix B Q3) is not
answered by this run — it needs a long/tool-heavy completion. The SSE-012
last-wins rule stays unconfirmed, not contradicted.

## LIVE-010 — conversation GC across a 24 h gap

Phase 1 ran a turn, saved the sticky store snapshot and the turn transcript.
Phase 2 (`MIMO2API_LIVE_PHASE=2`) restores the snapshot, sends a delta turn and
checks whether the answer still carries the fact from phase 1:

- answer references the fact → upstream auto-save suffices, SDD §4.3 unchanged;
- `event:error` / lost context → SDD §4.3 must be amended to call
  `POST /open-apis/chat/conversation/save` on store.

## Notes for the next run

- `MIMO2API_LIVE_OTP_CODE` completes LIVE-001 (the whole point is that the
  gateway, not a script, walks consent → identity → landing chain).
- `MIMO2API_LIVE_OTP_CODES` (5 codes) enables LIVE-008.
- `MIMO2API_LIVE_PHASE1_STATE` overrides the phase-1 snapshot path (default
  `$TMPDIR/mimo2api-live-phase1.json` — move it somewhere durable before the
  24 h wait).
- The suite paces upstream calls by 2 s; the burst case is the only exception
  and is opt-in.

## LIVE-SESS-001 — the chained session is real, and it is the cheaper path

```
turn 1 (fresh, 3 messages replayed) → mode=replay elapsed=26.1s session=60dd9a45f6d0fc9190b684be519a3756
turn 2 (pinned, 1 delta message)   → mode=chain  elapsed=8.2s
turn 3 (control: same 4-message history, own session) → mode=replay elapsed=47.4s

chained frontier advanced: msgs 3 → 4, dialog anchor "16075414" → "16075443", same upstream conversation=true
chained answer: 海水很蓝
control answer: 海水很蓝
delta (control - chained) = 39.3s
```

What this proves, and what it does not:

- The pin is honoured across processes without stored state on the client's
  side: turn 2 sent only the new message (`deltaQueries=1`) and the upstream
  answered with the fact from turn 1 ("海水很蓝"), so the server-side
  conversation really was continued rather than reconstructed from replay.
- The frontier and the dialog anchor advanced on the chained entry, and the
  upstream `conversationId` was reused — the store, the header and the answer
  agree with each other.
- The control turn replayed the same history into a session of its own and paid
  47.4 s against 8.2 s. Latency on this service is noisy (turn 1's own replay
  cost 26.1 s), so read the delta as "the replay path pays for the whole history
  and the chain does not", not as a fixed speed-up factor.
- **Not covered live**: account migration (SESS-040, covered by the integration
  test) and the concurrent-fork race (SESS-020/021, covered by the integration
  tests) — both need either a second account or two simultaneous streams, which
  the live suite deliberately does not do against the operator's single account.

## LIVE-TTS-003 — a split near the cap does not help, and the cap is now refused

```
input: 1110 runes ("小米 MiMo 语音合成测试。" × 74), format wav
result: HTTP 502 after 278.2 s
body:   the upstream audio task failed: piece 1/2:
        upstream audio task failed (task 16075873, audit PASS)

earlier, same day, single syntheses (no splitting):
  128 runes → 12.8 s, audio returned
  384 runes → 17.8 s, audio returned
  700 runes → no answer within a 220 s client timeout
```

Reading it:

- **The split fired**: the error names `piece 1/2`, so the gateway cut the text
  on sentence boundaries and submitted pieces in order — the mechanism worked.
- **The failure was the upstream's own**, not our deadline: the task came back in
  a failed state (its content audit passed) at 278 s, while that piece's budget
  was ~570 s (60 s + 500 ms × 1020).
- **Piece size is the cause**: the splitter packs up to the 1024 cap, so an
  1110-rune input becomes ~1020 + ~90, and ~1020 runes sits far outside the
  reliable region this service shows. Raising the deadline cannot fix that.
- **Outcome**: >1024 input is now refused with 400 `input_too_long` before any
  upstream work, the splitter is deleted, and `MIMO2API_SPEECH_MAX_CHARS` is
  clamped to 1024 (ADR 0010). The old path cost ~278 s of upstream time per
  over-length request and then failed anyway.
