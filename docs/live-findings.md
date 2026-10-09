# Live findings (real upstream, real account)

Run: `MIMO2API_LIVE=1 go test ./internal/live -v` (see `internal/live/README.md`
for the env switches). Credentials come from the operator's
`data/accounts.json`; the suite works on a **copy** in a temp dir, so a run
never mutates the live session.

Last run: **2026-10-09 12:10 CST**, account `182******16`, egress IP outside
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
