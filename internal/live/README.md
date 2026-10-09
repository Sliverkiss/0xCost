# internal/live

The LIVE-* acceptance suite (TDD §1.3, §3.12, §6.5). It talks to the **real**
aistudio upstream with the operator's account, so it never runs implicitly:

```bash
MIMO2API_LIVE=1 go test ./internal/live -v
```

Without `MIMO2API_LIVE=1` every test skips instantly (it is safe to leave the
package in `./...` runs).

## Guarantees

- **Sequential and paced** (2 s between upstream calls), one pass per run.
- **Credentials** come exclusively from the operator's accounts file
  (`MIMO2API_LIVE_ACCOUNTS_FILE`, default `../../data/accounts.json`).
- The system under test gets a **copy** of that file in a temp dir, so a run
  never mutates the live session. Persistence assertions read the copy.
- **No test auto-solves a challenge.** A new-device OTP demand skips unless the
  operator supplies a code.

## Environment switches

| Variable | Effect |
|---|---|
| `MIMO2API_LIVE=1` | Required. Enables the suite. |
| `MIMO2API_LIVE_ACCOUNTS_FILE` | Accounts file to read (default `../../data/accounts.json`). |
| `MIMO2API_LIVE_OTP_CODE` | One-time code for LIVE-001's cold login. Without it LIVE-001 runs with OTP auto-start **off** and skips after proving the demand is reached. |
| `MIMO2API_LIVE_OTP_CODES` | Comma-separated codes (5) for LIVE-008. |
| `MIMO2API_LIVE_HAMMER=1` | Opt-in for LIVE-007 (unpaced burst; burns the account's short-term rate budget). |
| `MIMO2API_LIVE_PHASE=2` | Runs LIVE-010 phase 2 (needs a phase-1 snapshot ≥24 h old). |
| `MIMO2API_LIVE_PHASE1_STATE` | Path of the LIVE-010 phase-1 snapshot (default `$TMPDIR/mimo2api-live-phase1.json`). |
| `MIMO2API_LIVE_RECORD` | File to append LIVE-007/008/009/010 findings blocks to. |

## Reading the results

- `docs/live-findings.md` records the outcome of the last run, including which
  Appendix B questions are still open and why.
- Findings are printed with `t.Logf` and, when `MIMO2API_LIVE_RECORD` is set,
  appended there as fenced blocks. Nothing is written to the repo by default.
