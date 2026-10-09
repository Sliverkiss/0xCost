"""Tests for app.stream.openai_sse.stream_prompt_fc."""

import json
from typing import AsyncIterator, List

import pytest

from app.stream.openai_sse import stream_prompt_fc


async def _line_iter(lines: List[str]) -> AsyncIterator[str]:
    for line in lines:
        yield line


@pytest.mark.asyncio
async def test_stream_prompt_fc_captures_upstream_usage():
    """stream_prompt_fc should use upstream usage (with cached_tokens) in the final chunk."""
    # Simulate upstream SSE with content and a usage payload containing cached_tokens
    upstream_usage = {
        "prompt_tokens": 100,
        "completion_tokens": 20,
        "total_tokens": 120,
        "cached_tokens": 80,
    }
    chunk1 = {
        "id": "chatcmpl-1",
        "object": "chat.completion.chunk",
        "created": 1,
        "model": "m",
        "choices": [{"index": 0, "delta": {"content": "hello"}, "finish_reason": None}],
    }
    chunk2 = {
        "id": "chatcmpl-1",
        "object": "chat.completion.chunk",
        "created": 1,
        "model": "m",
        "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        "usage": upstream_usage,
    }
    lines = [
        f"data: {json.dumps(chunk1)}",
        "",
        f"data: {json.dumps(chunk2)}",
        "",
        "data: [DONE]",
        "",
    ]

    frames: List[str] = []
    async for frame in stream_prompt_fc(
        _line_iter(lines),
        model="m",
        tools=[],
        protocol="XYML",
        strip_think=True,
        prompt_text="test",
    ):
        frames.append(frame)

    # Find the final usage chunk (empty choices + usage)
    usage_frame = None
    for f in frames:
        for line in f.splitlines():
            if line.startswith("data:"):
                data = line[5:].strip()
                if data == "[DONE]":
                    continue
                try:
                    obj = json.loads(data)
                except Exception:
                    continue
                if obj.get("object") == "chat.completion.chunk" and not obj.get("choices"):
                    usage_frame = obj
    assert usage_frame is not None, "no usage chunk emitted"
    usage = usage_frame["usage"]
    # Should use upstream real values, not estimate
    assert usage["prompt_tokens"] == 100
    assert usage["completion_tokens"] == 20
    assert usage["total_tokens"] == 120
    # And must preserve cached_tokens passthrough
    assert usage["cached_tokens"] == 80
