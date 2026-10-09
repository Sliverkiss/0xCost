"""Canonical ↔ protocol shape converters (OpenAI / Responses API)."""

from __future__ import annotations

from typing import Any, Dict, List, Optional

from .models.canonical import Message, ToolCall, ToolDef, _parse_tool_arguments
from .util.ids import call_id, random_id, unix_now
from .util.tokens import resolve_usage


# ---------- Responses API ----------

def _map_responses_usage(usage: Dict[str, Any]) -> Dict[str, Any]:
    input_tokens = usage.get("prompt_tokens") or usage.get("input_tokens") or 0
    output_tokens = usage.get("completion_tokens") or usage.get("output_tokens") or 0
    total_tokens = usage.get("total_tokens") or (input_tokens + output_tokens)
    usage_out: Dict[str, Any] = {
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "total_tokens": total_tokens,
    }
    # Map cached_tokens / cache_read_input_tokens into input_tokens_details
    cached = usage.get("cached_tokens") or usage.get("cache_read_input_tokens")
    if cached:
        usage_out["input_tokens_details"] = {"cached_tokens": cached}
    # Pass through any other extra fields not already mapped
    core_keys = {
        "prompt_tokens",
        "completion_tokens",
        "total_tokens",
        "input_tokens",
        "output_tokens",
    }
    for k, v in usage.items():
        if k not in core_keys and k not in usage_out:
            usage_out[k] = v
    return usage_out


def responses_input_to_messages(body: Dict[str, Any]) -> List[Message]:
    messages: List[Message] = []
    instructions = body.get("instructions")
    if instructions:
        messages.append(Message(role="system", content=str(instructions)))

    raw_input = body.get("input")
    if isinstance(raw_input, str):
        messages.append(Message(role="user", content=raw_input))
        return messages

    for item in raw_input or []:
        if not isinstance(item, dict):
            continue
        itype = item.get("type")
        role = str(item.get("role") or "")
        if itype == "message" or role:
            role = role or "user"
            content = item.get("content")
            text = ""
            if isinstance(content, str):
                text = content
            elif isinstance(content, list):
                chunks = []
                for c in content:
                    if isinstance(c, dict) and c.get("type") in {"input_text", "output_text", "text"}:
                        chunks.append(str(c.get("text") or ""))
                    elif isinstance(c, str):
                        chunks.append(c)
                text = "\n".join(chunks)
            messages.append(Message(role=role if role in {"system", "user", "assistant"} else "user", content=text, raw=item))
        elif itype == "function_call":
            parsed = _parse_tool_arguments(item.get("arguments"))
            messages.append(
                Message(
                    role="assistant",
                    content="",
                    tool_calls=[
                        ToolCall(
                            id=str(item.get("call_id") or item.get("id") or call_id()),
                            name=str(item.get("name") or ""),
                            arguments=parsed,
                        )
                    ],
                    raw=item,
                )
            )
        elif itype == "function_call_output":
            messages.append(
                Message(
                    role="tool",
                    content=str(item.get("output") or ""),
                    tool_call_id=str(item.get("call_id") or ""),
                    raw=item,
                )
            )
    return messages


def responses_tools_to_defs(raw: Any) -> List[ToolDef]:
    defs: List[ToolDef] = []
    for item in raw or []:
        if not isinstance(item, dict):
            continue
        if item.get("type") == "function":
            name = str(item.get("name") or "")
            if not name:
                continue
            defs.append(
                ToolDef(
                    name=name,
                    description=str(item.get("description") or ""),
                    parameters=item.get("parameters") if isinstance(item.get("parameters"), dict) else {},
                    raw=item,
                )
            )
    # Also accept OpenAI chat style nested
    if not defs:
        from .models.canonical import openai_tools_to_defs

        return openai_tools_to_defs(raw)
    return defs


def build_responses_response(
    *,
    model: str,
    content: str = "",
    tool_calls: Optional[List[ToolCall]] = None,
    usage: Optional[Dict[str, int]] = None,
    prompt_text: str = "",
    legacy_map: Optional[Dict[str, str]] = None,
) -> Dict[str, Any]:
    from app.engine.xyml import responses_tool_items

    output: List[Dict[str, Any]] = []
    if content:
        output.append(
            {
                "id": f"msg_{random_id(12)}",
                "type": "message",
                "status": "completed",
                "role": "assistant",
                "content": [{"type": "output_text", "text": content}],
            }
        )
    if tool_calls:
        output.extend(responses_tool_items(tool_calls, legacy_map=legacy_map))
    status = "completed"
    resolved = resolve_usage(
        upstream=usage,
        prompt_text=prompt_text,
        completion_text=content,
    )
    usage_out = _map_responses_usage(resolved)
    return {
        "id": f"resp_{random_id(16)}",
        "object": "response",
        "created_at": unix_now(),
        "status": status,
        "model": model,
        "output": output,
        "usage": usage_out,
    }
