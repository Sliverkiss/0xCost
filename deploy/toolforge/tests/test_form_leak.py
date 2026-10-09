"""协议变体宽容解析 + 响应侧 array/object schema 强转 回归测试 (Ticket 04)."""

import json
from typing import AsyncIterator, List

import pytest

from app.adapters.openai_chat import extract_text_and_native_calls
from app.engine.xyml import ProtocolSpec, ToolCallConfig, ToolSieve, parse_tool_calls
from app.stream.openai_sse import stream_native_passthrough


def _fn_tool(name: str, params: dict) -> dict:
    return {"type": "function", "function": {"name": name, "parameters": params}}


TERMINAL = _fn_tool("terminal", {"type": "object", "properties": {"command": {"type": "string"}}})
HTTP = _fn_tool("http_get_url", {"type": "object", "properties": {"url": {"type": "string"}}})
TOOLS_AB = [TERMINAL, HTTP]

CLARIFY = _fn_tool(
    "clarify",
    {"type": "object", "properties": {"choices": {"type": "array", "items": {"type": "string"}}}},
)

# 上游 tool_calls.arguments: choices 是字符串而非数组 (缺陷2 现场)
ARGS_STR = json.dumps({"choices": '["A","B"]'})


def _cfg() -> ToolCallConfig:
    return ToolCallConfig(parse_protocols=[ProtocolSpec("XYML"), ProtocolSpec("QNML")])


VARIANT_A = (
    '<xy tool_calls>\n'
    '<invoke name="terminal">\n'
    '<command><![CDATA[echo hello]]></command>\n'
    '</invoke>\n'
    '</xy>'
)

VARIANT_B = (
    '<||DSML||tool_calls>\n'
    '    <||DSML||tool name="http_get_url">\n'
    '      <||DSML||parameter name="url"><![CDATA[https://haloowhite.com/x]]></||DSML||parameter>\n'
    '    </||DSML||invoke>\n'
    '  </||DSML||tool_calls>'
)


def _sieve_events(text: str):
    sieve = ToolSieve(tools=TOOLS_AB, config=_cfg())
    return sieve.process_chunk(text) + sieve.flush()


def _calls_of(events):
    out = []
    for ev in events:
        if ev.get("type") == "tool_calls":
            out.extend(ev.get("calls") or [])
    return out


def _content_of(events):
    return "".join(ev.get("text", "") for ev in events if ev.get("type") == "content")


# --- 缺陷1: 协议变体非流式 ---

def test_variant_a_parse_nonstream():
    calls = parse_tool_calls(VARIANT_A, TOOLS_AB, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "terminal"
    assert calls[0].input.get("command") == "echo hello"


def test_variant_b_parse_nonstream():
    calls = parse_tool_calls(VARIANT_B, TOOLS_AB, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "http_get_url"
    assert calls[0].input.get("url") == "https://haloowhite.com/x"


# --- 缺陷1: 流式 sieve 不泄漏 ---

def test_variant_a_sieve_no_leak():
    events = _sieve_events(VARIANT_A)
    assert [(c.name, c.input) for c in _calls_of(events)] == [("terminal", {"command": "echo hello"})]
    content = _content_of(events)
    assert "tool_calls" not in content and "<xy" not in content


def test_variant_b_sieve_no_leak():
    events = _sieve_events(VARIANT_B)
    assert [(c.name, c.input) for c in _calls_of(events)] == [("http_get_url", {"url": "https://haloowhite.com/x"})]
    content = _content_of(events)
    assert "DSML" not in content and "tool_calls" not in content


def test_plain_text_no_false_positive():
    plain = "这是一段普通文本。DSML 只是一个普通词。HTML 里 <xy> 也是合法自定义标签。"
    assert parse_tool_calls(plain, TOOLS_AB, config=_cfg()) == []
    events = _sieve_events(plain)
    assert _calls_of(events) == []
    assert _content_of(events).strip() == plain


# --- 缺陷2: 响应侧 choices 强转 ---

def test_choices_coerce_nonstream():
    upstream = {
        "choices": [{
            "message": {"tool_calls": [{
                "id": "c1", "type": "function",
                "function": {"name": "clarify", "arguments": ARGS_STR},
            }]},
        }],
    }
    _, calls, _, _ = extract_text_and_native_calls(upstream, tools=[CLARIFY])
    assert len(calls) == 1
    assert calls[0].arguments["choices"] == ["A", "B"]


def test_choices_no_tools_untouched():
    upstream = {
        "choices": [{
            "message": {"tool_calls": [{
                "id": "c1", "type": "function",
                "function": {"name": "clarify", "arguments": ARGS_STR},
            }]},
        }],
    }
    _, calls, _, _ = extract_text_and_native_calls(upstream)
    assert len(calls) == 1
    assert calls[0].arguments["choices"] == '["A","B"]'


async def _line_iter(lines: List[str]) -> AsyncIterator[str]:
    for line in lines:
        yield line


@pytest.mark.asyncio
async def test_choices_coerce_stream():
    chunk = {
        "id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "m",
        "choices": [{
            "index": 0,
            "delta": {"tool_calls": [{
                "index": 0, "id": "call_1", "type": "function",
                "function": {"name": "clarify", "arguments": ARGS_STR},
            }]},
            "finish_reason": None,
        }],
    }
    lines = [f"data: {json.dumps(chunk)}", "", "data: [DONE]", ""]
    frames: List[str] = []
    async for frame in stream_native_passthrough(_line_iter(lines), model="m", tools=[CLARIFY]):
        frames.append(frame)
    forwarded_args = None
    for f in frames:
        for ln in f.splitlines():
            if not ln.startswith("data:"):
                continue
            payload = ln[5:].strip()
            if payload == "[DONE]":
                continue
            obj = json.loads(payload)
            for choice in obj.get("choices") or []:
                delta = (choice or {}).get("delta") or {}
                for tc in delta.get("tool_calls") or []:
                    fn = tc.get("function") or {}
                    if fn.get("name") == "clarify":
                        forwarded_args = fn.get("arguments")
    assert forwarded_args is not None
    assert json.loads(forwarded_args)["choices"] == ["A", "B"]