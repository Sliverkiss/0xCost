"""Canonical request → upstream wire format (OpenAI-compat)."""

from __future__ import annotations

from typing import Any, Dict, Optional

from ..fc.inject import inject_prompt_messages, strip_tools_from_openai_body
from ..fc.parse import parse_text_to_calls
from ..fc.sanitize import sanitize_wire_body
from ..models.canonical import CanonicalRequest, CanonicalResponse, tool_defs_to_openai
from ..adapters.openai_chat import (
    _openai_messages_from_canonical,
    build_chat_completion_response,
    extract_text_and_native_calls,
)


def build_openai_wire(
    req: CanonicalRequest,
    *,
    upstream_model: str,
    protocol: str,
    convert_developer: bool,
    instructions_extra: str = "",
    sanitize: bool = True,
) -> Dict[str, Any]:
    body: Dict[str, Any] = {"model": upstream_model, "stream": req.stream}
    # `inbound_headers` is metadata for the transport, never a body field: a
    # caller that puts one in the body must not have it forwarded upstream.
    body.update(
        {
            k: v
            for k, v in (req.extra or {}).items()
            if k != "inbound_headers"
        }
    )
    if req.fc_mode == "prompt":
        messages = inject_prompt_messages(
            req.messages,
            req.tools,
            protocol=protocol,
            convert_developer_to_system=convert_developer,
        )
        if instructions_extra:
            if messages and messages[0].get("role") == "system":
                messages[0]["content"] = str(messages[0].get("content") or "") + "\n\n" + instructions_extra
            else:
                messages = [{"role": "system", "content": instructions_extra}] + messages
        body["messages"] = messages
        return sanitize_wire_body(strip_tools_from_openai_body(body)) if sanitize else strip_tools_from_openai_body(body)

    body["messages"] = _openai_messages_from_canonical(req)
    if req.fc_mode == "native" and req.tools:
        body["tools"] = tool_defs_to_openai(req.tools)
        if req.tool_choice is not None:
            body["tool_choice"] = req.tool_choice
    return sanitize_wire_body(body) if sanitize else body


def openai_result_to_canonical(
    result: Dict[str, Any],
    *,
    req: CanonicalRequest,
    protocol: str,
    strip_think: bool,
    legacy_map: Optional[Dict[str, str]] = None,
) -> CanonicalResponse:
    content, native_calls, finish, usage = extract_text_and_native_calls(
        result,
        legacy_map=legacy_map,
        tools=tool_defs_to_openai(req.tools),
    )
    if req.fc_mode == "prompt":
        calls = parse_text_to_calls(content, req.tools, protocol=protocol, strip_think=strip_think)
        if not calls and native_calls:
            calls = native_calls
        return CanonicalResponse(
            model=req.model,
            content="" if calls else content,
            tool_calls=calls,
            finish_reason="tool_calls" if calls else finish,
            usage=usage or {},
            raw=result,
        )
    return CanonicalResponse(
        model=req.model,
        content=content,
        tool_calls=native_calls,
        finish_reason=finish,
        usage=usage or {},
        raw=result,
    )


def encode_client_response(
    req: CanonicalRequest, resp: CanonicalResponse, *, prompt_text: str = "", legacy_map: Optional[Dict[str, str]] = None
) -> Dict[str, Any]:
    if req.surface == "responses":
        from ..convert import build_responses_response

        return build_responses_response(
            model=resp.model,
            content=resp.content,
            tool_calls=resp.tool_calls or None,
            usage=resp.usage,
            prompt_text=prompt_text,
            legacy_map=legacy_map,
        )
    return build_chat_completion_response(
        model=resp.model,
        content=resp.content,
        tool_calls=resp.tool_calls or None,
        finish_reason=resp.finish_reason,
        usage=resp.usage,
        prompt_text=prompt_text,
        legacy_map=legacy_map,
    )
