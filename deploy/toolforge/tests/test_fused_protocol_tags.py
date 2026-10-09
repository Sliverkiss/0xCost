"""融合协议标签回归测试 (SPEC-18).

缺陷: 模型输出融合格式的 XYML 标签——协议名和结构标签名之间无 ``|`` 分隔符,
直接拼接成一个标识符 (``<xymltool_calls>`` / ``<xymltool_invoke name="...">`` /
``<xymltool_parameter>``)。所有扫描器正则都要求 ``|``/``:`` 分隔符存在, 所以
融合格式全部 NO MATCH → parser 返回 0 calls, 整个块作为 content 泄漏。

修复: ``_canonicalize_markup`` 阶段用 ``_FUSED_TAG_RE`` 把融合标签拆回标准
``<|协议|结构>`` 形状; ToolSieve 对完整 pending/capture 规范化, 跨 chunk 边界
的融合标签也能被正确拆分。
"""

from app.engine.xyml import ToolCallConfig, ToolSieve, parse_tool_calls

# CDATA 结束符用字符串拼接构造, 避免源码里出现字面 ]] 序列。
_CDATA_CLOSE = "]]" + ">"


def _cd(value: str) -> str:
    return "<![CDATA[" + value + _CDATA_CLOSE


def _fn_tool(name: str, params: dict) -> dict:
    return {"type": "function", "function": {"name": name, "parameters": params}}


SEARCH_TOOLS = [
    _fn_tool(
        "search_files",
        {
            "type": "object",
            "properties": {"pattern": {"type": "string"}, "text": {"type": "string"}},
        },
    ),
]


def _cfg() -> ToolCallConfig:
    return ToolCallConfig.for_prompt()


def _sieve_events(text: str, tools: list = None):
    sieve = ToolSieve(tools=tools if tools is not None else SEARCH_TOOLS, config=_cfg())
    return sieve.process_chunk(text) + sieve.flush()


def _calls_of(events):
    out = []
    for ev in events:
        if ev.get("type") == "tool_calls":
            out.extend(ev.get("calls") or [])
    return out


def _content_of(events) -> str:
    return "".join(ev.get("text", "") for ev in events if ev.get("type") == "content")


def _fused_sample(name: str = "search_files", value: str = "test", prefix_proto: str = "xyml") -> str:
    """构造一个融合协议工具块 (协议名与结构标签名之间无分隔符)。"""
    open_calls = "<" + prefix_proto + "tool_calls>"
    open_invoke = "<" + prefix_proto + 'tool_invoke name="' + name + '">'
    open_param = "<" + prefix_proto + 'tool_parameter name="pattern">'
    param = open_param + _cd(value) + "</" + prefix_proto + "tool_parameter>"
    return (
        open_calls
        + open_invoke
        + param
        + "</" + prefix_proto + "tool_invoke>"
        + "</" + prefix_proto + "tool_calls>"
    )


# --- 非流式 ---


def test_fused_basic_parsed():
    """基本融合标签 (非流式): 1 call, name=search_files, pattern=test。"""
    text = (
        "<xymltool_calls>"
        '<xymltool_invoke name="search_files">'
        '<xymltool_parameter name="pattern">' + _cd("test") + "</xymltool_parameter>"
        "</xymltool_invoke>"
        "</xymltool_tool_calls>"
    )
    calls = parse_tool_calls(text, SEARCH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "test"


def test_fused_uppercase_parsed():
    """大写融合标签 (非流式): ``<XYMLtool_calls>`` → 1 call。"""
    text = (
        "<XYMLtool_calls>"
        '<XYMLtool_invoke name="search_files">'
        '<XYMLtool_parameter name="pattern">' + _cd("UPPER") + "</XYMLtool_parameter>"
        "</XYMLtool_invoke>"
        "</XYMLtool_tool_calls>"
    )
    calls = parse_tool_calls(text, SEARCH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "UPPER"


def test_fused_with_preamble():
    """融合标签 + 前置叙述 (非流式): 叙述不破坏解析, 1 call。"""
    text = "Here is the result:\n" + _fused_sample()
    calls = parse_tool_calls(text, SEARCH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "test"


# --- 流式 (3 字符分块) ---


def _stream_events(text: str, tools: list = None):
    sieve = ToolSieve(tools=tools if tools is not None else SEARCH_TOOLS, config=_cfg())
    events: list = []
    for i in range(0, len(text), 3):
        events.extend(sieve.process_chunk(text[i : i + 3]))
    events.extend(sieve.flush())
    return events


def test_fused_basic_stream_no_leak():
    """基本融合标签 (流式 3-char chunks): 1 call, 0 protocol leak。"""
    text = (
        "<xymltool_calls>"
        '<xymltool_invoke name="search_files">'
        '<xymltool_parameter name="pattern">' + _cd("test") + "</xymltool_parameter>"
        "</xymltool_invoke>"
        "</xymltool_tool_calls>"
    )
    events = _stream_events(text)
    content = _content_of(events)
    assert "xyml" not in content, f"小写协议名泄漏: {content!r}"
    assert "tool_calls" not in content, f"结构标签泄漏: {content!r}"
    assert "<" not in content, f"标记碎片泄漏: {content!r}"
    calls = _calls_of(events)
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "test"


def test_fused_uppercase_stream_no_leak():
    """大写融合标签 (流式 3-char chunks): 1 call, 0 leak。"""
    text = (
        "<XYMLtool_calls>"
        '<XYMLtool_invoke name="search_files">'
        '<XYMLtool_parameter name="pattern">' + _cd("UPPER") + "</XYMLtool_parameter>"
        "</XYMLtool_invoke>"
        "</XYMLtool_tool_calls>"
    )
    events = _stream_events(text)
    content = _content_of(events)
    assert "XYML" not in content, f"大写协议名泄漏: {content!r}"
    assert "<" not in content, f"标记碎片泄漏: {content!r}"
    calls = _calls_of(events)
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "UPPER"


def test_fused_with_preamble_stream():
    """融合标签 + 前置叙述 (流式): 叙述作为 content, 0 protocol leak。"""
    preamble = "Here is the result:\n"
    text = preamble + _fused_sample()
    events = _stream_events(text)
    content = _content_of(events)
    assert "Here is the result:" in content, f"叙述未保留: {content!r}"
    assert "xyml" not in content, f"协议名泄漏: {content!r}"
    assert "<" not in content, f"标记碎片泄漏: {content!r}"
    calls = _calls_of(events)
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "test"


# --- 回归 ---


def test_standard_marker_non_stream_regression():
    """标准格式回归 (非流式): ``<|XYML|tool_calls>`` 仍 1 call。"""
    text = "\n".join(
        [
            "<|XYML|tool_calls>",
            '  <|XYML|invoke name="search_files">',
            '    <|XYML|parameter name="pattern">' + _cd("std") + "</|XYML|parameter>",
            "  </|XYML|invoke>",
            "</|XYML|tool_calls>",
        ]
    )
    calls = parse_tool_calls(text, SEARCH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "std"


def test_standard_marker_stream_regression():
    """标准格式回归 (流式): ``<|XYML|tool_calls>`` 仍 1 call, 无泄漏。"""
    text = "\n".join(
        [
            "<|XYML|tool_calls>",
            '  <|XYML|invoke name="search_files">',
            '    <|XYML|parameter name="pattern">' + _cd("std") + "</|XYML|parameter>",
            "  </|XYML|invoke>",
            "</|XYML|tool_calls>",
        ]
    )
    events = _stream_events(text)
    content = _content_of(events)
    assert "XYML" not in content, f"标准格式泄漏: {content!r}"
    calls = _calls_of(events)
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "search_files"
    assert calls[0].input.get("pattern") == "std"


# --- 防误判 ---


def test_fused_false_positive_plain_text():
    """纯文本含 ``xyml`` (非标签内) → 0 calls。"""
    text = "The xyml protocol is documented in the xyml spec."
    calls = parse_tool_calls(text, SEARCH_TOOLS, config=_cfg())
    assert calls == []


def test_fused_false_positive_html_tags():
    """HTML/XML 标签 (protocol 名不在白名单) → 0 calls。"""
    text = (
        "<htmltool>hello</htmltool>\n"
        "<input_parameter name='x'>value</input_parameter>\n"
        "<mytool_calls>nope</mytool_calls>"
    )
    calls = parse_tool_calls(text, SEARCH_TOOLS, config=_cfg())
    assert calls == []