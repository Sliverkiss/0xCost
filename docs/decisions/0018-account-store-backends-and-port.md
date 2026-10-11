# 0018. The account store is pluggable (file or Upstash), and the service is port-3000 pure Go

Date: 2026-10-09
Status: accepted
Related: ADR 0015 (measured limits), ADR 0017 (no streaming, no session store)

## Context

The deployment target changed. Until now the gateway ran as one container on this
host with `./data` mounted for `accounts.json`; the next target is a hosted
platform (Belmo, nixpacks-based) where:

- the file system is ephemeral — a redeploy or restart must not cost a re-login of
  the whole pool, and the credentials have to live somewhere durable;
- the platform injects its own `PORT` and expects the app to bind it;
- nothing else about the app may need a mount, a sidecar or a shell.

The reference deployment on the same platform (`Sliverkiss/simple-chat`) already
solved exactly this: an accounts store with two implementations — the JSON file by
default, Redis (Upstash) when `DS_REDIS_HOST` is set — behind one interface, with
the runtime working from memory and writing through. Requested by the owner:
"我们的账号存储应该可以对接 upstash，参考 Sliverkiss/simple-chat，把文件位置都处理一下…
toml 也要写好来，纯 go 服务，然后端口改成 3000".

## Decision

1. **The account store becomes a backend** (`internal/accounts/backend.go`):
   `Load` once at startup, `SaveAccount` on every mutation. The Store keeps the
   live set in memory exactly as before, so no request path ever reads storage.
   - `FileBackend` (default): `MIMO2API_ACCOUNTS_FILE`, mode 0600, atomic
     temp+fsync+rename, **directory created on demand**.
   - `RedisBackend` (Upstash): `DS_REDIS_HOST` (+ `DS_REDIS_TOKEN`), one JSON key
     per account (`mimo2api:accounts:<id>`) plus an index set.
2. **The variable names are the reference deployment's, unprefixed**
   (`DS_REDIS_HOST`, `DS_REDIS_TOKEN`, `DS_REDIS_INSECURE`, `DS_REDIS_CA_FILE`), so one Upstash
   instance can serve both projects; the rest of the config stays
   `MIMO2API_`-prefixed.
3. **A configured-but-unusable Redis is fatal at startup.** Never a silent
   fallback to an empty file: the operator named a source of truth, and booting
   on the fallback would serve nobody while looking healthy.
4. **The Redis client is ours** (`internal/accounts/redis.go`): a ~200-line RESP2
   implementation (TCP/TLS, AUTH, PING, GET/SET/SADD/SREM/SMEMBERS) so the binary
   stays dependency-free — no go-redis in a project that is otherwise stdlib-only.
   It refuses a plaintext `redis://` URL to a non-loopback host, because account
   passwords and session cookies live in that store.
5. **`-import-redis <file>` seeds a hosted pool** from a local accounts file and
   exits, then verifies by loading the store back — the external write is read
   back rather than trusted.
6. **Port 3000, and the platform's `PORT` wins over the default.** Resolution:
   `MIMO2API_PORT` → `PORT` → 3000. The Dockerfile, compose mapping and docs all
   move to 3000 so the container and the hosted build agree instead of mapping two
   different numbers.
7. **`nixpacks.toml` is committed** (`providers = ["go"]`, one `go build` of
   `./cmd/mimo2api`, `./mimo2api` as the start command) so the hosted build needs
   no dashboard configuration.
8. **File locations are all either configurable or unnecessary**: the accounts
   path is configurable with a **relative default** (`data/accounts.json`) and its
   directory is created on demand; the only other file-touching binaries are the
   diagnostics (`cmd/stsprobe`, `cmd/loginprobe`, `cmd/attachprobe`), which take
   their path as an argument and are not part of the service.

## Consequences

- **A hosted deploy needs no volume**: with the Upstash pair set, the binary
  starts, loads the pool from Redis and writes logins/refreshes back. Without it,
  the file backend behaves as before (and now works from a bare directory).
- **The store surface is testable without Redis**: the RESP client is exercised
  against an in-test RESP server, and the store's write-through semantics against
  a counting backend — including the "backend write fails, the in-memory value
  stays correct" rule.
- **Cost accepted**: one more code path to keep correct (a hand-written protocol
  client) and a store that can now be misconfigured — mitigated by failing fast
  at startup and by refusing plaintext off-loopback.
- **`accounts.json` stops being the only answer to "where do the credentials
  live"**, so the docs must name both backends wherever durability is discussed.
  The deployed container on this host keeps the file backend; nothing about the
  current deployment changes until `DS_REDIS_HOST` is set for it.
- **The session store (ADR 0017) is not involved**: this is credential and
  runtime-state persistence, not conversation state — nothing about a request is
  stored here.
