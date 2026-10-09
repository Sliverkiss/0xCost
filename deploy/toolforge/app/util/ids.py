"""Compact ID helpers."""

from __future__ import annotations

import secrets
import time


def random_id(length: int = 12) -> str:  # noqa: D401
    alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
    return "".join(secrets.choice(alphabet) for _ in range(length))


def completion_id() -> str:
    return f"chatcmpl-{random_id(24)}"


def call_id() -> str:
    return f"call_{random_id(12)}"


def unix_now() -> int:
    return int(time.time())
