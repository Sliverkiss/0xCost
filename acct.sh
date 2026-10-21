#!/usr/bin/env bash
# acct.sh — identity-pool operator CLI wrapper.
#
# Usage:
#   ./acct.sh list                 list identities and counters
#   ./acct.sh acquire [n]          acquire one or n fresh identities
#   ./acct.sh retire <uid>         discard an identity from the running pool
#   ./acct.sh refill               top the virgin stock up to warm_size
#   ./acct.sh export               print the raw pool state (contains tokens)
#
# Mutations need admin.enabled=true and a non-empty api_key in config.json.
# Extra flags pass through, e.g. ./acct.sh -server http://127.0.0.1:7988 list
set -euo pipefail
cd "$(dirname "$0")"
exec go run ./cmd/acct "$@"
