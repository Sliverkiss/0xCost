<div align="center">

# mimo2api

**An OpenAI-compatible gateway for Xiaomi MiMo Studio.**

One static binary. One key. Standard OpenAI requests in, standard OpenAI responses out.

[![Telegram](https://img.shields.io/badge/Telegram-sliverkiss__blog-2AABEE?style=flat-square&logo=telegram)](https://t.me/sliverkiss_blog)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![Docker](https://img.shields.io/badge/Docker-distroless-2496ED?style=flat-square&logo=docker)](Dockerfile)
[![License: Non-Commercial](https://img.shields.io/badge/License-Non--Commercial-red?style=flat-square)](LICENSE)

*Point any OpenAI client at it and it answers from a pool of MiMo Studio accounts.*

</div>

---

## ✨ Overview

- 🗣️ **`POST /v1/chat/completions`** — OpenAI protocol; one JSON body, or an SSE stream with `"stream": true`
- 📋 **`GET /v1/models`** — the models the account can actually reach, plus `GET /v1/models/{id}`
- 🧠 **Deep thinking** — reasoning returned as `reasoning_content` (streamed as `delta.reasoning_content`)
- 🖼️ **Image understanding** — `image_url` content parts (http(s) URLs and base64 data URLs)
- 🔉 **Audio** — `POST /v1/audio/speech`, `POST /v1/audio/transcriptions`, `GET /v1/audio/voices`
- 🔄 **Account pool** — EWMA-latency selection with round-robin fairness inside the best band; a lease names the account one request talks to. Upstream failures pass through verbatim (no cooldown/backoff)
- 💾 **Account store: file or Upstash Redis** — the same binary, no volume needed with Redis
- 🩺 **`GET /healthz`** — plus an operator surface (`/v1/admin/pool`, OTP and consent endpoints)
- 📦 **Single static binary** — distroless, non-root, no shell, no runtime dependencies
- 🚫 **Stateless by design** — every request replays its history upstream; no session, no server-side state
- ⛔ **No function calling** — a request that still carries `tools` is answered normally, with the fact reported in `X-Mimo-Warnings`

## 🚀 Deploy

### Requirements

- Docker with Compose v2
- One or more MiMo Studio accounts (the first login asks for its SMS verification once)
- A reverse proxy for TLS if you serve it publicly

### 1. Accounts

`data/accounts.json` — one entry per account, or point `DS_REDIS_*` at a store instead (see below):

```json
{"accounts": [{"mobile": "1xxxxxxxxxx", "password": "your-password"}]}
```

### 2. Key

The gateway authenticates with one key, kept in an untracked env file:

```bash
mkdir -p data
printf 'MIMO2API_API_KEYS=%s\n' "your-gateway-key" > data/deploy.env
```

### 3. Run

```bash
chown 65532:65532 data data/accounts.json && chmod 750 data && chmod 600 data/accounts.json
docker compose --env-file data/deploy.env up -d --build --force-recreate
```

The container runs as distroless `nonroot` (uid 65532), so the accounts file needs that
ownership change — skipping it is the classic first-boot failure
(`open /data/accounts.json: permission denied`, then a restart loop).

Compose publishes **`127.0.0.1:21223`** only. TLS and the public hostname belong to the
reverse proxy in front (e.g. Caddy terminating `:443` and proxying to `127.0.0.1:21223):
an API port exposed to the internet is an exposed key.

If the first `up` reports `container mimo2api is unhealthy`, run `up -d` once more — a cold
start can outlast the health window's `start_period`. Confirm the port stays loopback-only
with `ss -ltn | grep 21223` (want `127.0.0.1`).

### 4. Verify

```bash
curl http://localhost:21223/healthz                              # {"status":"ok", ...}
curl http://localhost:21223/v1/models -H "Authorization: Bearer your-gateway-key"
curl http://localhost:21223/v1/chat/completions \
  -H "Authorization: Bearer your-gateway-key" -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}]}'
```

### Hosted platform (no durable disk)

Set `DS_REDIS_HOST` + `DS_REDIS_TOKEN` (e.g. Upstash) and the accounts live in the store
instead of a volume — the same image, and `accounts.json` is never read:

```bash
DS_REDIS_HOST=https://your-db.upstash.io
DS_REDIS_TOKEN=your-upstash-token
```

## ⚙️ Configuration

Handed to the container by the compose `environment:` block — `--env-file` alone only fills
that block, it does not reach the process.

| Env | Default | Purpose |
|---|---|---|
| `MIMO2API_API_KEYS` | unset | gateway key(s); comma-separated, accepted as `Authorization: Bearer` |
| `MIMO2API_PORT` | `21223` | listen port inside the container |
| `MIMO2API_ACCOUNTS_FILE` | `data/accounts.json` | account file when no Redis store is configured |
| `DS_REDIS_HOST` | unset | Upstash/Redis account store (portal URL, host, or a `rediss://` string) |
| `DS_REDIS_TOKEN` | unset | token for that store |
| `DS_REDIS_CA_FILE` | unset | CA bundle when the store uses TLS |
| `MIMO2API_DEFAULT_MODEL` | `mimo-v2.6-pro` | model used when a request omits `model` |
| `MIMO2API_LOG_LEVEL` | `info` | `debug` also logs upstream error bodies |
| `MIMO2API_REQUEST_TIMEOUT_MS` | `120000` | client-facing deadline for one turn |
| `MIMO2API_LEASE_TTL_S` | `120` | per-turn upstream silence window — a turn that goes quiet longer than this is cut |
| `MIMO2API_OTP_AUTO` | `false` | never submit a verification code on its own; a human gate stays human |

The full list with defaults lives in `internal/config/config.go`.

## 📄 License

**Non-commercial.** Personal and non-commercial use, modification and redistribution are
allowed with attribution; **any commercial use requires prior written permission** from the
copyright holder. See [LICENSE](LICENSE).

---

<div align="center">

[Telegram channel](https://t.me/sliverkiss_blog)

</div>
