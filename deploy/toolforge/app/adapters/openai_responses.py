"""OpenAI Responses API client surface — non-stream + true SSE stream."""

from __future__ import annotations

from typing import Any, Dict, Optional

from fastapi import HTTPException

from ..config import AppConfig
from ..convert import responses_input_to_messages, responses_tools_to_defs
from ..engine.orchestrator import handle_canonical
from ..models.canonical import CanonicalRequest
from ..upstream.router import UpstreamRouter
from . import _build_canonical


def body_to_canonical(body: Dict[str, Any]) -> CanonicalRequest:
    model = str(body.get("model") or "").strip()
    if not model:
        raise HTTPException(status_code=400, detail="model is required")
    messages = responses_input_to_messages(body)
    tools = responses_tools_to_defs(body.get("tools") or [])
    return _build_canonical(
        model=model,
        messages=messages,
        tools=tools,
        tool_choice=body.get("tool_choice", "auto"),
        stream=bool(body.get("stream")),
        surface="responses",
        temperature=body.get("temperature"),
        max_tokens=body.get("max_output_tokens") or body.get("max_tokens"),
        top_p=body.get("top_p"),
        extra={k: body[k] for k in ("metadata", "store", "user") if k in body},
    )


async def handle_responses(
    body: Dict[str, Any],
    *,
    config: AppConfig,
    router: UpstreamRouter,
    fc_header: Optional[str] = None,
    inbound_headers: Optional[Dict[str, str]] = None,
) -> Any:
    req = body_to_canonical(body)
    if inbound_headers:
        req.inbound_headers = dict(inbound_headers)
    return await handle_canonical(req, config=config, router=router, fc_header=fc_header)
