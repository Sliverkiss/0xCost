r"""CDATA 非贪婪截断回归测试 (SPEC-20).

缺陷: ``_CDATA_RE`` 非贪婪匹配 ``([\s\S]*?)\]\]>``, 当参数值本身包含 ``]]>``
序列时 (如 Python 字符串 ``x = "]]>"``、数组下标 ``data[0]]]>``), 非贪婪在
第一个 ``]]>`` 处截断, 参数值被切断。

``_compute_cdata_bounds`` 原来只检测"多余 opener" (嵌套 CDATA, SPEC-16),
不检测"多余 closer"——当 ``opener_count <= matched_pairs`` 时直接返回 None,
不进入 greedy 模式。

修复 (SPEC-20): 在 ``if lower.count(opener) <= len(matches)`` 分支的
``return None`` 之前检测"多余 closer"——当 ``]]>`` 总数超过匹配对数时,
说明值内部有 ``]]>``, 用最后一个 ``]]>`` 作为 greedy 结束。

注意: CDATA 结束符一律用字符串拼接构造, 不在源码里写连续的字面闭符序列。
"""

from app.engine.xyml import ProtocolSpec, ToolCallConfig, ToolSieve, parse_tool_calls
from app.engine.xyml import _CDATA_RE, _compute_cdata_bounds, _extract_cdata_from_value

# CDATA 结束符用拼接构造。
CDATA_CLOSE = "]]" + ">"


def _fn_tool(name: str, params: dict) -> dict:
    return {"type": "function", "function": {"name": name, "parameters": params}}


BASH_TOOL = _fn_tool("Bash", {"type": "object", "properties": {"command": {"type": "string"}}})
BASH_TOOLS = [BASH_TOOL]


def _cfg() -> ToolCallConfig:
    return ToolCallConfig(parse_protocols=[ProtocolSpec("XYML")])


def _wrap_body(body: str) -> str:
    return "\n".join(
        [
            "<|XYML|tool_calls>",
            '  <|XYML|invoke name="Bash">',
            '    <|XYML|parameter name="command">' + body + "</|XYML|parameter>",
            "  </|XYML|invoke>",
            "</|XYML|tool_calls>",
        ]
    )


def _sieve_calls(text: str, tools: list):
    sieve = ToolSieve(tools=tools, config=_cfg())
    events = []
    for i in range(0, len(text), 3):
        events.extend(sieve.process_chunk(text[i : i + 3]))
    events.extend(sieve.flush())
    leaked = "".join(ev.get("text", "") for ev in events if ev.get("type") == "content")
    calls = []
    for ev in events:
        if ev.get("type") == "tool_calls":
            calls.extend(ev.get("calls") or [])
    return calls, leaked


# --- 直接单测: _compute_cdata_bounds / _extract_cdata_from_value ---


def test_single_pair_unaffected():
    """用例 1: 1 opener + 1 closer → 非贪婪正确, 不进入新分支。"""
    raw = "<![CDATA[hello" + CDATA_CLOSE
    assert _compute_cdata_bounds(raw) is None
    assert _extract_cdata_from_value(raw) == ["hello"]


def test_value_with_inner_closer_truncated_fixed():
    """用例 2: 1 opener + 3 closer (值含 ]]>) → 新分支用最后一个 ]]>。"""
    raw = "<![CDATA[x = \"" + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)" + CDATA_CLOSE
    # 非贪婪在第一个 ]]> 截断。
    m = _CDATA_RE.search(raw)
    assert m is not None and m.group(1) == 'x = "', repr(m.group(1) if m else None)
    # _compute_cdata_bounds 检测多余 closer → greedy 边界。
    bounds = _compute_cdata_bounds(raw)
    assert bounds is not None, bounds
    start, end = bounds
    assert start == 0, bounds
    assert end == len(raw) - 3, (bounds, len(raw))  # 终点 = 最后一个 ]]>
    expected = 'x = "' + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)"
    assert _extract_cdata_from_value(raw) == [expected], _extract_cdata_from_value(raw)


def test_pipe_variant_inner_closer():
    """管道符 opener `<![CDATA|` + 值含 ]]>, 同样取最后一个 ]]>。"""
    raw = "<![CDATA|x = \"" + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)" + CDATA_CLOSE
    expected = 'x = "' + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)"
    assert _extract_cdata_from_value(raw) == [expected], _extract_cdata_from_value(raw)


def test_unclosed_still_handled():
    """用例 3: 1 opener + 0 closer → unclosed 分支处理。"""
    raw = "<![CDATA[hello"
    assert _extract_cdata_from_value(raw) == ["hello"]


def test_nested_still_handled():
    """用例 4: 多 opener (嵌套) → 现有 SPEC-16 greedy 逻辑处理。"""
    raw = (
        "<![CDATA[outer <![CDATA[inner" + CDATA_CLOSE
        + " tail" + CDATA_CLOSE
    )
    result = _extract_cdata_from_value(raw)
    assert len(result) == 1, result
    assert result[0].startswith("outer <![CDATA[inner"), repr(result[0])
    assert result[0].endswith(" tail"), repr(result[0])


def test_plain_text_ignored():
    """用例 5: 纯文本无 CDATA → 不匹配, 返回 None / 空列表。"""
    raw = "just plain text"
    assert _compute_cdata_bounds(raw) is None
    assert _extract_cdata_from_value(raw) == []


def test_plain_text_with_closer_no_opener():
    """纯文本含 ]]> 但无 opener → 不触发新分支 (matches 为空守卫)。"""
    raw = "print('" + CDATA_CLOSE + "')"  # 有 ]]> 无 <![CDATA[
    assert _compute_cdata_bounds(raw) is None
    assert _extract_cdata_from_value(raw) == []


def test_sibling_sections_unaffected():
    """多段兄弟 CDATA (1:1 配比) → 非贪婪路径不受新分支影响。"""
    raw = "<![CDATA[a" + CDATA_CLOSE + "x<![CDATA[b" + CDATA_CLOSE
    assert _compute_cdata_bounds(raw) is None
    assert _extract_cdata_from_value(raw) == ["a", "b"]


# --- 集成: 参数值含 ]]>, 走完整 parse_tool_calls ---


def test_parse_value_with_inner_closer():
    """XYML 块内参数值含 `x = "]]>]]>\nprint(x)` → command 完整。"""
    body = "<![CDATA[x = \"" + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)" + CDATA_CLOSE
    calls = parse_tool_calls(_wrap_body(body), BASH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    expected = 'x = "' + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)"
    assert calls[0].input.get("command") == expected, repr(calls[0].input.get("command"))
    # 值内部含 ]]> 是合法内容 (SPEC-20), 不要求中间无 ]]>, 只检查完整性与首尾。
    assert CDATA_CLOSE in calls[0].input.get("command"), repr(calls[0].input.get("command"))


def test_parse_array_subscript_inner_closer():
    """参数值含数组下标 `data[0]]]>` → 值完整。"""
    body = "<![CDATA[print(data[0]" + CDATA_CLOSE + CDATA_CLOSE + ")" + CDATA_CLOSE
    calls = parse_tool_calls(_wrap_body(body), BASH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    expected = "print(data[0]" + CDATA_CLOSE + CDATA_CLOSE + ")"
    assert calls[0].input.get("command") == expected, repr(calls[0].input.get("command"))


# --- 流式 (3 字符分块) ---


def test_stream_value_with_inner_closer():
    """流式 3-char chunks: 参数值含 `x = "]]>]]>\nprint(x)` → 完整、无泄漏。"""
    body = "<![CDATA[x = \"" + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)" + CDATA_CLOSE
    text = _wrap_body(body)
    calls, leaked = _sieve_calls(text, BASH_TOOLS)
    assert not leaked, repr(leaked)
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    expected = 'x = "' + CDATA_CLOSE + CDATA_CLOSE + "\nprint(x)"
    assert calls[0].input.get("command") == expected, repr(calls[0].input.get("command"))


def test_stream_normal_unaffected():
    """流式回归: 正常 CDATA (1:1) 与非 CDATA 参数不受影响。"""
    text = _wrap_body("<![CDATA[echo hello" + CDATA_CLOSE)
    calls, leaked = _sieve_calls(text, BASH_TOOLS)
    assert not leaked, repr(leaked)
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].input.get("command") == "echo hello", repr(calls[0].input.get("command"))
