#!/usr/bin/env bash
# OxCost pre-merge gates (R1W1 A4). Every merge to main must pass this
# script in full. Run from the repository root (or anywhere — the script
# cds to the repo root itself).
#
# Gates:
#   1. cargo fmt --check
#   2. cargo clippy -D warnings
#   3. cargo clippy -W clippy::pedantic (counted against the baseline in
#      docs/R1W1_NOTES.md; regressions beyond the baseline fail)
#   4. cargo test --workspace
#   5. cargo audit (RUSTSEC advisories)
#   6. cargo deny check (licenses AGPL-compatible, duplicates, bans)
#   7. gitleaks detect (secrets, full history)
#
# Exit: nonzero if any gate fails. A summary table prints at the end.

set -euo pipefail

cd "$(dirname "$0")/.."
source "${HOME}/.cargo/env" 2>/dev/null || true

declare -a GATE_NAMES GATE_STATUS GATE_NOTES
FAILED=0

record() {           # record <name> <status> <notes>
    GATE_NAMES+=("$1")
    GATE_STATUS+=("$2")
    GATE_NOTES+=("$3")
    if [[ "$2" == "FAIL" ]]; then
        FAILED=1
    fi
}

echo "== OxCost pre-merge gates =="

# ---- 1. fmt ----
if cargo fmt --check >/tmp/gate-out.log 2>&1; then
    record "fmt" "PASS" ""
else
    record "fmt" "FAIL" "run cargo fmt"
    tail -20 /tmp/gate-out.log >&2
fi

# ---- 2. clippy -D warnings ----
if cargo clippy --workspace --all-targets -- -D warnings >/tmp/gate-out.log 2>&1; then
    record "clippy(default)" "PASS" ""
else
    record "clippy(default)" "FAIL" "-D warnings"
    tail -30 /tmp/gate-out.log >&2
fi

# ---- 3. clippy pedantic (counted against baseline) ----
# Pedantic runs as a warning pass; the wave-1 baseline (existing lint
# count) is recorded in docs/R1W1_NOTES.md. Counts above the baseline
# fail the gate so new code stays clean; pre-existing lints are not
# mass-rewritten this wave (spec A4 decision).
cargo clippy --workspace --all-targets -- -W clippy::pedantic \
    >/tmp/gate-pedantic.log 2>&1 || true
PEDANTIC_COUNT=$(grep -c '^warning:' /tmp/gate-pedantic.log || true)
BASELINE=$(grep -oP 'pedantic baseline: \K[0-9]+' docs/R1W1_NOTES.md 2>/dev/null | head -1)
BASELINE=${BASELINE:-none}
if [[ "$BASELINE" != "none" && "$PEDANTIC_COUNT" -gt "$BASELINE" ]]; then
    record "clippy(pedantic)" "FAIL" "$PEDANTIC_COUNT lints > baseline $BASELINE"
    tail -10 /tmp/gate-pedantic.log >&2
else
    record "clippy(pedantic)" "PASS" "baseline $BASELINE, now $PEDANTIC_COUNT"
fi

# ---- 4. tests ----
if cargo test --workspace >/tmp/gate-out.log 2>&1; then
    record "test" "PASS" ""
else
    record "test" "FAIL" ""
    tail -30 /tmp/gate-out.log >&2
fi

# ---- 5. cargo audit ----
if ! command -v cargo-audit >/dev/null 2>&1; then
    record "audit" "SKIP" "cargo-audit not installed"
elif cargo audit >/tmp/gate-out.log 2>&1; then
    record "audit" "PASS" ""
else
    record "audit" "FAIL" "RUSTSEC advisories"
    tail -30 /tmp/gate-out.log >&2
fi

# ---- 6. cargo deny ----
if ! command -v cargo-deny >/dev/null 2>&1; then
    record "deny" "SKIP" "cargo-deny not installed"
elif cargo deny check >/tmp/gate-out.log 2>&1; then
    record "deny" "PASS" ""
else
    record "deny" "FAIL" "licenses/duplicates/bans"
    tail -30 /tmp/gate-out.log >&2
fi

# ---- 7. gitleaks ----
if ! command -v gitleaks >/dev/null 2>&1; then
    record "gitleaks" "SKIP" "gitleaks not installed"
elif gitleaks detect --no-banner --source . >/tmp/gate-out.log 2>&1; then
    record "gitleaks" "PASS" ""
else
    record "gitleaks" "FAIL" "secrets detected"
    tail -30 /tmp/gate-out.log >&2
fi

# ---- summary ----
echo
echo "== gate summary =="
printf '%-18s %-6s %s\n' "GATE" "STATE" "NOTES"
printf '%-18s %-6s %s\n' "----" "-----" "-----"
for i in "${!GATE_NAMES[@]}"; do
    printf '%-18s %-6s %s\n' "${GATE_NAMES[$i]}" "${GATE_STATUS[$i]}" "${GATE_NOTES[$i]}"
done
echo

exit "$FAILED"
