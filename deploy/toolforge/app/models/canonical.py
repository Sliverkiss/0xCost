"""Protocol-neutral request/response models."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Dict, List, Literal, Optional, Union

Surface = Literal["openai", "responses"]
FCMode = Literal["native", "prompt", "auto"]
ResolvedFCMode = Literal["native", "prompt", "passthrough"]


@dataclass
class ToolCall:
    id: str
    name: str
    arguments: Dict[str, Any] = field(default_factory=dict)


@dataclass
class ToolDef:
    name: str
    description: str = ""
    parameters: Dict[str, Any] = field(default_factory=dict)
    raw: Dict[str, Any] = field(default_factory=dict)


@dataclass
class Message:
    role: str
    content: Any = ""
    name: Optional[str] = None
    tool_call_id: Optional[str] = None
    tool_calls: List[ToolCall] = field(default_factory=list)
    # For assistant/tool history already in OpenAI shape
    raw: Dict[str, Any] = field(default_factory=dict)


@dataclass
class CanonicalRequest:
    model: str
    messages: List[Message]
    tools: List[ToolDef] = field(default_factory=list)
    tool_choice: Any = "auto"
    stream: bool = False
    surface: Surface = "openai"
    temperature: Optional[float] = None
    max_tokens: Optional[int] = None
    top_p: Optional[float] = None
    extra: Dict[str, Any] = field(default_factory=dict)
    # Headers the caller sent, for key_passthrough / passthrough_headers. A
    # dedicated field on purpose: `extra` is merged into the upstream request
    # body (wire.build_openai_wire), so putting headers there would ship the
    # caller's credentials to the upstream as JSON.
    inbound_headers: Dict[str, str] = field(default_factory=dict)
    # Resolved at runtime
    fc_mode: ResolvedFCMode = "passthrough"
    upstream_model: str = ""
    request_fc_override: Optional[str] = None  # force_prompt | prefer_native | auto


@dataclass
class CanonicalResponse:
    model: str
    content: str = ""
    tool_calls: List[ToolCall] = field(default_factory=list)
    finish_reason: str = "stop"
    usage: Dict[str, int] = field(default_factory=dict)
    raw: Dict[str, Any] = field(default_factory=dict)


def tool_defs_to_openai(tools: List[ToolDef]) -> List[Dict[str, Any]]:
    out: List[Dict[str, Any]] = []
    for tool in tools:
        if tool.raw:
            out.append(tool.raw)
            continue
        out.append(
            {
                "type": "function",
                "function": {
                    "name": tool.name,
                    "description": tool.description,
                    "parameters": tool.parameters or {"type": "object", "properties": {}},
                },
            }
        )
    return out


def openai_tools_to_defs(raw_tools: Any) -> List[ToolDef]:
    if not raw_tools:
        return []
    defs: List[ToolDef] = []
    for item in raw_tools:
        if not isinstance(item, dict):
            continue
        if item.get("type") == "function" and isinstance(item.get("function"), dict):
            fn = item["function"]
            defs.append(
                ToolDef(
                    name=str(fn.get("name") or ""),
                    description=str(fn.get("description") or ""),
                    parameters=fn.get("parameters") if isinstance(fn.get("parameters"), dict) else {},
                    raw=item,
                )
            )
        elif item.get("name"):
            params = item.get("parameters") or item.get("input_schema") or {}
            defs.append(
                ToolDef(
                    name=str(item.get("name") or ""),
                    description=str(item.get("description") or ""),
                    parameters=params if isinstance(params, dict) else {},
                    raw=item,
                )
            )
    return [d for d in defs if d.name]


def _parse_tool_arguments(args: Any) -> Dict[str, Any]:
    if isinstance(args, dict):
        return args
    if isinstance(args, str) and args.strip():
        try:
            value = json.loads(args)
            return value if isinstance(value, dict) else {"value": value}
        except Exception:
            return {"_raw": args}
    return {}


def openai_messages_to_canonical(raw_messages: Any, *, convert_developer: bool = True) -> List[Message]:
    messages: List[Message] = []
    for item in raw_messages or []:
        if not isinstance(item, dict):
            continue
        role = str(item.get("role") or "user")
        if convert_developer and role == "developer":
            role = "system"
        tool_calls: List[ToolCall] = []
        for tc in item.get("tool_calls") or []:
            if not isinstance(tc, dict):
                continue
            fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
            tool_calls.append(
                ToolCall(
                    id=str(tc.get("id") or ""),
                    name=str(fn.get("name") or ""),
                    arguments=_parse_tool_arguments(fn.get("arguments")),
                )
            )
        messages.append(
            Message(
                role=role,
                content=item.get("content", ""),
                name=item.get("name"),
                tool_call_id=item.get("tool_call_id"),
                tool_calls=tool_calls,
                raw=item,
            )
        )
    return messages
