# Decisions

Architecture decision records for mimo2api. One file per decision, numbered,
never rewritten — a reversal gets a new record that supersedes the old one.

Each record states the context, the decision, the consequences (including the
cost we accepted) and the evidence that drove it. Protocol claims point at
`docs/SDD.md`, `docs/TDD.md`, `docs/login-chain-reverse.md` or live evidence
rather than at chat history.

| # | Decision | Status |
|---|---|---|
| [0001](0001-cross-region-consent-policy.md) | Auto-agree the cross-region consent, with an explicit operator override | accepted |
| [0002](0002-renew-before-password-login.md) | Renew from the passToken before falling back to the password login | accepted |
| [0003](0003-session-from-whole-landing-chain.md) | Compose the session from the whole landing chain's cookies | accepted |
| [0004](0004-no-auto-solve-human-gates.md) | Never auto-solve a human gate; hand it to the operator | accepted |
| [0005](0005-exact-byte-protocol-fixtures.md) | Protocol fixtures live as committed exact-byte files | accepted |
| [0006](0006-acceptance-gates-and-coverage-floors.md) | Acceptance runs through Makefile gates with per-package coverage floors | accepted |
| [0007](0007-search-results-parsed-per-field.md) | Search results are parsed per field from live evidence; missing results are not a failure | accepted |
| [0008](0008-long-speech-split-and-rejoined.md) | Long speech is split on sentence boundaries and rejoined, never truncated | superseded by 0010 |
| [0009](0009-sessions-are-streaming-only-and-verified.md) | Sessions are streaming-only, explicitly identified when possible, and verified before chaining | superseded by 0017 |
| [0010](0010-speech-length-is-a-refused-limit.md) | Long speech is a refused limit, not a split | accepted (supersedes 0008) |
| [0011](0011-superseded-models-are-resolved-not-advertised.md) | A superseded model id is resolved, not advertised | accepted |
| [0012](0012-function-calling-through-a-middleware.md) | Function calling lives behind a middleware, reached through one path | superseded by 0016 |
| [0013](0013-device-identity-and-sms-dispatch.md) | The device identity is state (from the login page); an SMS code is a single-use operator budget | accepted |
| [0014](0014-self-healing-credentials.md) | Credentials self-heal: 12 h keepalive, async password re-login, no SMS without consent | accepted |
| [0015](0015-request-size-limits.md) | Request size limits are measured, never guessed (102,400 chars / 50,000 on fastchat) | accepted |
| [0016](0016-no-tools-non-streaming.md) | No function calling, and chat is non-streaming | accepted (streaming part superseded by 0017) |
| [0017](0017-no-streaming-no-session-store.md) | Chat has no streaming path and no session store (deleted, not switched off) | accepted (supersedes 0016 §2 and 0009) |
| [0018](0018-account-store-backends-and-port.md) | The account store is pluggable (file or Upstash); port 3000, platform `PORT` honoured, nixpacks build | accepted |
