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
