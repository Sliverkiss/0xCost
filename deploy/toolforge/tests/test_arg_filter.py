"""Tests for 响应侧参数白名单过滤 (ticket 06).

T4 在 native 直通路径只接了 coerce_tool_input (类型强转), 未接 _filter_input_for_tool
(白名单过滤), 导致上游模型惯性输出的 name/description 等多余字段被原样透传给客户端,
客户端按自身注册 schema 校验时报 InputValidationError。本票在 strong 前先按工具 schema
properties 过滤, 只加不改两个现有函数。
"""

import json

import pytest

from app.adapters.openai_chat import extract_text_and_native_calls
from app.stream.openai_sse import _coerce_payload_tool_arguments


# 工具 schema: Bash 仅注册 command 字段; 另一个工具注册 command + choices (数组)
BASH_TOOL = {
    "type": "function",
    "function": {
        "name": "Bash",
        "parameters": {
            "type": "object",
            "properties": {
                "command": {"type": "string"},
            },
            "required": ["command"],
        },
    },
}

CHOICE_TOOL = {
    "type": "function",
    "function": {
        "name": "Choose",
        "parameters": {
            "type": "object",
            "properties": {
                "command": {"type": "string"},
                "choices": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["command"],
        },
    },
}


def _upstream(arguments: object, tool_name: str = "Bash") -> dict:
    """构造含单个 tool_call 的上游 JSON (arguments 可为 dict 或 JSON 字符串)。"""
    return {
        "choices": [
            {
                "message": {
                    "tool_calls": [
                        {
                            "id": "1",
                            "type": "function",
                            "function": {
                                "name": tool_name,
                                "arguments": arguments,
                            },
                        }
                    ]
                }
            }
        ]
    }


# --- 1. 多余字段过滤 (非流式) ---

def test_filter_extra_name_description_fields():
    # 上游模型惯性输出 description/name, 但 Bash schema 只允许 command → 全部过滤
    _, calls, _, _ = extract_text_and_native_calls(
        _upstream({"command": "echo T5_RETRY", "description": "Print it", "name": "Print it"}),
        tools=[BASH_TOOL],
    )
    assert len(calls) == 1
    assert calls[0].arguments == {"command": "echo T5_RETRY"}


# --- 2. 保留合法字段 ---

def test_keep_legit_fields():
    # command + schema 允许的 choices 均保留
    _, calls, _, _ = extract_text_and_native_calls(
        _upstream({"command": "x", "choices": ["a", "b"]}, tool_name="Choose"),
        tools=[CHOICE_TOOL],
    )
    assert calls[0].arguments == {"command": "x", "choices": ["a", "b"]}


# --- 3. 类型强转仍生效 (T4 能力不回退) ---

def test_coerce_still_applies_after_filter():
    # choices 以 JSON 字符串传入 (字符串数组) → 强转成 list; 多余 name 同时被过滤
    _, calls, _, _ = extract_text_and_native_calls(
        _upstream({"command": "x", "choices": '["a", "b"]', "name": "junk"}, tool_name="Choose"),
        tools=[CHOICE_TOOL],
    )
    assert calls[0].arguments == {"command": "x", "choices": ["a", "b"]}


# --- 4. 无 tools 兼容 (原样透传) ---

def test_no_tools_passthrough():
    # 不传 tools → 不过滤不破坏, 多余字段原样保留
    _, calls, _, _ = extract_text_and_native_calls(
        _upstream({"command": "x", "description": "d", "name": "n"})
    )
    assert calls[0].arguments == {"command": "x", "description": "d", "name": "n"}


# --- 5. 流式 ---

def test_stream_filter_complete_json_fragment():
    # 完整 JSON 分片 → name 被过滤
    payload = {
        "choices": [
            {
                "delta": {
                    "tool_calls": [
                        {
                            "index": 0,
                            "id": "1",
                            "type": "function",
                            "function": {
                                "name": "Bash",
                                "arguments": json.dumps({"command": "ls", "name": "junk"}),
                            },
                        }
                    ]
                }
            }
        ]
    }
    _coerce_payload_tool_arguments(payload, [BASH_TOOL])
    fn = payload["choices"][0]["delta"]["tool_calls"][0]["function"]
    assert json.loads(fn["arguments"]) == {"command": "ls"}


def test_stream_incomplete_json_fragment_passthrough():
    # 残缺 JSON 分片 → 无法完整 json.loads → 原样 (不破坏直通)
    raw = '{"command": "ls", "na'
    payload = {
        "choices": [
            {
                "delta": {
                    "tool_calls": [
                        {
                            "index": 0,
                            "id": "1",
                            "type": "function",
                            "function": {"name": "Bash", "arguments": raw},
                        }
                    ]
                }
            }
        ]
    }
    _coerce_payload_tool_arguments(payload, [BASH_TOOL])
    fn = payload["choices"][0]["delta"]["tool_calls"][0]["function"]
    assert fn["arguments"] == raw