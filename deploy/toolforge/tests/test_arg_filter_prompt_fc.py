"""Tests for ticket 06b: prompt_fc 解析路径的参数白名单过滤.

T6 只在 native 直通路径 (_coerce_payload_tool_arguments) 加了过滤, 但 Claude Code 真实
链路走 fc_mode=prompt → stream_prompt_fc → ToolSieve 解析, 不经 native 直通, 缺陷未修复。
本票在 prompt_fc 两个解析出口 (_emit_calls 流式 / _coerce_parsed_call 非流式) 复用
_filter_input_for_tool, 过滤模型惯性输出的 name/description 等多余字段。
"""

import json
from typing import AsyncIterator, List

import pytest

from app.engine.xyml import ProtocolSpec, ToolCallConfig, ToolSieve, parse_tool_calls
from app.models.canonical import ToolDef
from app.stream.openai_sse import stream_prompt_fc


# 工具 schema: Bash 仅注册 command 字段(与 Claude Code 一致)
BASH_TOOL = {
    "type": "function",
    "function": {
        "name": "Bash",
        "description": "run shell",
        "parameters": {
            "type": "object",
            "properties": {"command": {"type": "string"}},
            "required": ["command"],
        },
    },
}

# schema 含 name/description 的客户端(非 Claude Code): 合法字段不应被误删
BASH_TOOL_WITH_META = {
    "type": "function",
    "function": {
        "name": "Bash",
        "description": "run shell",
        "parameters": {
            "type": "object",
            "properties": {
                "command": {"type": "string"},
                "name": {"type": "string"},
                "description": {"type": "string"},
            },
            "required": ["command"],
        },
    },
}


def cfg():
    return ToolCallConfig(
        emit_protocol="XYML",
        parse_protocols=[ProtocolSpec("XYML"), ProtocolSpec("QNML")],
        missing_required="defaulted",
    )


def _xyml_with_extra_params():
    return (
        '<|XYML|tool_calls>\n'
        '<|XYML|invoke name="Bash">\n'
        '<|XYML|parameter name="command"><![CDATA[echo hello]]></|XYML|parameter>\n'
        '<|XYML|parameter name="name"><![CDATA[Print hello]]></|XYML|parameter>\n'
        '<|XYML|parameter name="description"><![CDATA[Print hello]]></|XYML|parameter>\n'
        '</|XYML|invoke>\n'
        '</|XYML|tool_calls>'
    )


def _bash_def(tool: object = BASH_TOOL) -> ToolDef:
    fn = tool["function"]
    return ToolDef(
        name=fn["name"],
        description=fn.get("description", ""),
        parameters=fn.get("parameters", {}),
        raw=tool,
    )


async def _line_iter(lines: List[str]) -> AsyncIterator[str]:
    for line in lines:
        yield line


async def _collect_tool_call_arguments(frames: List[str]) -> List[dict]:
    """从 stream_prompt_fc 产出的 SSE 帧里提取 tool_calls 的 arguments(dict)。"""
    args: List[dict] = []
    for frame in frames:
        for line in frame.splitlines():
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                continue
            try:
                obj = json.loads(data)
            except Exception:
                continue
            choices = obj.get("choices") or []
            if not choices:
                continue
            delta = (choices[0] or {}).get("delta") or {}
            for tc in delta.get("tool_calls") or []:
                fn = tc.get("function") or {}
                raw = fn.get("arguments")
                if isinstance(raw, str) and raw.strip():
                    args.append(json.loads(raw))
    return args


# --- 1. 流式: sieve/stream_prompt_fc 过滤多余字段 ---

@pytest.mark.asyncio
async def test_stream_prompt_fc_filters_extra_params():
    text = _xyml_with_extra_params()
    lines = [
        f"data: {json.dumps({'choices': [{'index': 0, 'delta': {'content': text}}]})}",
        "",
        "data: [DONE]",
        "",
    ]
    frames: List[str] = []
    async for frame in stream_prompt_fc(
        _line_iter(lines),
        model="m",
        tools=[_bash_def()],
        protocol="XYML",
        strip_think=True,
        prompt_text="test",
    ):
        frames.append(frame)

    args = await _collect_tool_call_arguments(frames)
    assert args == [{"command": "echo hello"}], f"应只剩 command, got {args!r}"


# --- 2. 非流式: parse_tool_calls 过滤多余字段 ---

def test_parse_tool_calls_filters_extra_params():
    parsed = parse_tool_calls(_xyml_with_extra_params(), [BASH_TOOL], config=cfg())
    assert len(parsed) == 1, [{"name": c.name, "input": c.input} for c in parsed]
    assert parsed[0].input == {"command": "echo hello"}, f"input 应只剩 command, got {parsed[0].input!r}"


# --- 3. schema 含 name/description 时不过滤(合法字段保留) ---

def test_schema_with_name_and_description_preserved():
    parsed = parse_tool_calls(_xyml_with_extra_params(), [BASH_TOOL_WITH_META], config=cfg())
    assert len(parsed) == 1
    assert parsed[0].input == {
        "command": "echo hello",
        "name": "Print hello",
        "description": "Print hello",
    }, f"合法字段不应被删, got {parsed[0].input!r}"


# --- 4. 纯文本不误伤 ---

def test_plain_text_no_false_calls():
    plain = "这是一段普通文本，没有任何工具调用标记。"
    parsed = parse_tool_calls(plain, [BASH_TOOL], config=cfg())
    assert parsed == [], f"纯文本不应产生 call, got {parsed!r}"


# --- 5. 无 schema 匹配时不过滤(原样保留, 不破坏) ---

def test_no_schema_props_keeps_input():
    # 工具无 properties → _filter_input_for_tool 无 properties → dict 原样返回, 不删除字段
    from app.engine.xyml import _filter_input_for_tool

    no_props = {"type": "function", "function": {"name": "ShellNotes", "parameters": {"type": "object"}}}
    kept = _filter_input_for_tool("ShellNotes", {"command": "x", "name": "n", "description": "d"}, [no_props])
    assert kept == {"command": "x", "name": "n", "description": "d"}


if __name__ == "__main__":
    import sys

    sys.exit(pytest.main([__file__, "-v"]))