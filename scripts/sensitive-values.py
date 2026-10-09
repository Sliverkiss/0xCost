#!/usr/bin/env python3
"""Print the account identifiers that must never appear in the repository.

Read from the git-ignored `data/accounts.json` so the values are never written down
in a tracked file — which is the whole point of the check (docs/TDD.md §6.7). One
value per line; nothing is printed when the file is missing, so the caller can tell
"clean" from "nothing to compare" by that alone.
"""

from __future__ import annotations

import json
import re
import sys

FIELDS = ("phone", "user_id", "id", "password", "pass_token")
TOKEN_IN_COOKIE = re.compile(r"serviceToken=\"?([^\";]+)")


def values(path: str) -> list[str]:
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
    # Short values would match unrelated text; the check is for real identifiers.
    return [value for value in dict.fromkeys(found) if len(value) >= 6]


def main(argv: list[str]) -> int:
    path = argv[1] if len(argv) > 1 else "data/accounts.json"
    for value in values(path):
        print(value)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
