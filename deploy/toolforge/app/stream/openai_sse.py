"""Stable OpenAI chat.completion streaming (native + prompt FC)."""

from __future__ import annotations

import json
from typing import Any, AsyncIterator, Dict, List, Optional

from ..engine.xyml import _filter_input_for_tool, canonicalize_response_tool_name, coerce_tool_input
from ..fc.parse import create_sieve, parse_text_to_calls, parsed_calls_to_toolcalls, to_openai_tool_calls
from ..middleware_log import REQUEST_ID, _write
from ..models.canonical import ToolCall, ToolDef
from ..util.ids import completion_id, unix_now
from ..util.sse import done_frame, format_sse, iter_sse_events
from ..util.tokens import resolve_usage


def _log_upstream_text(full_text_parts: List[str]) -> None:
    """Persist the model's original text output (incl. XYML markup consumed by sieve)."""
    rid = REQUEST_ID.get("")
    if rid and full_text_parts:
        _write("upstream", rid, "".join(full_text_parts))


def _chunk(
    *,
    model: str,
    delta: Dict[str, Any],
    finish_reason: Optional[str] = None,
    chunk_id: Optional[str] = None,
    usage: Optional[Dict[str, int]] = None,
) -> Dict[str, Any]:
    chunk = {
        "id": chunk_id or completion_id(),
        "object": "chat.completion.chunk",
        "created": unix_now(),
        "model": model,
        "choices": [
            {
                "index": 0,
                "delta": delta,
                "finish_reason": finish_reason,
                "logprobs": None,
            }
        ],
    }
    if usage is not None:
        chunk["usage"] = usage
    return chunk


def _usage_chunk(
    *,
    model: str,
    chunk_id: str,
    usage: Dict[str, int],
) -> Dict[str, Any]:
    """OpenAI-standard usage-only chunk: empty choices + usage."""
    return {
        "id": chunk_id,
        "object": "chat.completion.chunk",
        "created": unix_now(),
        "model": model,
        "choices": [],
        "usage": usage,
    }


async def _emit_final_usage_frames(
    *,
    model: str,
    chunk_id: str,
    usage: Dict[str, Any],
) -> AsyncIterator[str]:
    """Yield the trailing usage-only chunk followed by the [DONE] sentinel."""
    yield format_sse(_usage_chunk(model=model, chunk_id=chunk_id, usage=usage))
    yield done_frame()


def _canonicalize_payload_tool_names(payload: Dict[str, Any], legacy_map: Dict[str, str]) -> None:
    """原地改写 native 直通 payload 中的 tool_calls 名称 (旧拼写 → 注册新名)。

    只处理 choices[].delta.tool_calls 与 choices[].message.tool_calls 两处, 且仅当
    legacy_map 非空才被调用, 因此非 Claude Code 客户端(无映射)时零改写。
    """
    for choice in payload.get("choices") or []:
        if not isinstance(choice, dict):
            continue
        for container in (choice.get("delta"), choice.get("message")):
            if not isinstance(container, dict):
                continue
            for tc in container.get("tool_calls") or []:
                if not isinstance(tc, dict):
                    continue
                fn = tc.get("function") if isinstance(tc.get("function"), dict) else None
                if not fn:
                    continue
                fn["name"] = canonicalize_response_tool_name(
                    str(fn.get("name") or ""), legacy_map
                )


def _coerce_payload_tool_arguments(payload: Dict[str, Any], tools: List[Dict[str, Any]]) -> None:
    """响应侧直通: 复用 coerce_tool_input 把完整可解析的 tool_calls arguments 强转 (如 choices)。"""
    if not tools:
        return
    for choice in payload.get("choices") or []:
        if not isinstance(choice, dict):
            continue
        for container in (choice.get("delta"), choice.get("message")):
            if not isinstance(container, dict):
                continue
            for tc in container.get("tool_calls") or []:
                if not isinstance(tc, dict):
                    continue
                fn = tc.get("function") if isinstance(tc.get("function"), dict) else None
                if not fn:
                    continue
                args = fn.get("arguments")
                if not isinstance(args, str) or not args.strip():
                    continue
                try:
                    parsed = json.loads(args)
                except Exception:
                    continue
                if not isinstance(parsed, dict):
                    continue
                canonical_name = str(fn.get("name") or "")
                # 先按工具 schema 白名单过滤多余字段 (如 name/description),
                # 再做类型强转; 残缺 JSON 分片在上面已跳过, 不破坏直通。
                filtered = _filter_input_for_tool(canonical_name, parsed, tools)
                coerced = coerce_tool_input(canonical_name, filtered, tools)
                if not isinstance(coerced, dict) or coerced == parsed:
                    continue
                fn["arguments"] = json.dumps(coerced, ensure_ascii=False)


async def stream_native_passthrough(
    line_iter: AsyncIterator[str],
    *,
    model: str,
    prompt_text: str = "",
    legacy_map: Optional[Dict[str, str]] = None,
    tools: Optional[List[Dict[str, Any]]] = None,
) -> AsyncIterator[str]:
    """Forward upstream OpenAI SSE with proper multi-line event framing."""
    finished = False
    chunk_id = completion_id()
    content_parts: List[str] = []
    upstream_usage: Optional[Dict[str, int]] = None
    map_for_names = legacy_map or {}
    try:
        async for event in iter_sse_events(line_iter):
            if event.is_done:
                finished = True
                async for f in _emit_final_usage_frames(
                    model=model,
                    chunk_id=chunk_id,
                    usage=resolve_usage(
                        upstream=upstream_usage,
                        prompt_text=prompt_text,
                        completion_text="".join(content_parts),
                    ),
                ):
                    yield f
                return
            payload = event.json()
            if payload is None:
                # Pass through non-JSON data frames as-is
                if event.data:
                    yield format_sse(event.data, event=event.event)
                continue
            if model and payload.get("model") != model:
                payload["model"] = model
            # 响应侧反向规范化: 模型幻觉的旧工具名 → 请求真实注册名
            if map_for_names:
                _canonicalize_payload_tool_names(payload, map_for_names)
            # 响应侧参数 schema 强转 (choices 等数组/对象)
            if tools:
                _coerce_payload_tool_arguments(payload, tools)
            # Capture upstream usage (including cached_tokens and other extra fields)
            if isinstance(payload.get("usage"), dict):
                upstream_usage = payload["usage"]
            choices = payload.get("choices") or []
            if choices:
                delta = (choices[0] or {}).get("delta") or {}
                piece = delta.get("content")
                if piece:
                    content_parts.append(str(piece))
            yield format_sse(payload, event=event.event)
    except Exception as exc:  # noqa: BLE001
        # Emit a final error-shaped chunk then DONE so clients unblock
        yield format_sse(
            _chunk(
                model=model,
                delta={"content": f"\n[stream error: {exc}]"},
                finish_reason="stop",
                chunk_id=chunk_id,
            )
        )
        async for f in _emit_final_usage_frames(
            model=model,
            chunk_id=chunk_id,
            usage=resolve_usage(
                upstream=None,
                prompt_text=prompt_text,
                completion_text="".join(content_parts),
            ),
        ):
            yield f
        return
    if not finished:
        async for f in _emit_final_usage_frames(
            model=model,
            chunk_id=chunk_id,
            usage=resolve_usage(
                upstream=upstream_usage,
                prompt_text=prompt_text,
                completion_text="".join(content_parts),
            ),
        ):
            yield f


async def stream_prompt_fc(
    line_iter: AsyncIterator[str],
    *,
    model: str,
    tools: List[ToolDef],
    protocol: str,
    strip_think: bool,
    prompt_text: str = "",
    legacy_map: Optional[Dict[str, str]] = None,
) -> AsyncIterator[str]:
    sieve = create_sieve(tools, protocol=protocol)
    chunk_id = completion_id()
    yield format_sse(_chunk(model=model, delta={"role": "assistant"}, chunk_id=chunk_id))

    full_text_parts: List[str] = []
    emitted_calls = False
    upstream_usage: Optional[Dict[str, Any]] = None

    def _final_usage() -> Dict[str, Any]:
        return resolve_usage(
            upstream=upstream_usage,
            prompt_text=prompt_text,
            completion_text="".join(full_text_parts),
        )

    async def _emit_calls(calls: List[ToolCall]) -> AsyncIterator[str]:
        nonlocal emitted_calls
        if not calls:
            return
        emitted_calls = True
        openai_calls = to_openai_tool_calls(calls, legacy_map=legacy_map)
        for index, tc in enumerate(openai_calls):
            yield format_sse(
                _chunk(
                    model=model,
                    delta={
                        "tool_calls": [
                            {
                                "index": index,
                                "id": tc.get("id"),
                                "type": "function",
                                "function": {
                                    "name": (tc.get("function") or {}).get("name"),
                                    "arguments": (tc.get("function") or {}).get("arguments", ""),
                                },
                            }
                        ]
                    },
                    chunk_id=chunk_id,
                )
            )
        yield format_sse(
            _chunk(model=model, delta={}, finish_reason="tool_calls", chunk_id=chunk_id)
        )
        async for f in _emit_final_usage_frames(model=model, chunk_id=chunk_id, usage=_final_usage()):
            yield f

    try:
        async for event in iter_sse_events(line_iter):
            if event.is_done:
                break
            payload = event.json()
            if not payload:
                continue
            # Capture upstream usage if the intermediate forwards it
            if isinstance(payload.get("usage"), dict):
                upstream_usage = payload["usage"]
            choices = payload.get("choices") or []
            if not choices:
                continue
            delta = (choices[0] or {}).get("delta") or {}
            piece = delta.get("content")
            if not piece:
                message = (choices[0] or {}).get("message") or {}
                piece = message.get("content")
            if not piece:
                continue
            full_text_parts.append(str(piece))
            for sev in sieve.process_chunk(str(piece)):
                if sev.get("type") == "content":
                    text = str(sev.get("text") or "")
                    if text:
                        yield format_sse(
                            _chunk(model=model, delta={"content": text}, chunk_id=chunk_id)
                        )
                elif sev.get("type") == "tool_calls":
                    calls = parsed_calls_to_toolcalls(sev.get("calls") or [])
                    _log_upstream_text(full_text_parts)
                    async for frame in _emit_calls(calls):
                        yield frame
                    return

        for sev in sieve.flush():
            if sev.get("type") == "content":
                text = str(sev.get("text") or "")
                if text:
                    yield format_sse(_chunk(model=model, delta={"content": text}, chunk_id=chunk_id))
            elif sev.get("type") == "tool_calls" and not emitted_calls:
                calls = parsed_calls_to_toolcalls(sev.get("calls") or [])
                _log_upstream_text(full_text_parts)
                async for frame in _emit_calls(calls):
                    yield frame
                return

        if not emitted_calls:
            full_text = "".join(full_text_parts)
            calls = parse_text_to_calls(
                full_text, tools, protocol=protocol, strip_think=strip_think
            )
            if calls:
                _log_upstream_text(full_text_parts)
                async for frame in _emit_calls(calls):
                    yield frame
                return
            yield format_sse(_chunk(model=model, delta={}, finish_reason="stop", chunk_id=chunk_id))
        _log_upstream_text(full_text_parts)
        async for f in _emit_final_usage_frames(model=model, chunk_id=chunk_id, usage=_final_usage()):
            yield f
    except Exception as exc:  # noqa: BLE001
        yield format_sse(
            _chunk(
                model=model,
                delta={"content": f"\n[stream error: {exc}]"},
                finish_reason="stop",
                chunk_id=chunk_id,
            )
        )
        async for f in _emit_final_usage_frames(model=model, chunk_id=chunk_id, usage=_final_usage()):
            yield f
