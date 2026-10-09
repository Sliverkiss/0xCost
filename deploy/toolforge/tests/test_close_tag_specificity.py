r"""close tag 正则特异性回归测试 (SPEC-21).

缺陷: ``_protocol_any_close_tag_re`` 在 R7-R17 从精确匹配 (``<\s*/\s*\|\s*XYML\s*\|\s*tool_calls\s*>``)
宽化为"任意标识符 + 分隔符可选 + tag 可选"。参数值内部的 HTML/XML 标签
(``</ZStack>`` / ``</NavigationStack>`` / ``</script>``) 因此被 LIFO scanner 误匹配
为 protocol close tag:

  当模型省略 ``]]>`` 闭合 CDATA 时, ``_cdata_sections`` 无法识别 CDATA 范围,
  LIFO scanner 不屏蔽 CDATA 内部的标签 → ``</ZStack>`` 被当作 invoke 的 close tag
  → invoke body 被截断, ``</ZStack>`` 之后的参数 (如 ``path``) 全部丢失。

修复 (SPEC-21): 收缩 close tag 正则, 要求其中**至少有一个 ``|`` 或 ``:`` 分隔符**,
HTML/XML 关闭标签 (``</identifier>`` 无分隔符) 不再匹配。

注意: CDATA 结束符一律用字符串拼接构造, 不在源码里写连续的字面闭符序列。
"""

from app.engine.xyml import (
    ProtocolSpec,
    ToolCallConfig,
    _protocol_any_close_tag_re,
    parse_tool_calls,
)

# CDATA 结束符用拼接构造, 避免源码里出现字面 ]]> 序列。
_CDATA_CLOSE = "]]" + ">"


def _fn_tool(name: str, params: dict) -> dict:
    return {"type": "function", "function": {"name": name, "parameters": params}}


BASH_TOOL = _fn_tool("Bash", {"type": "object", "properties": {"command": {"type": "string"}}})
BASH_TOOLS = [BASH_TOOL]


def _cfg() -> ToolCallConfig:
    return ToolCallConfig(parse_protocols=[ProtocolSpec("XYML")])


def _matches_close_tag(text: str) -> bool:
    """任意 close tag 是否被 `_protocol_any_close_tag_re` (XYML 协议) 匹配。"""
    return bool(
        _protocol_any_close_tag_re("XYML", "tool_calls", "invoke", "parameter").search(text)
    )


# --- HTML/XML 标签不被匹配 (SPEC-21 核心) ---


def test_html_close_tag_zstack_no_match():
    """``</ZStack>`` 不被匹配 (TSX 组件标签)。"""
    assert not _matches_close_tag("</ZStack>")


def test_html_close_tag_navigationstack_no_match():
    """``</NavigationStack>`` 不被匹配。"""
    assert not _matches_close_tag("</NavigationStack>")


def test_html_close_tag_script_no_match():
    """``</script>`` 不被匹配。"""
    assert not _matches_close_tag("</script>")


def test_html_close_tag_body_no_match():
    """``</body>`` 不被匹配。"""
    assert not _matches_close_tag("</body>")


def test_html_close_tag_html_no_match():
    """``</html>`` 不被匹配。"""
    assert not _matches_close_tag("</html>")


# --- 标准协议 close tag 仍被匹配 ---


def test_standard_close_tag_parameter_matches():
    """``</|XYML|parameter>`` 正确匹配。"""
    assert _matches_close_tag("</|XYML|parameter>")


def test_colon_close_tag_parameter_matches():
    """``</:XYML:parameter>`` (冒号变体) 正确匹配。"""
    assert _matches_close_tag("</:XYML:parameter>")


def test_double_pipe_close_tag_parameter_matches():
    """``</||DSML||parameter>`` (双竖线协议名变体) 正确匹配。"""
    assert _matches_close_tag("</||DSML||parameter>")


def test_standard_close_tag_invoke_matches():
    """``</|XYML|invoke>`` 正确匹配。"""
    assert _matches_close_tag("</|XYML|invoke>")


def test_standard_close_tag_tool_calls_matches():
    """``</|XYML|tool_calls>`` 正确匹配。"""
    assert _matches_close_tag("</|XYML|tool_calls>")


def test_custom_tag_after_separator_matches():
    """``</|XYML|custom_tag>`` (tag 名变体) 正确匹配。"""
    assert _matches_close_tag("</|XYML|custom_tag>")


def test_bare_close_tag_separator_before_matches():
    """``</|XYML>`` (分隔符在前, tag 省略) 正确匹配。"""
    assert _matches_close_tag("</|XYML>")


# --- 端到端 ---


def test_unclosed_cdata_with_tsx_tags_file_text_and_path_preserved():
    """端到端: 模型不写 ``]]>`` + file_text 值含 ``</ZStack>`` → file_text 完整 + path 不丢失。

    对应真实 upstream_3f476d9e 场景: file_text 的 CDATA 未闭合 (无 ``]]>``),
    值内含 TSX 组件标签。修复前 ``</ZStack>`` 被误当 invoke 的 close tag,
    file_text 截断到 ``</ZStack>`` 处、后续 path 参数丢失。"""
    file_text_body = (
        "import io\n"
        'path = "/var/mobile/.../source-home.tsx"\n'
        "old = 'function PornhubTabSurface(props) { return "
        "<NavigationStack><ZStack>...{props.children}</ZStack></NavigationStack> }'\n"
        "new = old + '\\n' + 'function SidebarRow(props) { return "
        "<Button action={props.action}><HStack><Text>{props.title}</Text></HStack>"
        "</Button> }'\n"
    )
    text = "\n".join(
        [
            "<|XYML|tool_calls>",
            '  <|XYML|invoke name="file_tool">',
            '    <|XYML|parameter name="command"><![CDATA[write'
            + _CDATA_CLOSE
            + "</|XYML|parameter>",
            # file_text 的 CDATA 未闭合 (无 ]]>), 值内含 TSX 标签。
            "    <|XYML|parameter name=\"file_text\"><![CDATA[" + file_text_body + "</|XYML|parameter>",
            '    <|XYML|parameter name="path">/var/mobile/scripts/x.tsx</|XYML|parameter>',
            "  </|XYML|invoke>",
            "</|XYML|tool_calls>",
        ]
    )
    tools = [
        _fn_tool(
            "file_tool",
            {
                "type": "object",
                "properties": {
                    "command": {"type": "string"},
                    "file_text": {"type": "string"},
                    "path": {"type": "string"},
                },
            },
        )
    ]
    calls = parse_tool_calls(text, tools, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    inp = calls[0].input
    assert inp.get("command") == "write", inp
    assert inp.get("path") == "/var/mobile/scripts/x.tsx", "path 丢失"
    file_text = inp.get("file_text")
    assert file_text is not None, "file_text 丢失"
    # file_text 完整保留到 TSX 代码末尾, 不因 </ZStack> 被截断。
    assert "</ZStack>" in file_text
    assert "</NavigationStack>" in file_text
    assert file_text.rstrip().endswith("</Button> }'"), file_text[-80:]
    assert "file_text" in inp and "path" in inp


def test_standard_cdata_with_protocol_close_still_parses():
    """回归: 标准 ``<![CDATA[content]]>`` + ``</|XYML|parameter>`` 仍正确解析。"""
    text = "\n".join(
        [
            "<|XYML|tool_calls>",
            '  <|XYML|invoke name="Bash">',
            '    <|XYML|parameter name="command"><![CDATA[echo hi' + _CDATA_CLOSE + "</|XYML|parameter>",
            "  </|XYML|invoke>",
            "</|XYML|tool_calls>",
        ]
    )
    calls = parse_tool_calls(text, BASH_TOOLS, config=_cfg())
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    assert calls[0].name == "Bash"
    assert calls[0].input.get("command") == "echo hi"
    assert not str(calls[0].input.get("command")).startswith("<![CDATA[")


def test_streaming_unclosed_cdata_tsx_no_truncation():
    """流式 (3 字符分块): 未闭合 CDATA 含 ``</ZStack>`` 不截断, file_text 完整。"""
    from app.engine.xyml import ToolSieve

    file_text_body = (
        "import io\n"
        'path = "/var/mobile/.../source-home.tsx"\n'
        "old = 'function PornhubTabSurface(props) { return "
        "<NavigationStack><ZStack>...{props.children}</ZStack></NavigationStack> }'\n"
    )
    text = "\n".join(
        [
            "<|XYML|tool_calls>",
            '  <|XYML|invoke name="file_tool">',
            '    <|XYML|parameter name="command"><![CDATA[write'
            + _CDATA_CLOSE
            + "</|XYML|parameter>",
            "    <|XYML|parameter name=\"file_text\"><![CDATA[" + file_text_body + "</|XYML|parameter>",
            '    <|XYML|parameter name="path">/var/mobile/scripts/x.tsx</|XYML|parameter>',
            "  </|XYML|invoke>",
            "</|XYML|tool_calls>",
        ]
    )
    tools = [
        _fn_tool(
            "file_tool",
            {
                "type": "object",
                "properties": {
                    "command": {"type": "string"},
                    "file_text": {"type": "string"},
                    "path": {"type": "string"},
                },
            },
        )
    ]
    sieve = ToolSieve(tools=tools, config=_cfg())
    events = []
    for i in range(0, len(text), 3):
        events.extend(sieve.process_chunk(text[i : i + 3]))
    events.extend(sieve.flush())
    leaked = "".join(ev.get("text", "") for ev in events if ev.get("type") == "content")
    assert "XYML" not in leaked, f"协议标记泄漏: {leaked!r}"
    assert "<" not in leaked, f"标记碎片泄漏: {leaked!r}"
    calls = []
    for ev in events:
        if ev.get("type") == "tool_calls":
            calls.extend(ev.get("calls") or [])
    assert len(calls) == 1, [(c.name, c.input) for c in calls]
    inp = calls[0].input
    assert inp.get("path") == "/var/mobile/scripts/x.tsx", "path 丢失"
    file_text = inp.get("file_text")
    assert file_text is not None and "</ZStack>" in file_text, "file_text 被截断"
