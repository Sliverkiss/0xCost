# 0021. A platform-supplied PORT with no API key refuses to start

Date: 2026-10-09
Status: accepted
Related: ADR 0018 (hosted deploy, port resolution), ADR 0009/0013 (account safety)

## Context

The first hosted deployment (Belmo, `https://mimo2api-ac40.onbelmo.uk`) answered
Belmo's own "Service starting up — your service is warming up" page, HTTP 503, for
more than five minutes. Nothing in that page says why, and the platform's build and
run logs are the only place the reason appears.

Reviewing the startup path against a hosted platform found one silent failure that
reproduces exactly that symptom. The gateway has always had this rule:

```go
// Empty API keys force a local bind.
if len(c.apiKeys) == 0 && c.host != "127.0.0.1" && c.host != "localhost" {
    c.host = "127.0.0.1"
}
```

That is right on a laptop (measured: `listening on 127.0.0.1:8125`, reachable by
the operator) and harmless in compose, which sets `MIMO2API_PORT`. On a PaaS it is
a trap: the port comes from the platform's own `PORT`, the platform's router
connects from **outside** the container, and a loopback bind is therefore
unreachable. The platform then keeps serving its "starting up" page, and the
gateway's own log line — `listening on 127.0.0.1:<port> (accounts: …, api keys: 0)`
— is the only clue, sitting in a log the operator may not be looking at.

Port resolution itself was verified correct before this conclusion: building the
exact nixpacks command from a clean checkout and running it with only `PORT=8123`
set produced `listening on [::]:8123 (accounts: 1 enabled, api keys: 1)`. So the
port is honoured; what was missing was the key.

## Decision

**When the platform supplies `PORT` (and `MIMO2API_PORT` is unset, which is how a
compose deployment looks) and no API keys are configured, startup fails with an
error naming `MIMO2API_API_KEYS`.**

```
MIMO2API_API_KEYS is required when the platform supplies PORT (got PORT=3000):
without a key the gateway would bind 127.0.0.1 behind the platform's proxy, where
nothing can reach it (every request a 503 and no hint in the logs). Set
MIMO2API_API_KEYS to the key your clients use
```

The three states are now distinct and each is diagnosable from one line:

| condition | behaviour |
| --- | --- |
| `MIMO2API_PORT` set, no keys (compose, local) | bind `127.0.0.1`, log says so — unchanged |
| `PORT` set, keys set (hosted, configured) | bind every interface on `PORT` — unchanged |
| `PORT` set, no keys (hosted, misconfigured) | **refuse to start**, naming the variable — new |

## Consequences

* The failure the owner saw becomes a failed deploy with a stated reason instead of
  a 503 page that never explains itself. The cost is accepting a hard exit where the
  old code limped: correct, because in that configuration the alternative is a
  process that cannot serve anything.
* **Do not "fix" this by binding `0.0.0.0` without a key.** That would publish an
  unauthenticated gateway in front of the pool — a much worse outcome than a
  deployment that stops and says why (account safety over engineering convenience).
  An operator who genuinely wants open auth on a platform sets `MIMO2API_HOST`
  explicitly, which is a deliberate act rather than a default.
* The remaining hosted unknowns are unchanged and must be probed on the platform
  itself: the platform's **egress IP** (this account's IP was risk-controlled
  before) and whether the store is reachable from there (`DS_REDIS_HOST` set but
  unusable is fatal by design, ADR 0018 — its error message is the other thing to
  look for in a platform log).
