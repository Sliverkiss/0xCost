"""Parse model output into normalized tool calls (Path B)."""

from __future__ import annotations

import re
from typing import Any, Dict, List, Optional

from app.engine.xyml import (
    ToolSieve,
    openai_tool_calls,
    parse_tool_calls,
)

from ..models.canonical import ToolCall, ToolDef
from . import _config
from .inject import tools_for_sdk

_THINK_RE = re.compile(r"<think>[\s\S]*?</think>", re.IGNORECASE)


def strip_think_tags(text: str) -> str:
    if not text:
        return ""
    return _THINK_RE.sub("", text)


def parsed_to_toolcall(item: Any) -> ToolCall:
    """Convert a ParsedToolCall or dict-like item into a ToolCall (M6).

    Handles both attribute-style (ParsedToolCall) and mapping-style items.
    """
    name = getattr(item, "name", None) or (item.get("name") if isinstance(item, dict) else "")
    call_id = getattr(item, "id", None) or (item.get("id") if isinstance(item, dict) else "")
    arguments = getattr(item, "input", None)
    if arguments is None and isinstance(item, dict):
        arguments = item.get("input") or item.get("arguments") or {}
    if not isinstance(arguments, dict):
        arguments = {"value": arguments}
    return ToolCall(id=str(call_id or ""), name=str(name or ""), arguments=arguments)


def parsed_calls_to_toolcalls(calls_raw: List[Any]) -> List[ToolCall]:
    """Map a raw parsed-call list into normalized ToolCalls (dedup helper)."""
    return [parsed_to_toolcall(item) for item in calls_raw]


def parse_text_to_calls(
    text: str,
    tools: List[ToolDef],
    *,
    protocol: str = "XYML",
    strip_think: bool = True,
) -> List[ToolCall]:
    source = strip_think_tags(text) if strip_think else (text or "")
    sdk_tools = tools_for_sdk(tools)
    parsed = parse_tool_calls(source, sdk_tools, config=_config(protocol))
    return parsed_calls_to_toolcalls(parsed)


def to_openai_tool_calls(
    calls: List[ToolCall], legacy_map: Optional[Dict[str, str]] = None
) -> List[Dict[str, Any]]:
    return openai_tool_calls(calls, legacy_map=legacy_map)


def create_sieve(tools: List[ToolDef], *, protocol: str = "XYML", hold_length: int = 96) -> ToolSieve:
    return ToolSieve(tools_for_sdk(tools), config=_config(protocol), hold_length=hold_length)
