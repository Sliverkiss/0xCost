# Spec — Account pool: attribution, risk-control form, observability, egress

- **Status**: accepted (owner decided the three open questions on 2026-10-09; see §10)
- **Date**: 2026-10-09
- **Owner**: operator
- **Scope**: `internal/pool`, `internal/accounts`, the error-classification layer in
  `internal/api` + `internal/mimo`, the `/healthz` surface, and per-account egress.
- **Order agreed with the owner**: ① attribution → ② risk-control form → ③
  observability → ④ per-account egress.

## 0. How this spec was written

It is the second half of the reference exercise the owner opened with Qwen2API: the
session half is done (spec → SDD §4.3.1 → TDD §3.5.1 → ADR 0009 → live evidence), and
this is the pool half. As with that one, `Rfym21/Qwen2API` is **prior art to argue with,
not a template** — its design answers a different upstream, and the parts that do not
transfer are named below rather than quietly copied.

One correction belongs on the record, because it shaped the early draft of this spec:
the claim "461 is an egress property, measured" was **wrong** — that measurement belongs
to a different project. For MiMo we have **never observed 461 live at all** (the only
"461" strings in our captures are numbers inside web-search content). Everything we
believe about 461 comes from one line in a reference implementation
(`PROTOCOL.md:132-139`, quoted in `docs/login-chain-reverse.md:137`) plus our own
unverified assumption. §5 is written accordingly: the *form* of 461 is an open question,
and the requirement is to make it answerable, not to act as if it were answered.

## 1. What the pool owns

The pool answers three questions per request: *which account serves it*, *how many may be
in flight on one account*, and *when does an account come back*. It also owns the
operator's view of account health. It does **not** own sessions (ADR 0009 keeps session
identity separate, with sticky-session affinity passing an account id into `AcquireSticky`)
nor the upstream protocol itself.

## 2. Current behaviour (verified in code, 2026-10-09)

Selection is already better than the reference's: a score
(`inflight/capacity*100 + ewma_latency + warm_penalty`) with an even-load band and
round-robin inside it (`pool.go:293-365`), per-account in-flight caps with a bounded
queue wait, sticky acquisition for session continuity, a recovery probe gate before a
cooled account becomes selectable again (`pool.go:461-500`), and persistent disabled
state with a reason.

Error handling today, precisely:

| Signal | Pool action | Where |
|---|---|---|
| `ErrUpstreamBusy` ("服务器繁忙，请稍后再试") | **no pool action** — one retry after a short pause, account stays healthy ("platform load, not a dead account") | `chat.go:371-392` |
| content refusal | no pool action | `chat.go:394` |
| model rejected by the channel | no pool action, logged (gateway/catalog disagreement) | `chat.go:357-369` |
| 429 | `Report429`: 60 s doubling to a 10 m cap, one ladder | `pool.go:386-390` |
| 5xx **and** transport **and** SSE idle-stall | `Report5xx`: 30 s doubling to the cap — **one shared ladder and one shared `failStreak`**, documented as "Transport errors are treated the same" | `pool.go:392-396`, `chat.go:408`, `chat.go:571` |
| 451 / 461 / STS 70016 / captcha / dead passToken / OTP-pending | `ReportDisabled` (no retry); 461 and dead passToken are flagged `permanent` — never auto-probed, persisted across restarts | `chat.go:344-350`, `chat.go:772-797`, `pool.go:418-437` |
| 401 (session dead) | one renewal attempt via `RenewFunc`, then the account is auth-dead | `pool.go:73-86`, RENEW-* |

Observability today is four aggregate integers (`total/available/cooldown/disabled` on
`/healthz`, `server.go:268-287`) — no per-account state, no reason, no timing.

Accounts (`accounts.Account`) are `id/phone/password/enabled/label/user_id/pass_token/
session/state/cooldown_until/disabled_reason`. There is **no egress field**; all accounts
share one `http.Client` (`client.go:79`) using Go's standard transport with header-level
fingerprinting (no CGO/curl impersonation in this project).

## 3. Prior art: Rfym21/Qwen2API, and what actually transfers

Its `AccountRotator` (`src/utils/account-rotator.js`) and request path
(`src/utils/request.js`) carry hard-won production lessons, and their comments say why.

| Their mechanism | Transfers? |
|---|---|
| Separate classes: transport failure (counter, 3 strikes → 5 min cooldown) vs HTTP 4xx/5xx (warn-only, "upstream refused ≠ account broken") vs quota exhausted (long park) | **Partly.** The transport-vs-HTTP split transfers (§4 REQ-2/3); the 5xx policy does not — see below. |
| `quotaCooldownUntil`: quota-dead account parked for `Retry-After` or 1 h, released **by time only**, never cleared by the token-refresh reset (because that runs on a timer for all accounts and would hand the dead account straight back) | **No — rejected for us.** Qwen's free accounts have real daily quotas; MiMo's do not, as far as our account can see: the live `bot/config` has no quota/balance/plan/limit field at all (keyword and field-level scan, zero hits), the only quota-shaped endpoints are Claw's (`/open-apis/user/mimo-claw/{quotaUsage,tokenPlanUsage}`, the frozen product), and the only "quota" in our error vocabulary is `70022 SMS quota` from login. Adopting the class would invent a state we cannot observe. |
| `excludedEmails` per request — do not re-burn an account already tried in this request; give up when every candidate is excluded | **Already ours** (`Acquire(ctx, excludeIDs...)`), including the give-up path. |
| `resetFailures` on a successful credential refresh; explicit `cooldownStartedAt` rather than deriving it from last-use time (because CLI-only failures never update last-use) | **Transfers.** Ours resets the streak on success but there is no refresh-triggered reset, and no explicit marker — the ladder is a single counter shared by classes. |
| `lastErrorAt` / `lastErrorCode` kept separately from cooldown, for the operator's warn indicator | **Transfers** — this is most of §6. |
| Egress/service-level challenge handled by a dedicated breaker, not by blaming the account | **Structurally transfers** to 451/461 (§5), which is exactly the class we cannot yet classify. |
| Per-account `proxy`, per-account fingerprint UA | **Partly.** Per-account egress transfers (§7). Per-account UA does not: our fingerprint is a deliberate global constant (SDD §2.4), and varying it per account would be a risk-control *change*, not an isolation improvement. |
| LRU selection (`_selectLeastUsedAccount`) | **No.** Our score+band selection is strictly stronger; recorded only so the difference is deliberate. |
| `Retry-After` from upstream propagated to the client | **Partly, and deliberately not as-is.** We hide the upstream's number from clients (ERRMAP-011 privacy invariant); using it *internally* to size a cooldown is a separate question, raised as an open decision in §10. |

## 4. ① Failure attribution

**REQ-1.** Every upstream failure must be classified into exactly one class, and each
class must map to exactly one pool policy, stated in one table in the SDD (extending
SDD §7.1). The classes: `transport` (connection error, timeout, SSE idle-stall, EOF mid-
body), `upstream-5xx`, `throttle-429`, `service-busy` (the busy frame), `content`
(refusal), `model-rejected`, `account-auth` (401 after renewal), `account-dead`
(dead passToken / OTP demand / 70016), `risk-control` (451/461 — §5).

**REQ-2.** Transport failures and upstream 5xx must stop sharing one ladder and one
counter. Rationale, from our own code: a transport failure can be network- or
egress-shaped rather than account-shaped, yet today it consumes the same `failStreak`
(`pool.go:392`) and produces the same 30 s-doubling cooldown as an upstream 5xx; a
diagnosis cannot tell the two apart afterwards, and a local network blip escalates the
account's cooldown as if the account were sick. Each class gets its own counter and its
own base/cap, both configurable.

**REQ-3.** The 5xx *policy* stays as it is (cool the account, shedding our load from a
sick service) — the reference's "never cool on 5xx" is not adopted, because it answers an
upstream whose 5xx means "you specifically". This is a deliberate disagreement and must
be recorded as such in the SDD, not left implicit.

**REQ-4.** The already-correct behaviours are load-bearing and must not regress: the
service-busy frame stays a no-pool-action retry (`chat.go:371-392`), content refusals and
channel model rejections stay no-pool-action, and a client disconnect keeps counting as
neither success nor failure.

**REQ-5.** An account's cooldown must record *why* it is cooling (class + upstream status
+ timestamp), not just until when. This is what makes REQ-2 diagnosable and is the input
§6 renders.

**REQ-6.** The cooldown ladder must not escalate across classes: a `transport` failure
after three `upstream-5xx` failures starts its own ladder, and `ReportSuccess` resets all
counters. Rationale: today one mixed counter means an unrelated blip can push an account
to the cap.

**REQ-7.** A successful credential renewal/refresh must reset the failure counters for
that account (the reference's `resetFailures`; ours is missing), while never clearing
cooldowns that are time-owned.

## 5. ② The form of risk control (451/461): evidence before policy

**What we know.** 451/461 on `/open-apis/bot/chat`, `/open-apis/chat*` and `/open-apis/tts`
are handled as a ban; 461 is treated as permanent, and the reference implementation calls
it permanent. **What we do not know:** whether 461 is account-scoped, egress-scoped,
model-scoped, or a transient challenge — our own docs say so
(`docs/login-chain-reverse.md:169`), and we have never seen one live.

**REQ-8.** No behaviour change to 461 may be justified by assumption alone. Until evidence
exists, 461 stays fail-closed (account disabled, no retry) — but it must stop being
*opaque*: the event must be recorded with the account id, the egress label (§7), the
upstream status and body, the model, the channel, the timestamp, and how many other
accounts on the same egress hit a risk-control signal inside a configurable window.

**REQ-9.** The classification experiment is a deliverable, and it must be **passive**: it
correlates whatever 461/451 events occur naturally (cross-account-same-egress,
same-account-across-egresses, same-account-across-models) and reports the shape. We do
**not** deliberately trigger risk control on the operator's real account to learn the
answer — account safety outranks engineering convenience, and nothing in this project may
induce a ban to study it. If the evidence never arrives, the fail-closed default stands
and the spec says so.

**REQ-10.** The three shapes get three different policies, decided once evidence lands:
(a) *account-scoped* → keep disable, keep permanence, keep operator recovery;
(b) *egress-scoped* → a dedicated egress brake: the accounts behind that egress become
temporarily unselectable while the accounts themselves stay healthy, and the brake is
released by time + probe, never by an account reset;
(c) *transient/challenge-shaped* → one bounded retry, then the egress brake.
A `permanent` flag must never be set on an unclassified signal: permanence is a claim
about the upstream's intent, and today we cannot support it.

**REQ-11.** Whatever the outcome, an operator must be able to see and undo it: the current
state (`permanent` or braked), the evidence that produced it, and a documented way to
re-enable (already `Enable(accountID)`, plus the egress brake release).

## 6. ③ Observability

**REQ-12.** A per-account diagnostic view must exist for the operator: for each account —
id (masked), label, enabled/state, in-flight vs capacity, EWMA first-byte latency,
cooldown end and its class/reason, disabled reason and whether it is permanent, failure
counters per class (§4), last error code + timestamp, egress label, and credential age
(session/ph renewal time) where known.

**REQ-13.** The aggregate `/healthz` contract must not change: the Docker healthcheck
subcommand parses that body, and it is the only vendor-neutral surface. The detailed view
is a **separate authenticated endpoint, `GET /v1/admin/pool`** (owner, §10.1), authenticated
the same way as the existing `/v1/admin/otp/submit`.

**REQ-14.** The diagnostic view must never leak a credential: no cookie, no `ph`, no
password, no proxy URL with credentials, no upstream payload — only ids, states, reasons,
counters and timings. Account ids are masked to a stable short form so correlation works
without exposing the account.

**REQ-15.** A risk-control event (§5) must be visible in the same view, including the
correlation needed to read its shape (other accounts hitting the same signal on the same
egress), because that view *is* the instrument the experiment of REQ-9 depends on.

## 7. ④ Per-account egress

**REQ-16.** An account may be assigned an egress label. Accounts without one share the
default egress (`direct`). The label is what §5's correlation and any egress brake key on.

**REQ-17.** Credentials for an egress live in the environment, never in a tracked file or
in the repository: `accounts.json` carries only the label (e.g. `"egress": "hk1"`), while
the actual endpoint and its credentials come from `MIMO2API_EGRESS_<LABEL>` (a URL) at
startup. This follows the project rule that credentials are runtime-only, and it keeps a
shared accounts file publishable.

**REQ-18.** An unknown label (no matching environment variable) must fail loudly at
startup, listing the missing names — silently falling back to `direct` would send an
account out of an egress the operator believes it is behind, which is worse than refusing
to start.

**REQ-19.** Requests for an account must go out through its egress on every upstream call
this gateway makes for that account — chat (both channels), TTS, ASR, `bot/config`,
renewal, and the recovery probe. A partially applied egress would produce exactly the
mixed-signal mess §5 is trying to diagnose.

**REQ-20.** Per-account egress is an isolation tool, not a rotation trick: this spec does
not add proxy pools, health-scored proxies, or automatic egress switch-on-failure. If an
egress is braked (§5), its accounts wait or the operator intervenes.

## 8. Invariants

- **INV-1.** Accounting: `available + cooldown + disabled == total` at any observation
  point. (Holds today; asserted by the existing summary tests.)
- **INV-2.** No request is served by an account whose class counters are escalated
  (disabled), whose cooldown is live, or whose egress is braked.
- **INV-3.** Every state change is attributed to a class and a timestamp (§4 REQ-5).
- **INV-4.** No secret in any exposed surface: no credential, cookie, `ph`, password,
  proxy URL or upstream body beyond the status/error code (§6 REQ-14).
- **INV-5.** `permanent` implies evidence of account-scoped intent; unclassified signals
  are never permanent (§5 REQ-10).
- **INV-6.** Removing an account's egress label returns it to `direct` on the next start
  with no other behaviour change (reversibility).

## 9. Non-goals

- **Quota-exhaustion class**: rejected for this upstream (§3).
- **Claw**: frozen by the owner; no pool work touches it.
- **Protocol-level egress shaping** (TLS/JA3 impersonation) — the fingerprint stays a
  global constant (SDD §2.4).
- **Deliberate risk-control triggering** to learn 461's form (§5 REQ-9).
- **Automatic failover to another egress** on failure (§7 REQ-20).
- **Session affinity across accounts**: sticky sessions pin an account on purpose
  (ADR 0009); the pool must not silently move a live conversation to another account.

## 10. Decisions (owner, 2026-10-09)

1. §6 REQ-13 — **decided**: a separate authenticated `GET /v1/admin/pool`; `/healthz`
   keeps its four aggregate numbers frozen for the healthcheck subcommand.
2. §3: use the upstream's own `Retry-After` hint *internally* to size cooldowns? —
   **decided: no.** Cooldowns are sized by our own ladders only (owner: "完全不用上游的
   数字，只用我们自己的阶梯"). The upstream's number is still never echoed to clients
   (ERRMAP-011). Consequence: REQ-2's per-class base/cap constants are the single source
   of cooldown timing, and nothing in the pool may read a `Retry-After` value.
3. §7 REQ-16: label vocabulary — **decided**: free text, validated at startup against
   `[a-z0-9-]{1,16}`, so the operator names egresses the way they think about them
   (`hk1`, `sg2`) without a vocabulary the gateway has to be edited to extend.

## 11. Acceptance plan

Unit/integration (all in the mock suite, no network):

- **POOLATTR-001..0nn**: one case per class of §4, asserting the pool action (or its
  absence) *and* that the other classes' counters are untouched; a transport failure after
  three 5xx failures starts a fresh ladder; `ReportSuccess` clears all counters; a
  successful renewal clears counters but not a live cooldown.
- **RISK-001..00n**: a 461 event is recorded with account/egress/model/channel/status/body
  hash and timestamp; the same signal on two accounts behind one egress is correlated; a
  451 does not set `permanent`; `Enable` clears it.
- **POOLVIEW-001..00n**: accounting invariant; masking; no-credential assertion over the
  serialized body (extending the existing hygiene checks); cooldown/brake reasons present.
- **EGRESS-001..00n**: an account with a label uses the labelled endpoint for a chat, a
  TTS, a `bot/config` read and a probe (asserted through the injected transport); a missing
  label fails startup with the name listed; an unlabelled account uses `direct`.

Live (gated by `MIMO2API_LIVE=1`, evidence recorded in `docs/live-findings.md`):

- **LIVE-POOL-001**: pool behaviour under the real account — a cooldown from a real 429 (if
  one occurs naturally) clears through the probe gate, and the diagnostic view matches what
  the requests did.
- **LIVE-POOL-002**: passive risk-control observation (§5) — recorded if and when 451/461
  occurs; the spec explicitly allows this to remain "no observation yet", because inducing
  it is forbidden.
- **LIVE-EGRESS-001**: one account behind an operator-provided egress completes a chat
  turn and a TTS synthesis, proving REQ-19 end-to-end (requires the owner to supply an
  egress endpoint; without one, this stays unverified and is reported as such).

## 12. References

- `docs/specs/session-identity.md` — the session half of this exercise, same method.
- `docs/decisions/0009-sessions-are-streaming-only-and-verified.md` — the settled session
  decisions, including why the pool and sessions stay separate.
- `docs/login-chain-reverse.md:137,169` — the 461 claim and our own unverified-form note.
- `docs/SDD.md` §2.4 (fingerprint), §4.1/4.4/4.5 (pool states, selection, cooldown ladder),
  §4.6 (permanent disable), §7.1 (error mapping), §7.4 (client-facing errors).
- `docs/web-surface-reverse.md` §8 — Claw's quota surfaces, excluded here.
- Reference (prior art, not a template): `Rfym21/Qwen2API`,
  `src/utils/account-rotator.js`, `src/utils/request.js`, `src/utils/upstream-error.js`,
  `src/utils/token-manager.js`.
