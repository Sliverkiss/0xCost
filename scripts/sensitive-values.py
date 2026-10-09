#!/usr/bin/env python3
"""Print the account identifiers that must never appear in the repository.

Two git-ignored sources, read at run time so the values are never written down in a
tracked file — which is the whole point of the check (docs/TDD.md §6.7):

  * `data/accounts.json` — account identifiers (phone, user id, password, tokens,
    the device identity).
  * `data/deploy.env`    — deploy credentials (DS_REDIS_TOKEN, API keys, egress
    credentials). A store credential is exactly as sensitive as an account
    password: both open the pool (ADR 0020).

One value per line; nothing is printed for a missing source, so the caller can tell
"clean" from "nothing to compare" by that alone.
"""

from __future__ import annotations

import json
import re
import sys

FIELDS = ("phone", "user_id", "id", "password", "pass_token")
TOKEN_IN_COOKIE = re.compile(r"serviceToken=\"?([^\";]+)")


# Deploy credentials that must never reach a tracked file. Prefixes, so
# MIMO2API_EGRESS_SG_PASSWORD is covered by MIMO2API_EGRESS_ (ADR 0020).
ENV_KEY_PREFIXES = ("DS_REDIS_TOKEN", "DS_REDIS_HOST", "MIMO2API_API_KEYS", "MIMO2API_EGRESS_")


def env_values(path: str) -> list[str]:
    """Values of credential keys in a docker-compose style env file."""
    try:
        with open(path, encoding="utf-8") as handle:
            lines = handle.readlines()
    except OSError:
        return []
    found: list[str] = []
    for line in lines:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key, value = key.strip(), value.strip().strip('"').strip("'")
        if value and any(key.startswith(prefix) for prefix in ENV_KEY_PREFIXES):
            found.append(value)
    return found


def values(path: str, env_path: str = "data/deploy.env") -> list[str]:
    try:
        with open(path, encoding="utf-8") as handle:
            payload = json.load(handle)
    except (OSError, json.JSONDecodeError):
        return []

    found: list[str] = []
    for account in payload.get("accounts") or []:
        if not isinstance(account, dict):
            continue
        for field in FIELDS:
            value = account.get(field)
            if value:
                found.append(str(value))
        session = account.get("session") or {}
        if isinstance(session, dict):
            if session.get("ph"):
                found.append(str(session["ph"]))
            match = TOKEN_IN_COOKIE.search(str(session.get("cookie") or ""))
            if match:
                found.append(match.group(1))
        # The device identity (deviceId/pass_ua/uLocale) is account-identifying
        # too: it must never reach a tracked file (internal/mimo/device.go).
        device = account.get("device") or {}
        if isinstance(device, dict):
            for value in device.values():
                if value:
                    found.append(str(value))
    found.extend(env_values(env_path))
    # Short values would match unrelated text; the check is for real identifiers.
    return [value for value in dict.fromkeys(found) if len(value) >= 6]


def main(argv: list[str]) -> int:
    path = argv[1] if len(argv) > 1 else "data/accounts.json"
    env_path = argv[2] if len(argv) > 2 else "data/deploy.env"
    for value in values(path, env_path):
        print(value)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
