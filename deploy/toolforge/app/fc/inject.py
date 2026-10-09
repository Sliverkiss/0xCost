"""Prompt injection and history rendering for Path B (prompt FC)."""

from __future__ import annotations

import json
from typing import Any, Dict, List, Optional

from app.engine.xyml import (
    build_tool_instructions,
    normalize_tools,
    render_tool_call,
)

from ..models.canonical import Message, ToolDef, tool_defs_to_openai
from . import _config


def tools_for_sdk(tools: List[ToolDef]) -> List[Dict[str, Any]]:
    return normalize_tools(tool_defs_to_openai(tools))


def build_instructions(tools: List[ToolDef], *, protocol: str = "XYML") -> str:
    return build_tool_instructions(tools_for_sdk(tools), config=_config(protocol))


def _content_to_text(content: Any) -> str:
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: List[str] = []
        for item in content:
            if isinstance(item, dict):
                if item.get("type") == "text":
                    parts.append(str(item.get("text") or ""))
                elif "text" in item:
                    parts.append(str(item.get("text") or ""))
            elif isinstance(item, str):
                parts.append(item)
        return "\n".join(p for p in parts if p)
    return str(content)


def render_history_messages(
    messages: List[Message],
    *,
    protocol: str = "XYML",
) -> List[Dict[str, Any]]:
    """Flatten OpenAI-style tool history into pure chat messages for prompt FC."""
    cfg = _config(protocol)
    out: List[Dict[str, Any]] = []

    for msg in messages:
        role = msg.role
        if role == "tool":
            tool_id = msg.tool_call_id or "unknown"
            name = msg.name or ""
            body = _content_to_text(msg.content)
            header = f"[Tool Result id={tool_id}"
            if name:
                header += f" name={name}"
            header += "]"
            out.append({"role": "user", "content": f"{header}\n{body}"})
            continue

        if role == "assistant" and msg.tool_calls:
            blocks: List[str] = []
            text = _content_to_text(msg.content).strip()
            if text:
                blocks.append(text)
            for call in msg.tool_calls:
                blocks.append(
                    render_tool_call(call.name, call.arguments or {}, config=cfg)
                )
            out.append({"role": "assistant", "content": "\n".join(blocks)})
            continue

        # system / user / assistant without tools
        mapped_role = role if role in {"system", "user", "assistant"} else "user"
        content = _content_to_text(msg.content)
        if mapped_role == "system" and not content.strip():
            continue
        out.append({"role": mapped_role, "content": content})

    return out


def inject_prompt_messages(
    messages: List[Message],
    tools: List[ToolDef],
    *,
    protocol: str = "XYML",
    convert_developer_to_system: bool = True,
) -> List[Dict[str, Any]]:
    """Build upstream OpenAI chat messages for Path B."""
    instructions = build_instructions(tools, protocol=protocol)
    history = render_history_messages(messages, protocol=protocol)

    # Merge instructions into first system message or prepend.
    if history and history[0].get("role") == "system":
        history[0]["content"] = (
            str(history[0].get("content") or "").rstrip() + "\n\n" + instructions
        ).strip()
        return history

    return [{"role": "system", "content": instructions}] + history


def strip_tools_from_openai_body(body: Dict[str, Any]) -> Dict[str, Any]:
    cleaned = dict(body)
    cleaned.pop("tools", None)
    cleaned.pop("tool_choice", None)
    cleaned.pop("functions", None)
    cleaned.pop("function_call", None)
    cleaned.pop("parallel_tool_calls", None)
    return cleaned
