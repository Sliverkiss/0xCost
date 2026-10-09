"""Shared adapter helpers."""

from __future__ import annotations

from typing import Any, Dict, List, Optional

from ..models.canonical import CanonicalRequest, Message, Surface, ToolDef


def _build_canonical(
    *,
    model: str,
    messages: List[Message],
    tools: List[ToolDef],
    tool_choice: Any,
    stream: bool,
    surface: Surface,
    temperature: Optional[float],
    max_tokens: Optional[int],
    top_p: Optional[float],
    extra: Dict[str, Any],
) -> CanonicalRequest:
    """Construct a CanonicalRequest from already-resolved body parts."""
    return CanonicalRequest(
        model=model,
        messages=messages,
        tools=tools,
        tool_choice=tool_choice,
        stream=stream,
        surface=surface,
        temperature=temperature,
        max_tokens=max_tokens,
        top_p=top_p,
        extra=extra,
    )