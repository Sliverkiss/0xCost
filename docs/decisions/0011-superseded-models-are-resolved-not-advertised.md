# 0011 — A superseded model id is resolved, not advertised

Status: accepted (2026-10-09)
Supersedes: none
Related: SDD §5.2, TDD MODELS-014/015, CHAT-REDIR-001, AUDIO-REDIR-001,
`docs/web-surface-reverse.md` §3

## Context

The upstream's own catalog states which ids it has retired: 18 of the 25 rows in
this account's `bot/config` carry a **`redirectTo`** naming the id the platform
serves instead (`mimo-v2-flash` → `mimo-v2.6-flash`, `mimo-v2-tts` →
`mimo-v2.5-tts`, and so on, including one two-hop chain,
`mimo-v2-flash-studio` → `mimo-v2-flash` → `mimo-v2.6-flash`).

We parsed the field since the M19 work but never used it, and the deployments
advertised every row. The upstream then refuses the retired id:

- **Audio surface**: HTTP 400, body `{"code":400,"msg":"model: [mimo-v2-tts] not
  support"}`, or — for an id it cannot resolve at all — the misleading
  `modelCode cannot be empty`. A live `/v1/audio/speech` naming `mimo-v2-tts` or
  `mimo-v2.1-tts` answered **502 `upstream_failure`** with no explanation, while
  `mimo-v2.5-tts` synthesised normally.
- **Chat surface**: the retired id is answered with the platform's busy frame
  (`服务器繁忙，请稍后再试`), which we map to 503 `upstream_busy` and one retry. Three
  interleaved rounds in the same minute: `mimo-v2.6-flash` 3/3 succeeded,
  `mimo-v2-flash` 3/3 returned busy.

So the deployment offered, by name, models it could not serve, and the failure it
produced named neither the model nor the reason.

## Decision

1. **Only what the upstream will run is advertised.** `/v1/models` drops every row
   carrying `redirectTo`; for the current account that is 7 models
   (`mimo-v2.6-flash`, `mimo-v2.6-pro`, `mimo-v2.6-pro-ultraspeed-studio`,
   `mimo-v2.5-tts`, `mimo-v2.5-tts-voiceclone`, `mimo-v2.5-tts-voicedesign`,
   `mimo-v2.5-asr`). The owner's direction: clean the list by `redirectTo` and keep
   only the usable entries.
2. **A request naming a retired id is resolved before it reaches the upstream**, on
   both surfaces, transitively and with a cycle guard — the same thing the Studio
   SPA does with its client-side `deprecatedModelRedirectMap`, which is built from
   this exact field. The response still echoes the id the client asked for, and
   `/v1/models/{id}` returns the successor's entry under the requested id with a
   `redirect_to` field, so the substitution is discoverable rather than silent.
3. **A refusal teaches us.** If the upstream still refuses a model we sent — the
   retirement happened after our last refresh — the refusal itself rebuilds the map
   (at most once per 5 s, so a burst cannot stampede the catalog read) and the
   request is retried once on the id the upstream wants. Both surfaces do this: chat
   on the busy frame, audio on the explicit `not support` / `modelCode cannot be
   empty` answer. Without it the first request after a retirement fails with an
   error that names neither the model nor the reason.
4. **The map is read from memory on the request paths.** It is warmed at startup by
   the binary, refreshed on the janitor tick and as a side effect of reading the
   model list. A chat or audio request must never trigger a catalog read: doing so
   turned a single chat request into a second login attempt and a second OTP
   challenge (caught by ADOTP-001/002).

## Consequences

- A client using an old id keeps working and gets the successor's answer, with the
  substitution visible in `/v1/models/{id}`, in the debug log, and in the model
  name the upstream echoes internally.
- The advertised set is no longer a mirror of `bot/config` but an opinion about it:
  a model the upstream retires stays hidden until the catalog changes. The refresh
  cadence is the catalog TTL for the list (`MIMO2API_CATALOG_TTL_S`, default 10m),
  the janitor tick for the map (`MIMO2API_PROBE_INTERVAL_S`, default 60s), and the
  refusal path for the moment it actually bites. Nothing is hardcoded: no model name
  appears in the code, so a retirement needs no deploy.
- The names that used to look duplicated collapse on their own: the retired alias
  is gone from the list, leaving the one the upstream actually serves.
- **Cost accepted**: a model name that is requested but silently served by another
  model can surprise a client comparing `model` across responses. Mitigated by the
  `redirect_to` field and by the debug line naming both ids; not eliminated.
- We depend on the upstream's field being accurate. It was verified against the
  live catalog before this decision, and a wrong `redirectTo` would send a
  resolvable request to a model the caller did not choose — which the same
  `redirect_to` field makes visible after the fact.
