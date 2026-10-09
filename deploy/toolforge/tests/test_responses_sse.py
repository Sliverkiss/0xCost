"""Tests for app.stream.responses_sse.complete_events."""

import json
from typing import AsyncIterator, List

import pytest

from app.stream.responses_sse import ResponsesStreamState, complete_events, stream_responses_from_openai_chat


def test_complete_events_cached_tokens():
    state = ResponsesStreamState(model="m")
    usage = {"prompt_tokens": 100, "completion_tokens": 50, "cached_tokens": 80}
    events = complete_events(state, usage=usage)
    assert len(events) == 1
    # Parse the SSE frame
    frame = events[0]
    data_line = [l for l in frame.splitlines() if l.startswith("data:")][0]
    payload = json.loads(data_line[5:].strip())
    response = payload["response"]
    assert response["usage"]["input_tokens"] == 100
    assert response["usage"]["output_tokens"] == 50
    assert response["usage"]["total_tokens"] == 150
    assert response["usage"]["input_tokens_details"]["cached_tokens"] == 80


async def _line_iter(lines: List[str]) -> AsyncIterator[str]:
    for line in lines:
        yield line


def _chunk_with_content(content: str, finish_reason=None):
    return {
        "id": "chatcmpl-1",
        "object": "chat.completion.chunk",
        "created": 1,
        "model": "m",
        "choices": [{"index": 0, "delta": {"content": content}, "finish_reason": finish_reason}],
    }


def _sse_lines(*payloads):
    lines: List[str] = []
    for p in payloads:
        lines.append(f"data: {json.dumps(p)}")
        lines.append("")
    lines.append("data: [DONE]")
    lines.append("")
    return lines


async def _collect_completed_usage(frames: List[str]):
    for frame in frames:
        for line in frame.splitlines():
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if not data or data == "[DONE]":
                continue
            try:
                obj = json.loads(data)
            except Exception:
                continue
            if obj.get("type") == "response.completed":
                return obj["response"]["usage"]
    return None


@pytest.mark.asyncio
async def test_stream_responses_no_upstream_usage_estimates():
    """上游不返回 usage 帧时, 流式 Responses 应本地估算非零 usage."""
    lines = _sse_lines(
        _chunk_with_content("hello world", finish_reason="stop"),
    )
    frames: List[str] = []
    async for frame in stream_responses_from_openai_chat(
        _line_iter(lines),
        model="m",
        prompt_text="prompt here",
    ):
        frames.append(frame)

    usage = await _collect_completed_usage(frames)
    assert usage is not None, "response.completed 事件未包含 usage"
    assert usage["total_tokens"] > 0
    assert usage["input_tokens"] > 0 or usage["output_tokens"] > 0


@pytest.mark.asyncio
async def test_stream_responses_cached_tokens_passthrough():
    """上游返回含 cached_tokens 的 usage 帧时, passthrough 保留 cache 字段."""
    usage_payload = {
        "prompt_tokens": 100,
        "completion_tokens": 20,
        "total_tokens": 120,
        "cached_tokens": 80,
    }
    lines = _sse_lines(
        _chunk_with_content("hi"),
        {
            "id": "chatcmpl-1",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": "m",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            "usage": usage_payload,
        },
    )
    frames: List[str] = []
    async for frame in stream_responses_from_openai_chat(
        _line_iter(lines),
        model="m",
        prompt_text="prompt",
    ):
        frames.append(frame)

    usage = await _collect_completed_usage(frames)
    assert usage is not None
    assert usage["input_tokens"] == 100
    assert usage["output_tokens"] == 20
    assert usage["total_tokens"] == 120
    assert usage["input_tokens_details"]["cached_tokens"] == 80
