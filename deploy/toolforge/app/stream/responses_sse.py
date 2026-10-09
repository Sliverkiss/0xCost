"""OpenAI Responses API streaming event encoder.

Maps upstream OpenAI chat.completion.chunk SSE (or aggregated text/tool calls)
into the official Responses event sequence used by modern OpenAI SDKs.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Dict, List, Optional

from ..convert import _map_responses_usage
from ..engine.xyml import canonicalize_response_tool_name
from ..fc.parse import create_sieve, parse_text_to_calls, parsed_to_toolcall
from ..models.canonical import ToolCall, ToolDef
from ..util.ids import call_id, random_id, unix_now
from ..util.sse import format_sse, iter_sse_events
from ..util.tokens import resolve_usage


def _resp_id() -> str:
    return f"resp_{random_id(16)}"


def _msg_id() -> str:
    return f"msg_{random_id(12)}"


def _fc_id() -> str:
    return f"fc_{random_id(12)}"


@dataclass
class ResponsesStreamState:
    response_id: str = field(default_factory=_resp_id)
    model: str = ""
    sequence: int = 0
    output_index: int = 0
    created_at: int = field(default_factory=unix_now)
    text_item_id: Optional[str] = None
    text_content_index: int = 0
    text_started: bool = False
    full_text: str = ""
    # native tool call assembly: index -> {id, name, arguments}
    tool_buffers: Dict[int, Dict[str, str]] = field(default_factory=dict)
    tool_item_ids: Dict[int, str] = field(default_factory=dict)
    emitted_tool_indexes: set = field(default_factory=set)
    completed: bool = False

    def next_seq(self) -> int:
        self.sequence += 1
        return self.sequence

    def base_response(self, *, status: str = "in_progress") -> Dict[str, Any]:
        return {
            "id": self.response_id,
            "object": "response",
            "created_at": self.created_at,
            "status": status,
            "model": self.model,
            "output": [],
            "usage": None,
        }


def emit_event(event_type: str, payload: Dict[str, Any]) -> str:
    body = dict(payload)
    body["type"] = event_type
    return format_sse(body, event=event_type)


def start_events(state: ResponsesStreamState) -> List[str]:
    return [
        emit_event(
            "response.created",
            {
                "response": state.base_response(status="in_progress"),
                "sequence_number": state.next_seq(),
            },
        ),
        emit_event(
            "response.in_progress",
            {
                "response": state.base_response(status="in_progress"),
                "sequence_number": state.next_seq(),
            },
        ),
    ]


def ensure_text_item(state: ResponsesStreamState) -> List[str]:
    out: List[str] = []
    if state.text_started:
        return out
    state.text_started = True
    state.text_item_id = _msg_id()
    item = {
        "id": state.text_item_id,
        "type": "message",
        "status": "in_progress",
        "role": "assistant",
        "content": [],
    }
    out.append(
        emit_event(
            "response.output_item.added",
            {
                "output_index": state.output_index,
                "item": item,
                "sequence_number": state.next_seq(),
            },
        )
    )
    out.append(
        emit_event(
            "response.content_part.added",
            {
                "item_id": state.text_item_id,
                "output_index": state.output_index,
                "content_index": state.text_content_index,
                "part": {"type": "output_text", "text": ""},
                "sequence_number": state.next_seq(),
            },
        )
    )
    return out


def text_delta_events(state: ResponsesStreamState, delta: str) -> List[str]:
    if not delta:
        return []
    out = ensure_text_item(state)
    state.full_text += delta
    out.append(
        emit_event(
            "response.output_text.delta",
            {
                "item_id": state.text_item_id,
                "output_index": state.output_index,
                "content_index": state.text_content_index,
                "delta": delta,
                "sequence_number": state.next_seq(),
            },
        )
    )
    return out


def finish_text_item(state: ResponsesStreamState) -> List[str]:
    if not state.text_started or not state.text_item_id:
        return []
    out = [
        emit_event(
            "response.output_text.done",
            {
                "item_id": state.text_item_id,
                "output_index": state.output_index,
                "content_index": state.text_content_index,
                "text": state.full_text,
                "sequence_number": state.next_seq(),
            },
        ),
        emit_event(
            "response.content_part.done",
            {
                "item_id": state.text_item_id,
                "output_index": state.output_index,
                "content_index": state.text_content_index,
                "part": {"type": "output_text", "text": state.full_text},
                "sequence_number": state.next_seq(),
            },
        ),
        emit_event(
            "response.output_item.done",
            {
                "output_index": state.output_index,
                "item": {
                    "id": state.text_item_id,
                    "type": "message",
                    "status": "completed",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": state.full_text}],
                },
                "sequence_number": state.next_seq(),
            },
        ),
    ]
    state.output_index += 1
    return out


def ensure_tool_item(state: ResponsesStreamState, index: int, *, call_id_value: str, name: str) -> List[str]:
    if index in state.emitted_tool_indexes:
        return []
    state.emitted_tool_indexes.add(index)
    item_id = _fc_id()
    state.tool_item_ids[index] = item_id
    item = {
        "id": item_id,
        "type": "function_call",
        "status": "in_progress",
        "call_id": call_id_value or call_id(),
        "name": name or "",
        "arguments": "",
    }
    return [
        emit_event(
            "response.output_item.added",
            {
                "output_index": state.output_index + index,
                # keep sequential output_index: we'll use absolute later on done
                "item": item,
                "sequence_number": state.next_seq(),
            },
        )
    ]


def tool_args_delta(
    state: ResponsesStreamState,
    index: int,
    *,
    call_id_value: str,
    name: str,
    args_delta: str,
) -> List[str]:
    out = ensure_tool_item(state, index, call_id_value=call_id_value, name=name)
    buf = state.tool_buffers.setdefault(index, {"id": call_id_value or "", "name": name or "", "arguments": ""})
    if call_id_value:
        buf["id"] = call_id_value
    if name:
        buf["name"] = name
    if args_delta:
        buf["arguments"] = buf.get("arguments", "") + args_delta
        item_id = state.tool_item_ids.get(index) or _fc_id()
        out.append(
            emit_event(
                "response.function_call_arguments.delta",
                {
                    "item_id": item_id,
                    "output_index": state.output_index + index,
                    "delta": args_delta,
                    "sequence_number": state.next_seq(),
                },
            )
        )
    return out


def finish_tool_items(state: ResponsesStreamState) -> List[str]:
    out: List[str] = []
    for index in sorted(state.tool_buffers.keys()):
        buf = state.tool_buffers[index]
        item_id = state.tool_item_ids.get(index) or _fc_id()
        args = buf.get("arguments") or ""
        call = buf.get("id") or call_id()
        name = buf.get("name") or ""
        out.append(
            emit_event(
                "response.function_call_arguments.done",
                {
                    "item_id": item_id,
                    "output_index": state.output_index,
                    "arguments": args,
                    "sequence_number": state.next_seq(),
                },
            )
        )
        out.append(
            emit_event(
                "response.output_item.done",
                {
                    "output_index": state.output_index,
                    "item": {
                        "id": item_id,
                        "type": "function_call",
                        "status": "completed",
                        "call_id": call,
                        "name": name,
                        "arguments": args,
                    },
                    "sequence_number": state.next_seq(),
                },
            )
        )
        state.output_index += 1
    return out


def complete_events(
    state: ResponsesStreamState,
    *,
    usage: Optional[Dict[str, Any]] = None,
    error: Optional[str] = None,
    prompt_text: str = "",
    completion_text: str = "",
) -> List[str]:
    if state.completed:
        return []
    state.completed = True
    if error:
        return [
            emit_event(
                "response.failed",
                {
                    "response": {
                        **state.base_response(status="failed"),
                        "error": {"message": error},
                    },
                    "sequence_number": state.next_seq(),
                },
            )
        ]

    # Build final output snapshot
    output: List[Dict[str, Any]] = []
    if state.text_started:
        output.append(
            {
                "id": state.text_item_id,
                "type": "message",
                "status": "completed",
                "role": "assistant",
                "content": [{"type": "output_text", "text": state.full_text}],
            }
        )
    for index in sorted(state.tool_buffers.keys()):
        buf = state.tool_buffers[index]
        output.append(
            {
                "id": state.tool_item_ids.get(index) or _fc_id(),
                "type": "function_call",
                "status": "completed",
                "call_id": buf.get("id") or call_id(),
                "name": buf.get("name") or "",
                "arguments": buf.get("arguments") or "",
            }
        )

    response = state.base_response(status="completed")
    response["output"] = output
    if usage is not None:
        resolved = resolve_usage(
            upstream=usage,
            prompt_text=prompt_text,
            completion_text=completion_text,
        )
        response["usage"] = _map_responses_usage(resolved)
    return [
        emit_event(
            "response.completed",
            {"response": response, "sequence_number": state.next_seq()},
        )
    ]


async def stream_responses_from_openai_chat(
    line_iter: AsyncIterator[str],
    *,
    model: str,
    tools: Optional[List[ToolDef]] = None,
    protocol: str = "XYML",
    strip_think: bool = True,
    prompt_fc: bool = False,
    legacy_map: Optional[Dict[str, str]] = None,
    prompt_text: str = "",
) -> AsyncIterator[str]:
    """Convert upstream chat.completion SSE into Responses event stream."""
    state = ResponsesStreamState(model=model)
    map_for_names = legacy_map or {}
    for frame in start_events(state):
        yield frame

    sieve = create_sieve(tools or [], protocol=protocol) if prompt_fc else None
    full_prompt_text: List[str] = []
    completion_parts: List[str] = []
    usage: Dict[str, int] = {}
    saw_done = False

    try:
        async for event in iter_sse_events(line_iter):
            if event.is_done:
                saw_done = True
                break
            payload = event.json()
            if not payload:
                continue
            if isinstance(payload.get("usage"), dict):
                usage = payload["usage"]

            choices = payload.get("choices") or []
            if not choices:
                continue
            choice = choices[0] or {}
            delta = choice.get("delta") or {}
            finish = choice.get("finish_reason")

            if prompt_fc:
                piece = delta.get("content")
                if piece:
                    full_prompt_text.append(str(piece))
                    assert sieve is not None
                    for sev in sieve.process_chunk(str(piece)):
                        if sev.get("type") == "content":
                            for frame in text_delta_events(state, str(sev.get("text") or "")):
                                yield frame
                        elif sev.get("type") == "tool_calls":
                            # Finish any text first
                            for frame in finish_text_item(state):
                                yield frame
                            calls_raw = sev.get("calls") or []
                            for i, item in enumerate(calls_raw):
                                tc = parsed_to_toolcall(item)
                                args_s = (
                                    json.dumps(tc.arguments, ensure_ascii=False)
                                    if not isinstance(tc.arguments, str)
                                    else tc.arguments
                                )
                                for frame in tool_args_delta(
                                    state,
                                    i,
                                    call_id_value=str(tc.id or call_id()),
                                    name=str(tc.name or ""),
                                    args_delta=args_s,
                                ):
                                    yield frame
                            for frame in finish_tool_items(state):
                                yield frame
                            for frame in complete_events(
                                state,
                                usage=usage,
                                prompt_text=prompt_text,
                                completion_text="".join(full_prompt_text),
                            ):
                                yield frame
                            return
                continue

            # Native path
            content_piece = delta.get("content")
            if content_piece:
                completion_parts.append(str(content_piece))
                for frame in text_delta_events(state, str(content_piece)):
                    yield frame

            for tc in delta.get("tool_calls") or []:
                if not isinstance(tc, dict):
                    continue
                index = int(tc.get("index") or 0)
                fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
                raw_name = str(fn.get("name") or "")
                name = canonicalize_response_tool_name(raw_name, map_for_names)
                for frame in tool_args_delta(
                    state,
                    index,
                    call_id_value=str(tc.get("id") or ""),
                    name=name,
                    args_delta=str(fn.get("arguments") or ""),
                ):
                    yield frame

            if finish == "tool_calls":
                for frame in finish_text_item(state):
                    yield frame
                for frame in finish_tool_items(state):
                    yield frame
                for frame in complete_events(
                    state,
                    usage=usage,
                    prompt_text=prompt_text,
                    completion_text="".join(completion_parts),
                ):
                    yield frame
                return
            if finish in {"stop", "length", "content_filter"}:
                break

        # End of stream handling
        if prompt_fc and sieve is not None:
            for sev in sieve.flush():
                if sev.get("type") == "content":
                    for frame in text_delta_events(state, str(sev.get("text") or "")):
                        yield frame
                elif sev.get("type") == "tool_calls":
                    for frame in finish_text_item(state):
                        yield frame
                    calls_raw = sev.get("calls") or []
                    for i, item in enumerate(calls_raw):
                        tc = parsed_to_toolcall(item)
                        args_s = (
                            json.dumps(tc.arguments, ensure_ascii=False)
                            if not isinstance(tc.arguments, str)
                            else tc.arguments
                        )
                        for frame in tool_args_delta(
                            state,
                            i,
                            call_id_value=str(tc.id or call_id()),
                            name=str(tc.name or ""),
                            args_delta=args_s,
                        ):
                            yield frame
            # fallback full parse
            if not state.tool_buffers:
                full = "".join(full_prompt_text)
                calls = parse_text_to_calls(
                    full, tools or [], protocol=protocol, strip_think=strip_think
                )
                if calls:
                    for i, call in enumerate(calls):
                        args_s = json.dumps(call.arguments or {}, ensure_ascii=False)
                        for frame in tool_args_delta(
                            state,
                            i,
                            call_id_value=call.id or call_id(),
                            name=call.name,
                            args_delta=args_s,
                        ):
                            yield frame

        if state.tool_buffers:
            for frame in finish_text_item(state):
                yield frame
            for frame in finish_tool_items(state):
                yield frame
        else:
            for frame in finish_text_item(state):
                yield frame
        completion_text = "".join(full_prompt_text) if prompt_fc else "".join(completion_parts)
        for frame in complete_events(
            state,
            usage=usage,
            prompt_text=prompt_text,
            completion_text=completion_text,
        ):
            yield frame
    except Exception as exc:  # noqa: BLE001 — stream must close cleanly
        for frame in complete_events(state, error=str(exc)):
            yield frame
