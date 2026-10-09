"""Truncation detection and parse-error retry helpers (Phase 2)."""

from __future__ import annotations

import re
from typing import List, Optional, Tuple

from ..models.canonical import ToolCall, ToolDef
from .parse import parse_text_to_calls

_OPEN_MARKERS = (
    r"<[|:][A-Za-z0-9_]+\s*[|:]\s*tool_calls\b",
    r"<[|:][A-Za-z0-9_]+\s*[|: \t]*invoke\b",
    r"<tool_calls>",
    r"<tool_use>",
    r'"tool_calls"\s*:',
    r"function\.name\s*:",
)

_CLOSE_MARKERS = (
    r"</[|:][A-Za-z0-9_]+\s*[|:]?\s*(?:invoke|tool_calls|tool_use)\b",
    r"</[|:][A-Za-z0-9_]+\s*[|:]?\s*>",
    r"</tool_calls>",
    r"</tool_use>",
)

_OPEN_RES = tuple(re.compile(p, re.IGNORECASE) for p in _OPEN_MARKERS)
_CLOSE_RES = tuple(re.compile(p, re.IGNORECASE) for p in _CLOSE_MARKERS)


def is_tool_call_truncated(text: str) -> bool:
    if not text:
        return False
    has_open = any(regex.search(text) for regex in _OPEN_RES)
    if not has_open:
        return False
    has_close = any(regex.search(text) for regex in _CLOSE_RES)
    if has_close:
        # Still truncated if open count > close-ish for JSON
        if text.rstrip().endswith((",", ":", "{", "[")):
            return True
        return False
    return True


def build_retry_user_message(
    *,
    original_output: str,
    reason: str = "parse_failed",
) -> str:
    snippet = (original_output or "")[-2000:]
    if reason == "truncated":
        return (
            "Your previous tool-call output was truncated mid-envelope. "
            "Re-emit the COMPLETE tool call protocol block only, with no extra commentary.\n\n"
            f"Previous output (truncated):\n{snippet}"
        )
    return (
        "Your previous tool-call output could not be parsed. "
        "Re-emit a valid tool call protocol block for the listed tools only.\n\n"
        f"Previous output:\n{snippet}"
    )


def parse_with_recovery_hint(
    text: str,
    tools: List[ToolDef],
    *,
    protocol: str = "XYML",
    strip_think: bool = True,
) -> Tuple[List[ToolCall], Optional[str]]:
    """Parse once; if empty and looks truncated, return retry reason."""
    calls = parse_text_to_calls(text, tools, protocol=protocol, strip_think=strip_think)
    if calls:
        return calls, None
    if is_tool_call_truncated(text):
        return [], "truncated"
    # Non-empty model output that looks like it tried tools
    if text and any(regex.search(text) for regex in _OPEN_RES):
        return [], "parse_failed"
    return [], None
