# 0020. Account management is a CLI against the configured store, not a file edit or a Redis console

Date: 2026-10-09
Status: accepted
Related: ADR 0018 (pluggable account store: file or Upstash), ADR 0013 (device
anchor), ADR 0014 (self-healing credentials), ADR 0017 (no session store)

## Context

The owner asked how to **manage** accounts once the store is Upstash rather than
`data/accounts.json`: an Upstash endpoint (`https://<db>.upstash.io/`) was handed over
with the question "看看怎么用upstash来进行管理而不是account.json".

ADR 0018 made the store pluggable, and the deployed container had already been
observed running on Upstash (accounts loaded, real chat answered, a cleared
session re-renewed and written back). What was missing was the operational half:

1. **There is no file to edit.** Everything the file offered — look at the pool,
   fix a bad row, turn an account off, drop a dead one — had no counterpart. The
   only options were a raw Redis console (which means holding a write-capable
   credential in an interactive session, and knowing the key layout by hand) or
   redeploying with a new seed file.
2. **The store could not be mutated.** `Store` exposed `Accounts`/`Get` (read),
   `Update` (replace an existing row) and `ReplaceAll` (whole-set rewrite, the
   import path). `Update` refuses to invent an entry, so "add an account" and
   "remove an account" were impossible — the two operations an operator reaches
   for most.
3. **The backend interface had no delete.** `Backend` was `Load` + `SaveAccount`
   + `Close`: an id could be overwritten but never removed, on either store.

## Decision

**`mimo2api -accounts <command>` is the management surface, and it works on
whichever store the environment selects.** Same binary, same env as the server:
`DS_REDIS_HOST` set → Upstash, otherwise `MIMO2API_ACCOUNTS_FILE`. It opens the
store through the exact selection the server uses (`openStore`), so "which
backend am I editing?" is never a guess — each command prints the backend it
acted on.

```
mimo2api -accounts list                    # id, enabled, egress, credential/session/device state
mimo2api -accounts show <id>               # full record, secrets as presence/length
mimo2api -accounts enable <id>
mimo2api -accounts disable <id> [reason]
mimo2api -accounts set-egress <id> <label>
mimo2api -accounts rm <id>
mimo2api -accounts import <accounts.json>  # upsert every row into the active store
mimo2api -accounts export <file>           # whole store → 0600 file (backup)
mimo2api -accounts verify                  # connect, count, report
```

Supporting changes:

* `Store.Put` (upsert: add or replace) and `Store.Remove` (delete, reporting an
  unknown id as `ErrNotFound`), both writing through to the backend.
* `Backend.DeleteAccount` on both stores. Redis deletes the index member first,
  then the record key, so an interrupted delete leaves an invisible key rather
  than a dangling index entry — `Load` iterates the index. The file backend
  rewrites atomically through the same temp-file + rename path as a save.
* `-accounts import` supersedes `-import-redis`, which only ever worked when
  `DS_REDIS_HOST` was set; `import` is backend-agnostic ("load these into
  Upstash" and "restore this dump into the file" are the same command). The old
  flag stays for compatibility.
* `scripts/sensitive-values.py` now also reads the git-ignored `data/deploy.env`,
  so a **store credential** (DS_REDIS_TOKEN, API keys, egress credentials) is
  scanned for exactly like an account password. Both open the pool.
* `docker-compose.yml` passes `DS_REDIS_HOST`/`DS_REDIS_TOKEN`/`DS_REDIS_CA_FILE`
  into the container. `--env-file` only substitutes variables *into* the compose
  file — a container does not inherit the operator's shell, and without these
  lines a deploy silently kept using the file backend while the operator believed
  Upstash was in use.

## Consequences

* **No new HTTP surface.** Management is a local command, not an admin endpoint:
  the gateway still exposes only `/healthz`, `/v1/*` and the four existing
  `/v1/admin/*` routes (pool view, OTP pending/submit, consent). A write-capable
  account API reachable over the network is a much larger risk than a binary the
  operator already has.
* **Membership and enable/disable changes apply at the next start.** The store is
  read once at boot by design (ADR 0018): the request path must not pay a Redis
  round-trip per request, and the in-flight pool reads tickets from memory. The
  *runtime* mutations that matter (session renewal, cooldown, credential
  rotation) already write through, so `-accounts` is for the deliberate,
  occasional changes — after which the operator restarts the container, or uses
  the existing runtime admin routes for the human gate.
* **`accounts.json` keeps a role.** It is the import source and the export
  target: a hosted deployment with no volume (Belmo) can be seeded from a local
  file and backed up to one, and a deployment can be moved between stores with
  `export` → `import`.
* **Secrets stay out of output.** `list`/`show` print presence and ages, never
  values: an operator reading a terminal in a shared space must not be handed a
  session cookie. `export` writes mode 0600 and says so.
* Verified live against real Upstash: `verify`/`list`/`show` read the record,
  `disable`/`enable` round-trips wrote through and were read back, `export`
  produced a 0600 dump (immediately deleted), and the deployed container was
  switched to the Upstash store and answered a real chat.
