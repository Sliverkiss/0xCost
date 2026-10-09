"""Protocol-agnostic helpers for LLM tool calling.

The implementation intentionally depends only on the Python standard library so
applications can use it in lightweight clients, workers, and serverless jobs.
"""

from __future__ import annotations

import copy
import json
import re
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any, Dict, Iterable, List, Optional, Sequence, Set, Tuple, Union

from app.util.ids import call_id, random_id


DEFAULT_RAW_STRING_PARAMS: Set[str] = {
    "content",
    "command",
    "cmd",
    "script",
    "code",
    "prompt",
    "file_content",
    "old_string",
    "new_string",
    "insert_text",
    "patch",
    "pattern",
    "text",
    "query",
    "url",
    "path",
    "file_path",
}

DEFAULT_TOOL_ALIASES: Dict[str, str] = {
    "fs_open_file": "Read",
    "fs_put_file": "Write",
    "fs_patch_file": "Edit",
    "shell_run": "Bash",
    "text_search": "Grep",
    "path_find": "Glob",
    "notebook_patch": "NotebookEdit",
    "http_get_url": "WebFetch",
    "web_query": "WebSearch",
}

SAFE_TOOL_ALIASES: Dict[str, str] = {
    "Read": "fs_open_file",
    "Write": "fs_put_file",
    "Edit": "fs_patch_file",
    "Bash": "shell_run",
    "Grep": "text_search",
    "Glob": "path_find",
    "NotebookEdit": "notebook_patch",
    "WebFetch": "http_get_url",
    "WebSearch": "web_query",
}

# 旧拼写 → 新名 (仅当新名在请求注册集内时生效)。
# CodeBuddy 模型会幻觉旧工具名 (shell_run/fs_open_file/...), 而 Claude Code 客户端
# 注册的是新名 (Bash/Read/...)。在响应输出层做一次"旧拼写 → 请求真实注册名"的反向
# 映射即可修复, 且不破坏其他客户端。
_LEGACY_TOOL_NAME: Dict[str, str] = {
    "shell_run": "Bash",
    "fs_open_file": "Read",
    "fs_put_file": "Write",
    "fs_patch_file": "Edit",
    "text_search": "Grep",
    "path_find": "Glob",
    "notebook_patch": "NotebookEdit",
    "http_get_url": "WebFetch",
    "web_query": "WebSearch",
}


def build_legacy_name_map(registered_names: Iterable[str]) -> Dict[str, str]:
    """返回 {旧拼写: 真实注册名} 的子集: 只包含 "注册名里确有对应新名" 的条目。

    - 输入: 本次请求注册的工具名集合(规范化后, 大小写不敏感)
    - 输出: 例如 {'shellrun': 'Bash'} 若 Bash 在注册集内; 否则该条目被跳过
    """
    reg = {_tool_alias_key(n) for n in registered_names if n}
    out: Dict[str, str] = {}
    for legacy, new_name in _LEGACY_TOOL_NAME.items():
        if _tool_alias_key(new_name) in reg:
            out[_tool_alias_key(legacy)] = new_name
    return out


def canonicalize_response_tool_name(
    name: Any,
    legacy_map: Dict[str, str],
) -> str:
    """把模型回传的工具名规范化: 旧拼写→注册新名; 其他原样。"""
    key = _tool_alias_key(name)
    return legacy_map.get(key, name)


MARKUP_REPLACEMENTS: Tuple[Tuple[str, str], ...] = (
    ("＜", "<"),
    ("＞", ">"),
    ("／", "/"),
    ("∕", "/"),
    ("⁄", "/"),
    ("＝", "="),
    ("：", ":"),
    ("｜", "|"),
    ("│", "|"),
    ("┃", "|"),
    ("▏", "|"),
    ("▕", "|"),
    ("“", '"'),
    ("”", '"'),
    ("‘", "'"),
    ("’", "'"),
    ("﹤", "<"),
    ("﹥", ">"),
)


# --- Pre-compiled static regex patterns ---
_ATTR_RE = re.compile(
    r"\b(?:name|parameter)\s*=\s*(?:\"([^\"]*)\"|'([^']*)'|([A-Za-z0-9_.:-]+))",
    re.IGNORECASE | re.DOTALL,
)
# SPEC-15: the CDATA opener may be the standard `<![CDATA[` or the pipe variant
# `<![CDATA|` (deepseek-v4-flash emits the latter).  Shared by _CDATA_RE and
# _strip_nested_cdata_markers.
_CDATA_OPENER_RE = re.compile(r"<!\[CDATA[\[|]", re.IGNORECASE)
_CDATA_RE = re.compile(r"<!\[CDATA[\[|]([\s\S]*?)\]\]>", re.IGNORECASE)
# SPEC-18: fused protocol tags — the model emitted the protocol name jammed
# directly against the structural tag name with no `|`/`:` separator
# (`<xymltool_calls>` / `<xymltool_invoke name="X">` / `<xymltool_parameter>`).
# The standard scanners all require a `|`/`:` delimiter, so a fused tag never
# matches anywhere.  This regex splits a fused tag back into the standard
# `<|protocol|struct>` shape inside _canonicalize_markup, so every existing
# structured regex works downstream.  The protocol name is allow-listed, so
# HTML/XML prose (`<htmltool>`, `<input_parameter>`) is never touched.
_FUSED_TAG_RE = re.compile(
    r"(</?)"
    + r"(xyml|XYML|dsml|DSML|qnml|QNML)"
    + r"([A-Za-z0-9_]*?)"
    + r"(tool_calls|invoke|parameter|tool)"
    + r"(?![A-Za-z0-9_])"
    + r"(\s[^>]*>|>)",
    re.IGNORECASE,
)
# SPEC-11: protocol close tag written where the model omitted the CDATA
# `]]>` closer. Matches `</|...>` / `</||...>` / `</:...>`-style tags only
# (leading `|`/`:` after `</`), so HTML close tags like `</script>` inside
# CDATA are NOT treated as the CDATA boundary.
_PROTO_CLOSE_AFTER_CDATA = re.compile(r"</[|:]+[A-Za-z_][^>]*>", re.IGNORECASE)
_XML_TOOL_CALL_RES = tuple(
    re.compile(expr, re.IGNORECASE)
    for expr in (
        r"<tool_use\b([^>]*)>([\s\S]*?)</tool_use>",
        r"<tool_call\b([^>]*)>([\s\S]*?)</tool_call>",
        r"<function\b([^>]*)>([\s\S]*?)</function>",
        r"<invoke\b([^>]*)>([\s\S]*?)</invoke>",
    )
)
_TEXT_KV_LINE_RE = re.compile(r"^([A-Za-z_.-][A-Za-z0-9_.-]*)\s*:\s*(.*)$", re.DOTALL)
_XML_TAG_BODY_RE = re.compile(r"<([A-Za-z_][A-Za-z0-9_.:-]*)\b[^>]*>([\s\S]*?)</\1>")
_PATH_POLLUTION_RE = re.compile(r"[\r\n<>]")
_NAME_ATTR_RE = re.compile(
    r"(?:^|[\s|])name\s*=\s*(?:\"([^\"]*)\"|'([^']*)'|([^\s|/>]+))",
    re.DOTALL,
)
_JSON_TOOL_MARKER_RE = re.compile(r'^\s*\{\s*"tool_calls"', re.IGNORECASE | re.DOTALL)
_FUNC_NAME_MARKER_RE = re.compile(r"function\.name\s*:", re.IGNORECASE | re.DOTALL)
_STRUCT_CLOSE_RE = re.compile(r"\n\s*[\]}]\s*$")
_MD_FENCE_RE = re.compile(r"```[a-zA-Z0-9_-]*\s*([\s\S]*?)```")
_JSON_FENCE_RE = re.compile(r"```(?:json)?\s*([\s\S]*?)```", re.IGNORECASE)
_REPAIR_NAME_EQ_RE = re.compile(r'"name="\s*', re.IGNORECASE | re.DOTALL)
_REPAIR_NAME_EQ2_RE = re.compile(r'"name=([^",}\s]+)"', re.IGNORECASE | re.DOTALL)
_REPAIR_KEY_EQ_RE = re.compile(
    r'"(name|input|arguments|args|parameters|tool|tool_name|function_name)"\s*=\s*',
    re.IGNORECASE | re.DOTALL,
)
_REPAIR_KEY_COLON_RE = re.compile(
    r'([{,]\s*)(name|input|arguments|args|parameters|tool|tool_name|function_name)\s*:',
    re.IGNORECASE | re.DOTALL,
)
_ALIAS_KEY_RE = re.compile(r"[^a-z0-9]+")
# SPEC-09: mixed protocol-name markers (e.g. <||DSML|||XYML|tool_calls>). The
# structured regexes only accept one optional protocol prefix, so the doubled
# protocol name falls through and the sieve never enters capturing mode — the
# block leaks to the client as content.
#
# The loose scan requires a protocol separator (`|` or `:`) inside the tag:
# a mixed marker is *formed by* concatenating two protocol names, so it always
# carries one. Requiring it keeps genuine HTML/XML prose — a bare
# `<parameter name="x">` or `<invoke>` in a doc answer — from being captured
# and silently swallowed by flush() when it fails to parse.
#
# SPEC-13: collapsed protocol markers — the model reused a bare protocol marker
# as a parameter tag, writing name="PARAM" directly on it
# (`<||DSML|| name="command"><![CDATA[...]]></||DSML||>`). The original branch
# requires a structural tag word (tool_calls/invoke/parameter), which the
# collapsed marker lacks. The second branch widens the loose scan to any
# `<...>` tag carrying both a protocol separator and a name= attribute — the
# collapsed marker always has both, and genuine HTML/XML prose (bare
# `<a name="...">`, `<parameter name="...">`) has no `|`/`:` separator.
_LOOSE_TAG_MARKER_RE = re.compile(
    r"<\s*[^>]*[|:][^>]*\b(?:tool_calls|invoke|parameter)\b[^>]*>"  # SPEC-09: mixed/malformed structural tags
    r"|"
    r"<\s*[^>]*[|:][^>]*\bname\s*=[^>]*>",  # SPEC-13: collapsed marker (protocol sep + name= attr)
    re.IGNORECASE,
)
# SPEC-13: collapsed protocol marker — a bare protocol marker reused as a
# parameter tag. Shape: `<[|:]{1,2}NAME\1 name="PARAM">VALUE</\1NAME\1>` (e.g.
# `<||DSML|| name="command"><![CDATA[...]]></||DSML||>`). The name= attribute
# must immediately follow the closing delimiter (no tag word between), which is
# what distinguishes it from the standard `<|XYML|parameter name="x">` form —
# that form has `parameter` between the delimiters and name, so the `\1\s+name`
# sequence never lines up.
_COLLAPSED_PARAM_RE = re.compile(
    r"<([|:]{1,2})([A-Za-z_][A-Za-z0-9_]*)\1\s+name\s*=\s*[\"']?(\w+)[\"']?[^>]*>"
    r"(.*?)"
    r"</\1\2\1\s*>",
    re.IGNORECASE | re.DOTALL,
)
# SPEC-17: a bare `<:>` residue line the model emits at the end of its
# narrative, right before the XYML protocol block. Only standalone lines
# match (line start or after a newline, optional whitespace, `<:>`,
# optional whitespace, line end or newline); inline `<:>` in code
# (`a <:> b`) or backtick references is preserved.
_COLON_MARKER_RESIDUE_RE = re.compile(r"(?:^|\n)[ \t]*<:>[ \t]*(?=\n|$)")


@dataclass
class ParsedToolCall:
    """A normalized function call extracted from model output."""

    name: str
    input: Any = field(default_factory=dict)
    id: str = field(default_factory=call_id)

    def __post_init__(self) -> None:
        if not self.id:
            self.id = call_id()
        if self.input is None:
            self.input = {}


class ProtocolSpec:
    """Names and tags for one markup tool-call protocol."""

    def __init__(
        self,
        name: str,
        tags: Optional[Mapping[str, str]] = None,
        **options: Any,
    ) -> None:
        if not isinstance(name, str) or not name.strip():
            raise TypeError("ProtocolSpec name must be a non-empty string")
        supplied_tags = dict(tags or options.pop("tags", {}) or {})
        self.name = name.strip()
        self.tags = {
            "root": supplied_tags.get("root", "tool_calls"),
            "invoke": supplied_tags.get("invoke", "invoke"),
            "parameter": supplied_tags.get("parameter", "parameter"),
        }


class ToolCallConfig:
    """Parser, compatibility, and validation policies for tool calls.

    Snake-case option names are preferred in Python. JavaScript SDK option names
    are also accepted so a shared JSON configuration can be reused unchanged.
    """

    def __init__(self, options: Optional[Mapping[str, Any]] = None, **kwargs: Any) -> None:
        values: Dict[str, Any] = dict(options or {})
        values.update(kwargs)

        self.emit_protocol = str(
            _take_option(
                values,
                "emit_protocol",
                "emitProtocol",
                "default_protocol",
                "defaultProtocol",
                default="XYML",
            )
            or "XYML"
        ).strip()
        protocols = _take_option(values, "parse_protocols", "parseProtocols", "protocols")
        if protocols is None:
            protocols = [
                ProtocolSpec(self.emit_protocol),
                ProtocolSpec("QNML"),
            ]
        self.parse_protocols = _normalize_protocol_specs(protocols, self.emit_protocol)
        self.strict = bool(_take_option(values, "strict", default=False))
        self.unknown_tool = _take_option(values, "unknown_tool", "unknownTool", default="drop")
        self.missing_required = _take_option(
            values, "missing_required", "missingRequired", default="drop"
        )
        self.enable_markup = bool(
            _take_option(values, "enable_markup", "enableMarkup", default=True)
        )
        self.enable_xml = bool(_take_option(values, "enable_xml", "enableXml", default=True))
        self.enable_json = bool(_take_option(values, "enable_json", "enableJson", default=True))
        self.enable_text_kv = bool(
            _take_option(values, "enable_text_kv", "enableTextKV", default=True)
        )
        self.enable_coercion = bool(
            _take_option(values, "enable_coercion", "enableCoercion", default=True)
        )
        self.enable_dedupe = bool(
            _take_option(values, "enable_dedupe", "enableDedupe", default=True)
        )
        self.prompt_style = _take_option(values, "prompt_style", "promptStyle", default="standard")
        self.tool_aliases = dict(DEFAULT_TOOL_ALIASES)
        self.tool_aliases.update(
            dict(_take_option(values, "tool_aliases", "toolAliases", default={}) or {})
        )
        self.argument_aliases = dict(
            _take_option(values, "argument_aliases", "argumentAliases", default={}) or {}
        )
        custom_raw = _take_option(values, "raw_string_params", "rawStringParams", default=[]) or []
        self.raw_string_params = set(DEFAULT_RAW_STRING_PARAMS)
        self.raw_string_params.update(str(value).lower() for value in custom_raw)
        self.id_factory = _take_option(values, "id_factory", "idFactory", default=call_id)
        if not callable(self.id_factory):
            raise TypeError("id_factory must be callable")

    @classmethod
    def default(cls) -> "ToolCallConfig":
        return cls()

    @classmethod
    def for_prompt(cls, protocol: str = "XYML", missing_required: str = "defaulted") -> "ToolCallConfig":
        """Build config for prompt-based FC with standard parse protocols."""
        emit = (protocol or "XYML").strip() or "XYML"
        return cls(
            emit_protocol=emit,
            parse_protocols=[
                ProtocolSpec(emit),
                ProtocolSpec("QNML"),
                ProtocolSpec("XYML"),
                ProtocolSpec("DSML"),
            ],
            missing_required=missing_required,
        )


def _normalize_tools_key(value: Any) -> Tuple[str, ...]:
    """Build a stable, hashable cache key for ``normalize_tools`` input.

    The normalized output copies the whole ``function`` dict (or whole tool
    dict) of every accepted item, so the key must cover the full input to
    stay faithful. Tool definitions are JSON-serializable (schemas already
    round-trip through ``_tools_key`` today), so a ``sort_keys`` JSON dump is
    a lossless-enough fingerprint.
    """
    return tuple(
        json.dumps(raw, sort_keys=True, default=str)
        for raw in _as_list(value)
    )


@lru_cache(maxsize=64)
def _normalized_tools(key: Tuple[str, ...]) -> Tuple[Dict[str, Any], ...]:
    out: List[Dict[str, Any]] = []
    for raw_json in key:
        raw = json.loads(raw_json)
        if not _is_mapping(raw):
            continue
        if raw.get("type") == "function" and _is_mapping(raw.get("function")):
            out.append(dict(raw["function"]))
        elif isinstance(raw.get("name"), str) and raw["name"].strip():
            out.append(dict(raw))
    return tuple(out)


def normalize_tools(value: Any) -> List[Dict[str, Any]]:
    """Normalize OpenAI function specifications and plain tool specifications.

    P1-3: called 5+ times on the same tool list per request chain
    (build_tool_instructions, parse_tool_calls, coerce_tool_input, ToolSieve,
    _tools_key, _parse_protocol_markup). The normalized result is cached by
    input fingerprint and each call returns a deep copy, so a caller mutating
    its own result (including a nested ``parameters`` dict) can't poison the
    shared cache entry. Deep copy cost is negligible — normalize runs once per
    request chain and the cache absorbs the repeated calls.
    """

    return [copy.deepcopy(tool) for tool in _normalized_tools(_normalize_tools_key(value))]


def build_tool_instructions(
    tools: Any,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
    protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
) -> str:
    config = _resolve_config(config)
    active_protocol = _normalize_protocol_spec(protocol or config.emit_protocol)
    normalized_tools = normalize_tools(tools)
    safe_tools = [dict(tool, name=_safe_tool_name(tool.get("name"))) for tool in normalized_tools]
    names = [tool["name"] for tool in safe_tools if tool.get("name")]
    schemas = []
    for tool in safe_tools:
        parameters = tool.get("parameters") or tool.get("input_schema") or {}
        schemas.append(
            "\n".join(
                (
                    "Action name: {}".format(tool["name"]),
                    "Description: {}".format(_clip(tool.get("description", ""), 240)),
                    "Parameters: {}".format(_summarize_schema(parameters)),
                )
            )
        )
    example_tools = safe_tools[:2] or [
        {
            "name": "TOOL_NAME",
            "parameters": {
                "type": "object",
                "properties": {"ARG": {"type": "string"}},
            },
        }
    ]
    examples = "\n\n".join(
        render_tool_call(
            tool["name"],
            _example_input_from_tool(tool),
            config=config,
            protocol=active_protocol,
        )
        for tool in example_tools
    )
    accepted = ", ".join(spec.name for spec in config.parse_protocols)
    schema_block = "You have access to these tools:\n\n{}\n\n".format("\n\n".join(schemas)) if schemas else ""
    defensive_rules = ""
    if config.prompt_style != "minimal":
        defensive_rules = """
RULES:
1. If a tool is needed, output a parseable {name} tool-call block. If no tool is needed, answer normally.
2. Use exact action names and parameter names from the schema.
3. Strings should use <![CDATA[...]]>; objects may use JSON or nested XML-like values; arrays may use JSON arrays or repeated <item> nodes.
4. Never emit empty required parameters. Ask normally if required information is unknown.
5. After a tool result, call another tool only if needed; otherwise answer normally.
6. Path-like parameters must contain only the path string, not prose or protocol fragments.
""".format(name=active_protocol.name)
    rendered_format = render_tool_call(
        "TOOL_NAME", {"ARG": "value"}, config=config, protocol=active_protocol
    )
    return """=== {name} TOOL CALL PROTOCOL ===
{schema_block}Default protocol for new tool calls: {name}
Accepted parse protocols by this client: {accepted}
Available action names: {names}

FORMAT:
{rendered_format}
{defensive_rules}
CORRECT EXAMPLES:

{examples}

Remember: the preferred tool-call form is <|{name}|tool_calls>...</|{name}|tool_calls>.
=== END {name} TOOL INSTRUCTIONS ===""".format(
        name=active_protocol.name,
        schema_block=schema_block,
        accepted=accepted,
        names=", ".join(names),
        rendered_format=rendered_format,
        defensive_rules=defensive_rules,
        examples=examples,
    )


def render_tool_call(
    name: Any,
    input: Any = None,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
    protocol: Optional[Union[str, ProtocolSpec, Mapping[str, Any]]] = None,
) -> str:
    """Render one call in the configured markup protocol."""

    config = _resolve_config(config)
    active_protocol = _normalize_protocol_spec(protocol or config.emit_protocol)
    call_name = str(name or "").strip()
    if not call_name:
        return ""
    arguments = dict(input) if _is_mapping(input) else {"input": input}
    protocol_name = active_protocol.name
    root = active_protocol.tags["root"]
    invoke = active_protocol.tags["invoke"]
    parameter = active_protocol.tags["parameter"]
    lines = [
        "<|{}|{}>".format(protocol_name, root),
        '  <|{}|{} name="{}">'.format(protocol_name, invoke, _escape_xml(call_name)),
    ]
    for key in sorted(arguments, key=lambda item: str(item)):
        lines.append(
            '    <|{}|{} name="{}">{}</|{}|{}>'.format(
                protocol_name,
                parameter,
                _escape_xml(key),
                _render_markup_value(arguments[key]),
                protocol_name,
                parameter,
            )
        )
    lines.extend(
        (
            "  </|{}|{}>".format(protocol_name, invoke),
            "</|{}|{}>".format(protocol_name, root),
        )
    )
    return "\n".join(lines)


def parse_tool_calls(
    text: Any,
    tools: Any = None,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
) -> List[ParsedToolCall]:
    """Extract normalized calls from markup, JSON, XML, or text key/value output."""

    config = _resolve_config(config)
    normalized_tools = normalize_tools(tools)
    if not str(text or "").strip() or not normalized_tools:
        return []
    allowed = _build_allowed_tool_map(normalized_tools, config)
    calls: List[ParsedToolCall] = []
    if config.enable_markup:
        for protocol in config.parse_protocols:
            calls.extend(_parse_protocol_markup(text, protocol, allowed, normalized_tools, config))
    if config.enable_xml:
        calls.extend(_parse_xml_tool_calls(text, allowed, config))
    if config.enable_json:
        _for_each_json_fragment(
            text,
            lambda value: calls.extend(_parse_json_tool_calls(value, allowed, config)),
        )
    if config.enable_text_kv:
        calls.extend(_parse_text_kv_tool_calls(text, allowed, normalized_tools, config))
    fixed = calls
    if config.enable_coercion:
        fixed = [
            parsed
            for parsed in (_coerce_parsed_call(call, normalized_tools, config) for call in calls)
            if parsed is not None
        ]
    return _dedupe_tool_calls(fixed) if config.enable_dedupe else fixed


def coerce_tool_input(
    name: str,
    input: Any,
    tools: Any = None,
    *,
    config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
) -> Any:
    """Apply schema coercion and common aliases to a parsed call input."""

    config = _resolve_config(config)
    fixed = _coerce_tool_input_by_schema(name, input, normalize_tools(tools))
    if not _is_mapping(fixed):
        return fixed
    fixed = dict(fixed)
    aliases = config.argument_aliases.get(name, {}) if _is_mapping(config.argument_aliases) else {}
    for canonical, alternate_names in dict(aliases).items():
        _rename_first_present(fixed, canonical, *_as_list(alternate_names))
    if name == "AskUserQuestion":
        if fixed.get("question") is not None and fixed.get("questions") is None:
            fixed["questions"] = [
                {
                    "question": fixed["question"],
                    "header": "Question",
                    "multiSelect": False,
                    "options": [
                        {"label": "Yes", "description": "Confirm"},
                        {"label": "No", "description": "Decline"},
                    ],
                }
            ]
            fixed.pop("question", None)
        if fixed.get("questions") is not None and not isinstance(fixed["questions"], list):
            fixed["questions"] = [fixed["questions"]]
    elif name == "Agent":
        fixed.setdefault("description", "Execute sub-task")
        fixed.setdefault("prompt", fixed["description"])
    elif name == "Read":
        _rename_first_present(fixed, "file_path", "path", "filename", "file")
    elif name == "Write":
        _rename_first_present(fixed, "file_path", "path", "target_file", "filename", "file")
        _rename_first_present(
            fixed,
            "content",
            "text",
            "body",
            "data",
            "file_content",
            "contents",
            "value",
        )
    elif name == "Edit":
        _rename_first_present(fixed, "file_path", "path", "target_file", "filename", "file")
    elif name in {"Bash", "PowerShell"}:
        _rename_first_present(fixed, "command", "cmd", "script")
    elif fixed.get("query") is None and fixed.get("queries") is not None and _tool_accepts_field(
        name, tools, "query"
    ):
        queries = fixed.pop("queries")
        if isinstance(queries, list):
            fixed["query"] = "\n".join(str(value) for value in queries if str(value))
        else:
            fixed["query"] = str(queries).strip()
    return fixed


def _call_input(call: Any) -> Any:
    """Resolve a call's tool input from either ``input`` or ``arguments``."""
    value = _call_value(call, "input")
    return _call_value(call, "arguments", {}) if value is None else value


def openai_tool_calls(
    calls: Any, legacy_map: Optional[Dict[str, str]] = None
) -> List[Dict[str, Any]]:
    map_for_names = legacy_map or {}
    return [
        {
            "id": _call_value(call, "id"),
            "type": "function",
            "function": {
                "name": canonicalize_response_tool_name(_call_value(call, "name"), map_for_names),
                "arguments": _arguments_string(_call_input(call)),
            },
        }
        for call in _as_list(calls)
    ]


def responses_tool_items(
    calls: Any, legacy_map: Optional[Dict[str, str]] = None
) -> List[Dict[str, Any]]:
    map_for_names = legacy_map or {}
    return [
        {
            "id": "fc_{}".format(random_id(12)),
            "type": "function_call",
            "status": "completed",
            "call_id": _call_value(call, "id"),
            "name": canonicalize_response_tool_name(_call_value(call, "name"), map_for_names),
            "arguments": _arguments_string(_call_input(call)),
        }
        for call in _as_list(calls)
    ]


class ToolSieve:
    """Split streamed content from textual tool-call envelopes."""

    def __init__(
        self,
        tools: Any = None,
        *,
        config: Optional[Union[ToolCallConfig, Mapping[str, Any]]] = None,
        hold_length: int = 96,
    ) -> None:
        self.config = _resolve_config(config)
        self.tools = normalize_tools(tools)
        self.pending = ""
        self.capture = ""
        self.capturing = False
        self.hold_length = hold_length

    def process_chunk(self, chunk: Any) -> List[Dict[str, Any]]:
        if not chunk:
            return []
        # Canonicalize on entry: convert fullwidth/Unicode variant characters
        # (｜＞＜ etc. common from CJK models) to ASCII so all downstream
        # detection functions see consistent markup.
        self.pending = _canonicalize_markup(self.pending + str(chunk))
        events: List[Dict[str, Any]] = []
        if self.capturing:
            self.capture += self.pending
            self.pending = ""
            consumed = self._consume_capture(force=False)
            if consumed:
                events.extend(consumed)
            return events
        start = _first_tool_marker_index(self.pending, self.config)
        if start >= 0:
            prefix = self.pending[:start]
            if prefix:
                events.append(
                    {"type": "content", "text": _strip_colon_marker_residue(prefix)}
                )
            self.capture = self.pending[start:]
            self.pending = ""
            self.capturing = True
            consumed = self._consume_capture(force=False)
            if consumed:
                events.extend(consumed)
            return events
        if len(self.pending) <= self.hold_length:
            return events
        safe = self.pending[: -self.hold_length]
        self.pending = self.pending[-self.hold_length :]
        if safe:
            events.append(
                {"type": "content", "text": _strip_colon_marker_residue(safe)}
            )
        return events

    def flush(self) -> List[Dict[str, Any]]:
        events: List[Dict[str, Any]] = []
        if self.capturing and self.capture:
            consumed = self._consume_capture(force=True)
            if consumed:
                events.extend(consumed)
            elif _first_tool_marker_index(self.capture, self.config) >= 0:
                # Capture contains tool-call markup that failed to parse (e.g.
                # unknown tool name).  Do NOT leak the raw markup as content —
                # silently drop it.  This prevents XYML/XML blocks from being
                # forwarded to the client when the model emits a call for a
                # tool that is not in the registered tool list.
                pass
            else:
                events.append(
                    {"type": "content", "text": _strip_colon_marker_residue(self.capture)}
                )
            self.capture = ""
            self.capturing = False
        if self.pending:
            if _first_tool_marker_index(self.pending, self.config) >= 0:
                # Pending also contains tool markup that was never captured — drop it.
                pass
            else:
                events.append(
                    {"type": "content", "text": _strip_colon_marker_residue(self.pending)}
                )
            self.pending = ""
        return events

    def _consume_capture(self, force: bool) -> Optional[List[Dict[str, Any]]]:
        # SPEC-18: 融合标签跨 chunk 时逐 chunk canonicalize 在改动2中已修复 pending,
        # 但 self.capture 是从 pending 累积的, 可能仍含未拆分的融合标签残片。
        # 在 hold-guard 检查前对完整 capture 做规范化, 确保 _has_open_protocol_block
        # 和 _root_open 等检测函数看到的是标准格式。
        self.capture = _canonicalize_markup(self.capture)
        if not force and _has_open_protocol_block(self.capture, self.config):
            if self._root_open(self.capture):
                # Parallel invokes share one <tool_calls> root. Do not parse the
                # first invoke as soon as its own close tag arrives -- that would
                # reset the capture and swallow every sibling invoke. Wait until
                # the whole root block is LIFO-closed so all parallel calls are
                # captured and parsed together.
                if not self._root_closed(self.capture):
                    return None
            elif self._orphan_parameter(self.capture):
                # R35: parameter 开标签独占(无 root/invoke 开标签)。parameter 闭标签
                # 一到就解析会提前结束捕获, 让结尾残缺的 </invoke>/</tool_calls>
                # 闭标签被当作 content 泄漏。这里保持捕获直到 flush(force=True),
                # 由 parse_tool_calls 命中 _parse_orphan_parameter_calls 兜底。
                return None
            elif not _looks_structurally_closed(self.capture, self.config):
                # No root wrapper (a standalone <invoke>): parse once it closes.
                return None
        calls = parse_tool_calls(self.capture, self.tools, config=self.config)
        if not calls:
            return None
        self.capture = ""
        self.capturing = False
        return [{"type": "tool_calls", "calls": calls}]

    def _root_open(self, text: str) -> bool:
        return any(
            _protocol_open_tag_re(protocol.name, protocol.tags["root"]).search(text)
            for protocol in self.config.parse_protocols
        )

    def _orphan_parameter(self, text: str) -> bool:
        # 只有 parameter 开标签、完全没有 root/invoke 开标签时(半截块), 才算孤儿参数。
        for protocol in self.config.parse_protocols:
            if _protocol_open_tag_re(protocol.name, protocol.tags["root"]).search(text):
                return False
            if _protocol_open_tag_re(protocol.name, protocol.tags["invoke"]).search(text):
                return False
        return any(
            _protocol_open_tag_re(protocol.name, protocol.tags["parameter"]).search(text)
            for protocol in self.config.parse_protocols
        )

    def _root_closed(self, text: str) -> bool:
        return any(
            any(_iter_protocol_blocks(text, protocol, protocol.tags["root"]))
            for protocol in self.config.parse_protocols
        )


def _parse_protocol_markup(
    text: Any,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    canonical = _canonicalize_markup(_strip_markdown_fences(str(text or "")))
    calls: List[ParsedToolCall] = []
    # R13: an <invoke> that omits its name attribute falls back to the sole
    # available tool, so a bare "<|XYML|invoke>" with parameter children still
    # resolves when only one tool is registered.
    tool_names = {tool.get("name") for tool in normalize_tools(tools) if tool.get("name")}
    sole_tool = next(iter(tool_names)) if len(tool_names) == 1 else ""
    for candidate in _extract_protocol_candidates(canonical, protocol):
        # R22 同类恢复: 部分上游协议把 invoke 标签名写作 `tool`
        # (如 `<||DSML||tool name="X">`)。`tool` 块携带 name 属性时视作隐式
        # invoke; 其参数子块解析逻辑与 invoke 完全一致。
        for block_tag in _invoke_block_tags(protocol):
            for attrs, body in _iter_protocol_blocks(candidate, protocol, block_tag):
                name = _canonical_tool_name(_extract_name_attr(attrs), allowed, config)
                if not name:
                    name = sole_tool
                    if not name:
                        continue
                input = _parse_protocol_parameters(body, protocol, config)
                calls.append(ParsedToolCall(id=config.id_factory(), name=name, input=input))
    if not calls:
        calls.extend(_parse_tool_name_tag_calls(canonical, protocol, allowed, tools, config))
    if not calls:
        calls.extend(_parse_loose_protocol_calls(canonical, protocol, allowed, tools, config))
    if not calls:
        calls.extend(
            _parse_root_body_implicit_invoke(canonical, protocol, allowed, tools, config, sole_tool)
        )
    if not calls:
        calls.extend(
            _parse_orphan_parameter_calls(canonical, protocol, allowed, tools, config, sole_tool)
        )
    if not calls:
        calls.extend(
            _parse_collapsed_protocol_calls(canonical, protocol, allowed, tools, config, sole_tool)
        )
    return calls


def _parse_tool_name_tag_calls(
    text: str,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    """R22 recovery: model used the tool name as the invoke tag name.

    Instead of ``<|XYML|invoke name="write_file">``, the model emitted
    ``<|XYML|write_file>``.  The regular LIFO block scan only matches
    tags named ``invoke``/``parameter``/``tool_calls``, so these blocks
    are invisible to it.  Here we scan for any protocol open tag whose
    tag name resolves to a registered tool name, treat it as an implicit
    invoke, and parse its parameter children.

    The close tag may be:
    - A proper ``</|XYML|invoke>`` or ``</|XYML|write_file>``
    - A bare ``<|XYML|invoke>`` (missing the leading ``/``)
    - The root close ``</|XYML|tool_calls>`` (close tag omission)
    """
    open_re = _protocol_any_open_tag_re(
        protocol.name,
        protocol.tags["root"],
        protocol.tags["invoke"],
        protocol.tags["parameter"],
    )
    close_re = _protocol_any_close_tag_re(
        protocol.name,
        protocol.tags["root"],
        protocol.tags["invoke"],
        protocol.tags["parameter"],
    )
    structural = {
        protocol.tags["root"].lower(),
        protocol.tags["invoke"].lower(),
        protocol.tags["parameter"].lower(),
        "tool",
    }

    # Build a combined event list (open + close) for a LIFO scan that
    # correctly balances nested parameter blocks inside the tool-name tag.
    all_opens: List[Tuple[int, int, str]] = [
        (m.start(), m.end(), m.group("tag")) for m in open_re.finditer(text)
    ]
    all_closes: List[Tuple[int, int]] = [
        (m.start(), m.end()) for m in close_re.finditer(text)
    ]
    events: List[Tuple[Any, ...]] = []
    events.extend(("open", s, e, tag) for s, e, tag in all_opens)
    events.extend(("close", s, e) for s, e in all_closes)
    events.sort(key=lambda ev: ev[1])

    # First pass: for each tool-name open tag, find its matching close via LIFO.
    # A bare ``<|XYML|invoke>`` (missing ``/``) is treated as an open tag by
    # the regex, but the model intended it as a close.  We detect this: if an
    # ``invoke`` open tag appears with depth > 0 (inside a tool-name tag), we
    # treat it as a close tag instead.
    tool_tag_spans: List[Tuple[int, int, str]] = []  # (open_end, close_start, tool_name)
    stack: List[Tuple[str, int, int]] = []  # (tag, open_start, open_end)

    for ev in events:
        if ev[0] == "open":
            tag = ev[3]
            # Heuristic: a bare <invoke> inside a tool-name tag is really a close
            if tag.lower() == protocol.tags["invoke"].lower() and any(
                s[0].lower() not in structural for s in stack
            ):
                # Pop the most recent non-structural tag (the tool-name tag)
                while stack:
                    popped_tag, popped_start, popped_end = stack.pop()
                    if popped_tag.lower() not in structural:
                        tool_tag_spans.append((popped_end, ev[1], popped_tag))
                        break
            else:
                stack.append((tag, ev[1], ev[2]))
        elif stack:
            popped_tag, popped_start, popped_end = stack.pop()
            if popped_tag.lower() not in structural:
                tool_tag_spans.append((popped_end, ev[1], popped_tag))

    calls: List[ParsedToolCall] = []
    for body_start, body_end, tag_name in tool_tag_spans:
        tool_name = _canonical_tool_name(tag_name, allowed, config)
        if not tool_name:
            continue
        body = text[body_start:body_end]
        params = _parse_protocol_parameters(body, protocol, config)
        if params:
            calls.append(
                ParsedToolCall(id=config.id_factory(), name=tool_name, input=params)
            )
    return calls


def _invoke_block_tags(protocol: ProtocolSpec) -> List[str]:
    """invoke 块的候选标签名: 标准的 ``invoke`` 之外, 兼容 ``tool`` 标签
    (如变体 B 的 ``<||DSML||tool name="X">``)。"""

    tags = [protocol.tags["invoke"]]
    if "tool" not in tags:
        tags.append("tool")
    return tags


def _extract_protocol_candidates(text: str, protocol: ProtocolSpec) -> List[str]:
    candidates = [
        body
        for _, body in _iter_protocol_blocks(text, protocol, protocol.tags["root"])
    ]
    if candidates:
        return candidates
    match = _protocol_open_tag_re(protocol.name, protocol.tags["invoke"]).search(text)
    return [text[match.start() :]] if match else []


def _parse_protocol_parameters(body: str, protocol: ProtocolSpec, config: ToolCallConfig) -> Dict[str, Any]:
    out: Dict[str, Any] = {}
    for attrs, value in _iter_protocol_blocks(body, protocol, protocol.tags["parameter"]):
        name = _extract_name_attr(attrs)
        if name:
            # Parameter names are protocol-level case-insensitive: normalize to
            # the lowercase schema key so COMMAND/Command match "command".
            out[name.lower()] = _decode_markup_value(value, name, config)
    # R13 fallback: a parameter written with its name as the tag instead of a
    # name attribute (e.g. "<|XYML|pattern>...<|XYML|parameter>", or a stray
    # quote on the open tag "<|XYML|path">"). Well-formed parameters always
    # win; recovery only fills in keys the whitelist scan could not see.
    _recover_parameter_name_tags(out, body, protocol, config)
    return out or _parse_text_kv_input(body)


def _recover_parameter_name_tags(
    out: Dict[str, Any], body: str, protocol: ProtocolSpec, config: ToolCallConfig
) -> None:
    """Best-effort recovery of a parameter whose name leaked into the tag.

    The whitelist-based block scan only recognizes ``<|XYML|parameter ...>``,
    so a bad tag like ``<|XYML|pattern">`` or ``<|XYML|path>`` is invisible to
    it. Here we scan for any open tag whose name is not a structural keyword
    (e.g. ``pattern`` or ``path``) and pair it with a close tag that is either
    ``</|XYML|parameter>`` or the same leaked name. ``<name>`` becomes the
    parameter key. Only runs when no well-formed parameter was found and the
    body still contains a protocol block, so plain text and XML documents are
    never touched.
    """
    # The docstring contract: this recovery only fills keys the standard
    # whitelist scan could not see. If the standard path already extracted at
    # least one well-formed parameter (out is non-empty), running the regex
    # here would misread the attribute key in ``<|XYML|parameter name="url">``
    # as a leaked parameter tag and inject a bogus ``name`` field.
    if out:
        return
    structural = {"parameter", "invoke", "tool_calls", "root", "tool"}
    pattern = _parameter_name_tag_re(protocol.name)
    for match in pattern.finditer(body):
        name = match.group("name")
        if name.lower() in structural:
            continue
        out[name.lower()] = _decode_markup_value(match.group("val"), name, config)


def _parse_root_body_implicit_invoke(
    text: str,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
    sole_tool: str = "",
) -> List[ParsedToolCall]:
    """R18 recovery: model emitted <|XYML|tool_calls> root with parameter
    children but forgot the <|XYML|invoke name="..."> open tag (only a stray
    </|XYML|invoke> close tag appears, or none at all).

    When the root block body contains well-formed parameter blocks but no
    invoke open tag, the regular LIFO-block scan finds nothing.  Here we
    re-scan each root-body candidate: if it has >=1 parameter block and
    zero invoke open tags, treat the entire root body as a single implicit
    invoke whose name falls back to the sole registered tool.
    """
    if not sole_tool:
        return []
    root_tag = protocol.tags["root"]
    candidates = [
        body for _, body in _iter_protocol_blocks(text, protocol, root_tag)
    ]
    if not candidates:
        return []
    calls: List[ParsedToolCall] = []
    for body in candidates:
        has_invoke_open = bool(
            _protocol_open_tag_re(protocol.name, protocol.tags["invoke"]).search(body)
        )
        if has_invoke_open:
            continue
        params = _parse_protocol_parameters(body, protocol, config)
        if not params:
            continue
        calls.append(
            ParsedToolCall(id=config.id_factory(), name=sole_tool, input=params)
        )
    return calls


def _parse_orphan_parameter_calls(
    text: str,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
    sole_tool: str = "",
) -> List[ParsedToolCall]:
    """R35 recovery: 模型只输出一个良构 parameter 块 (有 name= 开标签 + 对应闭标签),
    而缺失 root/invoke 开标签 (常见尾随残缺的 </|XYML|invoke>、</|XYML|tool_calls> 闭标签),
    导致其余恢复链全部失效 (都要求先有 root 或 invoke 开标签)。

    当文本里不存在任何 root/invoke 开标签、但存在至少一个完整 parameter 块时,
    把该参数块当作"隐式 invoke 的唯一参数"解析, 工具名回退 sole_tool。为防误判:

    - 只处理参数键与 sole_tool schema 属性有交集的情况 (如 command 恰好是 Bash 的参数);
    - 无 sole_tool (多工具无法判定) 或键无交集时返回空, 不产生 call (宁漏勿错)。
    """
    if not sole_tool:
        return []
    # 前提: 文本中已能解析出完整调用 (root/invoke 开标签存在) 时, 不抢已覆盖场景。
    for tag in (protocol.tags["root"], protocol.tags["invoke"]):
        if _protocol_open_tag_re(protocol.name, tag).search(text):
            return []
    parameter_tag = protocol.tags["parameter"]
    if not _protocol_open_tag_re(protocol.name, parameter_tag).search(text):
        return []
    blocks = list(_iter_protocol_blocks(text, protocol, parameter_tag))
    if not blocks:
        return []
    params: Dict[str, Any] = {}
    for attrs, body in blocks:
        name = _extract_name_attr(attrs)
        if not name:
            continue
        params[name.lower()] = _decode_markup_value(body, name, config)
    if not params:
        return []
    # 参数键需与 sole_tool 的 schema 属性有交集, 才有把握判定工具归属。
    # 用 schema properties 做交集校验: 无 schema 或键无交集时返回空, 宁漏勿错。
    properties = _schema_properties(_tool_schema(sole_tool, tools))
    if not properties:
        return []
    accepted = _filter_input_for_tool(sole_tool, params, tools)
    if not accepted:
        return []
    return [ParsedToolCall(id=config.id_factory(), name=sole_tool, input=accepted)]


def _parse_collapsed_protocol_calls(
    text: str,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
    sole_tool: str = "",
) -> List[ParsedToolCall]:
    """SPEC-13 recovery: collapsed protocol marker (bare protocol name reused
    as a parameter tag).

    The model emitted ``<||DSML|| name="command"><![CDATA[...]]></||DSML||>``
    instead of a well-formed ``<|XYML|parameter name="command">`` block.  The
    tag carries a ``name=`` attribute directly on the protocol marker, so no
    structured parser upstream recognizes it.

    Every other recovery path (LIFO block scan, tool-name tags, loose
    protocol, root-body implicit invoke, orphan parameter) requires a
    root/invoke/parameter open tag and therefore all fall through.  This
    parser is the last rung: it matches the collapsed shape directly and
    extracts ``name``/value pairs.

    Safety (宁漏勿错):
    - Requires ``sole_tool`` (no tool-name discrimination with >1 tool).
    - Parameter keys must intersect the sole tool's schema properties.
    - Runs last, only when everything above produced nothing.
    """
    if not sole_tool:
        return []
    params: Dict[str, Any] = {}
    for match in _COLLAPSED_PARAM_RE.finditer(text):
        name = match.group(3)
        if not name:
            continue
        key = name.lower()
        if key in params:
            continue
        params[key] = _decode_markup_value(match.group(4), name, config)
    if not params:
        return []
    # 参数键需与 sole_tool 的 schema 属性有交集, 才有把握判定工具归属。
    properties = _schema_properties(_tool_schema(sole_tool, tools))
    if not properties:
        return []
    accepted = _filter_input_for_tool(sole_tool, params, tools)
    if not accepted:
        return []
    return [ParsedToolCall(id=config.id_factory(), name=sole_tool, input=accepted)]


def _parse_loose_protocol_calls(
    text: str,
    protocol: ProtocolSpec,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    if not re.search(r"\b{}\b".format(re.escape(protocol.name)), text, re.IGNORECASE):
        return []
    attributes: List[Tuple[str, str, bool, int]] = []
    for match in _ATTR_RE.finditer(text):
        raw = _html_unescape(next((value for value in match.groups() if value is not None), "").strip())
        if not raw:
            continue
        name = _canonical_tool_name(raw, allowed, config)
        attributes.append((raw, name or raw, bool(name), match.start()))
    calls: List[ParsedToolCall] = []
    for index, attribute in enumerate(attributes):
        raw, name, is_tool, position = attribute
        if not is_tool:
            continue
        next_tool = next(
            (item[3] for item in attributes[index + 1 :] if item[2]), len(text)
        )
        input: Dict[str, Any] = {}
        for field_raw, _, field_is_tool, field_position in attributes[index + 1 :]:
            if field_position >= next_tool or field_is_tool:
                break
            cdata_matches = _extract_cdata_from_value(text[field_position:next_tool])
            if cdata_matches:
                raw_string = str(field_raw or "").lower() in config.raw_string_params
                input[field_raw] = _coerce_markup_scalar(cdata_matches[0], raw_string=raw_string)
        filtered = _filter_input_for_tool(name, input, tools)
        if not filtered and _required_tool_args(name, tools):
            continue
        calls.append(ParsedToolCall(id=config.id_factory(), name=name, input=filtered))
    return calls


def _parse_xml_tool_calls(
    text: Any,
    allowed: Mapping[str, str],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    calls: List[ParsedToolCall] = []
    raw_text = str(text or "")
    for regex in _XML_TOOL_CALL_RES:
        for match in regex.finditer(raw_text):
            attrs, body = match.group(1), match.group(2).strip()
            name = _canonical_tool_name(_extract_name_attr(attrs), allowed, config)
            if name:
                calls.append(
                    ParsedToolCall(
                        id=config.id_factory(),
                        name=name,
                        input=_parse_tool_input(body),
                    )
                )
            else:
                # Fallback: <tool_call> with JSON body containing name (M4).
                ok, parsed = _try_json(body)
                if ok:
                    calls.extend(_parse_json_tool_calls(parsed, allowed, config))
    return calls


def _parse_json_tool_calls(
    value: Any,
    allowed: Mapping[str, str],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    calls: List[ParsedToolCall] = []
    if isinstance(value, list):
        for item in value:
            calls.extend(_parse_json_tool_calls(item, allowed, config))
        return calls
    if not _is_mapping(value):
        return calls
    for key in ("tool_calls", "tools"):
        if isinstance(value.get(key), list):
            for item in value[key]:
                calls.extend(_parse_json_tool_calls(item, allowed, config))
    name = _first_string(value.get("name"), value.get("tool"), value.get("tool_name"), value.get("function_name"))
    input = _first_defined(value.get("input"), value.get("arguments"), value.get("args"), value.get("parameters"))
    function = value.get("function")
    if _is_mapping(function):
        name = name or _first_string(function.get("name"))
        if input is None:
            input = _first_defined(function.get("arguments"), function.get("input"), function.get("parameters"))
    canonical_name = _canonical_tool_name(name, allowed, config)
    if canonical_name:
        calls.append(
            ParsedToolCall(
                id=_first_string(value.get("id"), value.get("call_id")) or config.id_factory(),
                name=canonical_name,
                input=_normalize_tool_input(input),
            )
        )
    return calls


def _parse_text_kv_tool_calls(
    text: Any,
    allowed: Mapping[str, str],
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> List[ParsedToolCall]:
    values: Dict[str, List[str]] = {"name": [], "arguments": []}
    current = ""
    aliases = {
        "function.name": "name",
        "name": "name",
        "tool": "name",
        "tool.name": "name",
        "tool_name": "name",
        "function.arguments": "arguments",
        "arguments": "arguments",
        "args": "arguments",
        "input": "arguments",
        "tool_input": "arguments",
        "parameters": "arguments",
    }
    for raw_line in str(text or "").splitlines():
        line = raw_line.strip()
        match = _TEXT_KV_LINE_RE.match(line)
        if match and match.group(1).lower() in aliases:
            current = aliases[match.group(1).lower()]
            values[current].append(match.group(2).strip())
            continue
        if current:
            values[current].append(raw_line)
    if not values["name"]:
        return []
    raw_name = "\n".join(values["name"]).splitlines()[0].strip().strip("'\"")
    name = _canonical_tool_name(raw_name, allowed, config)
    if not name:
        return []
    input = _normalize_tool_input("\n".join(values["arguments"]).strip())
    # Coercion is handled by the parse_tool_calls main loop (M2); return raw call.
    return [ParsedToolCall(id=config.id_factory(), name=name, input=input)]


def _coerce_parsed_call(
    call: ParsedToolCall,
    tools: Sequence[Mapping[str, Any]],
    config: ToolCallConfig,
) -> Optional[ParsedToolCall]:
    input = coerce_tool_input(call.name, call.input, tools, config=config)
    # R36: 白名单过滤多余字段 (如模型惯性输出的 name/description)
    if isinstance(input, dict):
        input = _filter_input_for_tool(call.name, input, tools)
    if config.unknown_tool == "error" and _tool_schema(call.name, tools) is None:
        raise ValueError("Unknown tool: {}".format(call.name))
    if _missing_required_args(call.name, input, tools):
        if config.missing_required == "error" or config.strict:
            raise ValueError("Missing required arguments for tool: {}".format(call.name))
        if config.missing_required == "drop":
            return None
    if _invalid_tool_args(input):
        return None
    return ParsedToolCall(id=call.id, name=call.name, input=input)


def _coerce_tool_input_by_schema(name: str, input: Any, tools: Sequence[Mapping[str, Any]]) -> Any:
    if not _is_mapping(input):
        return input
    properties = _schema_properties(_tool_schema(name, tools))
    if not properties:
        return input
    fixed = dict(input)
    for key, value in fixed.items():
        if _is_mapping(properties.get(key)):
            fixed[key] = _coerce_value_by_schema(value, properties[key])
    return fixed


def _coerce_value_by_schema(value: Any, schema: Mapping[str, Any]) -> Any:
    types = _schema_types(schema)
    if isinstance(value, str) and ("array" in types or "object" in types):
        parsed, changed = _parse_json_string_for_schema(value, "array" in types, "object" in types)
        if changed:
            value = parsed
    if "array" in types:
        if _is_mapping(value):
            value = [value]
        if isinstance(value, list) and _is_mapping(schema.get("items")):
            return [_coerce_value_by_schema(item, schema["items"]) for item in value]
        return value
    if "object" in types and _is_mapping(value):
        properties = _schema_properties(schema)
        if not properties:
            return value
        fixed = dict(value)
        for key, child in properties.items():
            if key in fixed and _is_mapping(child):
                fixed[key] = _coerce_value_by_schema(fixed[key], child)
        return fixed
    return value


def _parse_json_string_for_schema(value: str, want_array: bool, want_object: bool) -> Tuple[Any, bool]:
    stripped = value.strip()
    if not stripped:
        return value, False
    candidates = [stripped]
    if want_array and not stripped.startswith("["):
        candidates.append("[{}]".format(stripped))
    for candidate in candidates:
        ok, parsed = _try_json(candidate)
        if not ok:
            continue
        if want_array and isinstance(parsed, list):
            return parsed, True
        if want_array and _is_mapping(parsed):
            return [parsed], True
        if want_object and _is_mapping(parsed):
            return parsed, True
    return value, False


def _normalize_tool_input(value: Any) -> Any:
    if value is None:
        return {}
    if isinstance(value, str):
        trimmed = value.strip()
        if not trimmed:
            return {}
        ok, parsed = _try_json(trimmed)
        if ok:
            return _normalize_tool_input(parsed)
        key_values = _parse_text_kv_input(trimmed)
        return key_values if key_values else value
    return value


def _parse_tool_input(text: str) -> Any:
    if not text:
        return {}
    ok, parsed = _try_json(text)
    if ok:
        return _normalize_tool_input(parsed)
    parameters: Dict[str, Any] = {}
    for match in _XML_TAG_BODY_RE.finditer(text):
        parameters[match.group(1)] = _decode_markup_value(
            match.group(2), match.group(1), _DEFAULT_CFG
        )
    if parameters:
        return parameters
    key_values = _parse_text_kv_input(text)
    return key_values if key_values else {"input": text}


def _parse_text_kv_input(text: Any) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for raw_line in str(text or "").splitlines():
        line = raw_line.strip()
        if not line:
            continue
        equals = line.find("=")
        colon = line.find(":")
        separator = colon if equals < 0 or (colon >= 0 and colon < equals) else equals
        if separator <= 0:
            continue
        key = line[:separator].strip()
        value = line[separator + 1 :].strip().strip("'\"")
        if key:
            out[key] = value
    return out


def _for_each_json_fragment(text: Any, visit: Callable[[Any], None]) -> None:
    normalized = _strip_json_fence(str(text or ""))
    for candidate in (
        normalized,
        _repair_loose_json(normalized),
        _recover_json_like(normalized),
    ):
        ok, parsed = _try_json(candidate)
        if ok:
            visit(parsed)
    starts = [index for index, char in enumerate(normalized) if char in "{"]
    starts.extend(index for index, char in enumerate(normalized) if char == "[")
    for start in starts:
        for end in range(len(normalized), start, -1):
            fragment = normalized[start:end]
            ok, parsed = _try_json(fragment)
            if ok:
                visit(parsed)
                break
            ok, parsed = _try_json(_repair_loose_json(fragment))
            if ok:
                visit(parsed)
                break


def _build_allowed_tool_map(
    tools: Sequence[Mapping[str, Any]], config: ToolCallConfig
) -> Dict[str, str]:
    allowed: Dict[str, str] = {}
    for tool in tools:
        name = tool.get("name")
        if not name:
            continue
        allowed[_tool_alias_key(name)] = name
    # The upstream tools carry canonical names (fs_open_file/shell_run/...),
    # but the model is instructed to emit friendly aliases (Read/Bash/Grep/...).
    # SAFE_TOOL_ALIASES is keyed alias -> canonical, so reverse-register every
    # entry to make the alias spelling resolve back to the real tool name.
    for alias, canonical in SAFE_TOOL_ALIASES.items():
        allowed.setdefault(_tool_alias_key(alias), canonical)
    # config.tool_aliases (DEFAULT_TOOL_ALIASES) is keyed canonical -> alias.
    # Register both directions so either spelling resolves, without ever
    # overwriting a real tool name with a bare alias string.
    for canonical, alias in config.tool_aliases.items():
        key = _tool_alias_key(canonical)
        alias_key = _tool_alias_key(alias)
        resolved = allowed.get(key, canonical)
        allowed.setdefault(alias_key, resolved)
        allowed.setdefault(key, resolved)
    return allowed


def _canonical_tool_name(name: Any, allowed: Mapping[str, str], config: ToolCallConfig) -> str:
    raw = str(name or "").strip()
    if not raw:
        return ""
    direct = allowed.get(_tool_alias_key(raw))
    if direct:
        return direct
    configured = config.tool_aliases.get(raw) or config.tool_aliases.get(raw.lower())
    if configured and allowed.get(_tool_alias_key(configured)):
        return allowed[_tool_alias_key(configured)]
    if raw.startswith("u_"):
        candidate = raw[2:]
        if candidate:
            hit = allowed.get(_tool_alias_key(candidate))
            if hit:
                return hit
        # ``u_`` is the user-tool namespace prefix injected by
        # ``build_tool_instructions``.  When the underlying tool name itself
        # contains ``__`` (e.g. MCP-qualified ``mcp__playwright__browser_navigate``),
        # stripping only the leading ``u_`` is correct — the remainder is the
        # exact registered tool name and the direct lookup above already
        # handles it.  Do NOT strip ``mcp__...__`` segments here: that would
        # turn ``mcp__wrongserver__browser_navigate`` into ``browser_navigate``
        # and silently bind the call to the wrong tool when a bare-name sibling
        # is registered.
    return "" if config.unknown_tool == "drop" else raw


def _dedupe_tool_calls(calls: Iterable[ParsedToolCall]) -> List[ParsedToolCall]:
    seen: Set[str] = set()
    out: List[ParsedToolCall] = []
    for call in calls:
        key = "{}\0{}".format(_tool_alias_key(call.name), _stable_stringify(call.input))
        if not call.name or key in seen:
            continue
        seen.add(key)
        out.append(call)
    return out


def _tools_key(tools: Any) -> Tuple[Tuple[Any, str], ...]:
    return tuple(
        sorted(
            (
                tool.get("name"),
                json.dumps(
                    tool.get("parameters") or tool.get("input_schema"),
                    sort_keys=True,
                    default=str,
                ),
            )
            for tool in normalize_tools(tools)
            if tool.get("name")
        )
    )


@lru_cache(maxsize=64)
def _tool_schema_map(tools_key: Tuple[Tuple[Any, str], ...]) -> Dict[str, Mapping[str, Any]]:
    cached: Dict[str, Mapping[str, Any]] = {}
    for tool_name, params_json in tools_key:
        params = json.loads(params_json)
        if _is_mapping(params):
            cached[tool_name] = params
    return cached


def _tool_schema(name: str, tools: Any) -> Optional[Mapping[str, Any]]:
    return _tool_schema_map(_tools_key(tools)).get(name)


def _schema_properties(schema: Any) -> Optional[Mapping[str, Any]]:
    return schema.get("properties") if _is_mapping(schema) and _is_mapping(schema.get("properties")) else None


def _schema_types(schema: Any) -> Set[str]:
    types: Set[str] = set()
    if not _is_mapping(schema):
        return types
    kind = schema.get("type")
    if isinstance(kind, str):
        types.add(kind)
    elif isinstance(kind, list):
        types.update(item for item in kind if isinstance(item, str))
    if schema.get("properties") is not None:
        types.add("object")
    if schema.get("items") is not None:
        types.add("array")
    for key in ("anyOf", "oneOf", "allOf"):
        if isinstance(schema.get(key), list):
            for variant in schema[key]:
                types.update(_schema_types(variant))
    return types


def _required_tool_args(name: str, tools: Any) -> List[str]:
    seen: Set[str] = set()
    required: List[str] = []

    def add(*keys: Any) -> None:
        for key in keys:
            if isinstance(key, str) and key and key not in seen:
                seen.add(key)
                required.append(key)

    schema = _tool_schema(name, tools)
    if _is_mapping(schema) and isinstance(schema.get("required"), list):
        add(*schema["required"])
    if name == "Read":
        add("file_path")
    elif name == "Write":
        add("file_path", "content")
    elif name == "Edit":
        add("file_path")
    elif name in {"Bash", "PowerShell"}:
        add("command")
    return required


def _missing_required_args(name: str, input: Any, tools: Any) -> bool:
    if not _is_mapping(input):
        return False
    for key in _required_tool_args(name, tools):
        value = input.get(key)
        if value is None:
            return True
        if isinstance(value, str) and not value.strip() and not _required_arg_allows_empty_string(name, key):
            return True
    return False


def _required_arg_allows_empty_string(tool_name: str, argument_name: str) -> bool:
    return _tool_alias_key(tool_name) in {"write", "writefile", "createfile"} and _tool_alias_key(
        argument_name
    ) in {"content", "text", "body", "data", "value", "contents", "filecontent"}


def _invalid_tool_args(input: Any) -> bool:
    if not _is_mapping(input):
        return False
    return any(
        _is_path_like_arg_name(key) and _path_like_arg_looks_polluted(str(value or ""))
        for key, value in input.items()
    )


def _is_path_like_arg_name(name: Any) -> bool:
    return _tool_alias_key(name) in {
        "path",
        "filepath",
        "filename",
        "targetfile",
        "file",
        "dir",
        "directory",
        "cwd",
        "workdir",
        "workingdirectory",
    }


def _path_like_arg_looks_polluted(value: str) -> bool:
    trimmed = value.strip()
    if not trimmed or "\0" in trimmed or _PATH_POLLUTION_RE.search(trimmed):
        return True
    lowered = trimmed.lower()
    markers = (
        "<![cdata[",
        "]]>",
        "xyml|",
        "qnml|",
        "tool_calls",
        "invoke name=",
        "parameter name=",
        "</parameter",
        "</invoke",
        "function.name:",
        "function.arguments:",
    )
    return any(marker in lowered for marker in markers)


def _filter_input_for_tool(
    name: str, input: Mapping[str, Any], tools: Sequence[Mapping[str, Any]]
) -> Dict[str, Any]:
    properties = _schema_properties(_tool_schema(name, tools))
    return dict(input) if not properties else {key: value for key, value in input.items() if key in properties}


def _tool_accepts_field(name: str, tools: Any, field: str) -> bool:
    properties = _schema_properties(_tool_schema(name, tools))
    return bool(properties and field in properties)


def _rename_first_present(object: Dict[str, Any], canonical: str, *aliases: Any) -> None:
    if object.get(canonical) is not None:
        return
    for alias in aliases:
        if object.get(alias) is not None:
            object[canonical] = object.pop(alias)
            return


def _normalize_protocol_specs(values: Any, emit_protocol: str) -> List[ProtocolSpec]:
    out: List[ProtocolSpec] = []
    seen: Set[str] = set()
    for value in _as_list(values):
        spec = _normalize_protocol_spec(value)
        key = spec.name.lower()
        if key not in seen:
            seen.add(key)
            out.append(spec)
    if str(emit_protocol).lower() not in seen:
        out.insert(0, ProtocolSpec(emit_protocol))
    return out


def _normalize_protocol_spec(value: Union[str, ProtocolSpec, Mapping[str, Any]]) -> ProtocolSpec:
    if isinstance(value, ProtocolSpec):
        return value
    if isinstance(value, str):
        return ProtocolSpec(value)
    if _is_mapping(value):
        options = dict(value)
        name = options.pop("name", None)
        return ProtocolSpec(name, **options)
    raise TypeError("Invalid protocol spec")


@lru_cache(maxsize=64)
def _protocol_open_tag_re(protocol_name: str, tag: str) -> re.Pattern[str]:
    # 协议名放宽为任意标识符: 容忍 `<xy tool_calls>`(空格分隔) 与
    # `<||DSML||tool_calls>`(双竖线) 等上游协议变体; 标签名仍由调用方的
    # tag 实参白名单约束(root/invoke/parameter/tool), 避免纯文本误判。
    return re.compile(
        r"<\s*(?:[|:]{1,2})?\s*(?:[A-Za-z0-9_]+[|:])?\s*[A-Za-z0-9_]+\s*(?:[|:]{1,2}|\s+)\s*"
        + re.escape(tag)
        + r"\b[^>]*>",
        re.IGNORECASE,
    )


@lru_cache(maxsize=32)
def _protocol_any_open_tag_re(protocol_name: str, root: str, invoke: str, parameter: str) -> re.Pattern[str]:
    tag_words = sorted([root, invoke, parameter], key=len, reverse=True)
    tag_alt = "|".join(re.escape(word) for word in tag_words)
    # 前导段与标签名之间的分隔符必须是强分隔符 `|`/`:`(1~2 个), 不能用 `\s+`:
    # 若允许空格, 可选前缀段 `(?:[A-Za-z0-9_]+[|:])?` 会贪婪吞掉 `<|XYML|invoke` 的
    # `XYML|`, 名位落空后倒退, 名槽再吃掉 `invoke`, 最终把 `name` 误判成标签名
    # (attrs 变成 `="X"`), 导致 `_iter_protocol_blocks` 对带 `name=` 属性的标准
    # XYML 块 parse 不出任何 invoke/parameter 子块 (见 Ticket 05 依赖)。
    return re.compile(
        r"<\s*(?:[|:]{1,2})?\s*(?:[A-Za-z0-9_]+[|:])?\s*[A-Za-z0-9_]+\s*[|:]{1,2}\s*(?P<tag>"
        + tag_alt
        + r"|[A-Za-z_][A-Za-z0-9_.:-]*)\b(?P<attrs>[^>]*)>",
        re.IGNORECASE,
    )


@lru_cache(maxsize=32)
def _protocol_any_close_tag_re(protocol_name: str, root: str, invoke: str, parameter: str) -> re.Pattern[str]:
    tag_words = sorted([root, invoke, parameter], key=len, reverse=True)
    tag_alt = "|".join(re.escape(word) for word in tag_words)
    # SPEC-21: require at least one | or : separator so HTML/XML close tags
    # (</ZStack>, </script>, </body>) are NOT matched as protocol close tags.
    # The original R7 widening made the separator optional AND the tag name
    # optional, so any </identifier> matched.  Now the close tag must carry a
    # | or : separator — either before the protocol name, after it, or both.
    return re.compile(
        r"<\s*/\s*(?:"
        r"[|:]{1,2}\s*[A-Za-z0-9_]+\s*[|:]{1,2}\s*"  # </|XYML|tag> or </:XYML:tag>
        r"|"
        r"[A-Za-z0-9_]+\s*[|:]{1,2}\s*"  # </XYML|tag> (separator after name)
        r"|"
        r"[|:]{1,2}\s*[A-Za-z0-9_]+\s*"  # </|XYML> (separator before name, tag omitted)
        r")"
        r"(?:" + tag_alt + r"|[A-Za-z_][A-Za-z0-9_.:-]*)?"
        r"\s*>",
        re.IGNORECASE,
    )


@lru_cache(maxsize=32)
def _parameter_name_tag_re(protocol_name: str) -> re.Pattern[str]:
    return re.compile(
        r"<\s*(?:[|:]{1,2})?\s*(?:[A-Za-z0-9_]+[|:])?\s*[A-Za-z0-9_]+\s*(?:[|:]{1,2}|\s+)\s*(?P<name>[A-Za-z_][A-Za-z0-9_.:-]*)\b[^>]*>\s*"
        r"(?P<val>[\s\S]*?)\s*"
        r"</\s*(?:[|:]{1,2})?\s*(?:[A-Za-z0-9_]+[|:])?\s*[A-Za-z0-9_]+\s*(?:[|:]{1,2}|\s+)\s*(?:parameter|(?P=name))\s*>",
        re.IGNORECASE,
    )


@lru_cache(maxsize=32)
def _bare_protocol_marker_re(protocol_name: str) -> re.Pattern[str]:
    # 仅拓宽分隔符(单→双竖线/双冒号); 协议名仍按字面量匹配, 避免把纯文本
    # 里的 `<xy>`/`<DSML>` 裸片段误判为工具块前缀(防误识别回归, R33)。
    return re.compile(
        r"<\s*(?:[|:]{1,2})?\s*(?:[A-Za-z0-9_]+[|:])?\s*"
        + re.escape(protocol_name)
        + r"\s*(?:[|:]{1,2})?\s*>",
        re.IGNORECASE,
    )


def _compute_cdata_bounds(text: str) -> Optional[Tuple[int, int]]:
    """Return ``(start, end)`` for the single spanning CDATA section in ``text``.

    Shared by ``_cdata_sections`` and ``_extract_cdata_from_value``.  When more
    ``<![cdata[`` openers exist than matched ``_CDATA_RE`` pairs, an inner
    ``]]>`` (from a nested protocol literal) has prematurely closed an outer
    CDATA section — switch to greedy mode: take everything from the opener up
    to the last closer (SPEC-10/R24).  If no closer exists at all (SPEC-10/11:
    model omitted ``]]>`` and wrote a protocol close tag instead), extend the
    range only to the first protocol close tag, not to end of text — otherwise
    legitimate close tags (``</|XYML|invoke>``, ``</|XYML|tool_calls>``) get
    masked too, breaking the LIFO scanner's ability to find any block.

    SPEC-16: greedy only fires when the surplus opener is genuinely nested —
    it lies inside an already-matched pair AND an orphan ``]]>`` remains
    unpaired.  A narrative literal ``<![CDATA[`` mention (backticked prose, a
    command string) is a surplus opener with no pair to anchor to, so it must
    NOT trigger greedy — that would mask the whole protocol block.  A bare
    unclosed opener at the very start of the value (model omitted ``]]>``) is
    still honored via the protocol-close / end-of-value span below; a mid-value
    bare opener with no protocol close tag is a literal, not an unclosed CDATA.

    ``end`` is the exclusive *content* end: for a greedy close it points at the
    ``]]>`` itself (the closer is not part of the CDATA content), for the
    unclosed branch at the protocol-close-tag start.  ``_cdata_sections`` uses
    ``(start, end)`` as the opaque mask range (equivalent to the pre-SPEC-12
    range that included the ``]]>`` — no protocol tag can begin inside ``]]>``
    because it contains no ``<``); ``_extract_cdata_from_value`` slices
    ``text[start+9:end]``.

    Returns ``None`` when there is no spanning CDATA section (sibling CDATA
    sections handled by the non-greedy ``_CDATA_RE`` pass instead).
    """
    # Case-insensitive: ``_CDATA_RE`` uses IGNORECASE, so the plain-string
    # count/find/rfind must match ``<!cdata[`` / ``]]>`` variants too.
    lower = text.lower()
    opener = "<![cdata["
    closer = "]]>"
    matches = list(_CDATA_RE.finditer(text))
    if lower.count(opener) <= len(matches):
        # SPEC-20: even when opener count <= matched pairs, an "extra closer"
        # may exist — the parameter value itself contains a ``]]>`` sequence
        # (array subscript ``data[0]]]>``, string literal ``x = "]]>"``).
        # The non-greedy _CDATA_RE truncates at the first ``]]>``, dropping
        # the inner ``]]>`` and the real closer.  Detect: when the ``]]>``
        # total exceeds the matched-pair count, use the last ``]]>`` as the
        # greedy end.
        closer_count = lower.count(closer)
        if closer_count > len(matches) and matches:
            # Extra closer -> value contains ``]]>`` -> end at last ``]]>``.
            last_close = lower.rfind(closer)
            if last_close > matches[0].start():
                return matches[0].start(), last_close
        return None
    first_open = lower.find(opener)
    if first_open < 0:
        return None
    last_close = lower.rfind(closer)
    if last_close > first_open:
        # Greedy candidate.  SPEC-16 anchored guard: only when the surplus
        # opener is genuinely nested inside an already-matched pair AND an
        # orphan closer remains unpaired — otherwise the surplus opener is a
        # narrative/command literal and greedy would mask the protocol block.
        pair_spans = [(m.start(), m.end()) for m in matches]
        consumed_openers = {m.start() for m in matches}
        consumed_closers = {m.end() - len(closer) for m in matches}
        surplus = [
            m.start()
            for m in _CDATA_OPENER_RE.finditer(text)
            if m.start() not in consumed_openers
        ]
        if not surplus:
            return None
        orphans = [
            m.start()
            for m in re.finditer(re.escape(closer), lower)
            if m.start() not in consumed_closers
        ]
        if not orphans:
            return None
        # Greedy start anchors to the first matched pair that contains the
        # surplus opener, not to the narrative literal's position.
        for start, end in pair_spans:
            if any(start < pos < end for pos in surplus):
                return start, last_close
        return None
    # No closer after the first opener (SPEC-10/11): the model omitted the
    # ``]]>``.  A protocol close tag following it defines the span so the LIFO
    # scanner can still find the block structure; a bare opener at the very
    # start of the value spans to end-of-value; anything else (a mid-string
    # literal ``<![CDATA[`` in prose/command text, SPEC-15/16) is not an
    # unclosed CDATA.
    first_proto_close = _PROTO_CLOSE_AFTER_CDATA.search(text, first_open)
    if first_proto_close:
        return first_open, first_proto_close.start()
    if first_open == 0:
        return first_open, len(text)
    return None


def _cdata_sections(text: str) -> List[Tuple[int, int]]:
    """Return ``(start, end)`` ranges covering every CDATA section in ``text``.

    Default: non-greedy matching via ``_CDATA_RE``, compatible with multiple
    sibling CDATA sections (``<![CDATA[a]]>x<![CDATA[b]]>`` -> two ranges).
    R24/SPEC-10/11: when more ``<![CDATA[`` openers exist than matched pairs,
    an inner ``]]>`` (from a nested protocol literal) has prematurely closed an
    outer CDATA section — fall back to the single spanning range computed by
    ``_compute_cdata_bounds``.
    """
    sections = [(match.start(), match.end()) for match in _CDATA_RE.finditer(text)]
    bounds = _compute_cdata_bounds(text)
    if bounds is not None:
        sections = [bounds]
    return sections


def _iter_protocol_blocks(text: str, protocol: ProtocolSpec, tag: str) -> Iterable[Tuple[str, str]]:
    """Yield ``(attrs, body)`` for each properly nested block of ``tag``.

    Delimiters may drift between ``|`` and ``:`` independently on the open and
    close tags (``<|XYML|tool_calls>`` vs ``<:XYML:tool_calls>`` vs
    ``</:XYML>``), and a close tag may omit its name entirely. A LIFO stack
    over every protocol open/close tag keeps the nesting balanced so we never
    terminate a block early or mistreat a bare ``<:``/``<|`` in prose as a
    boundary.

    CDATA sections are opaque to this scanner: a tag-looking string inside one
    (e.g. ``</script>`` in a Python heredoc) is literal content, not a protocol
    boundary.  Every CDATA range is identified up front — the regex matches are
    not necessarily in text order — and any open/close event that falls inside
    one is excluded from the pairing scan.
    """
    cdata_sections = _cdata_sections(text)

    def _inside_cdata(pos: int) -> bool:
        return any(start <= pos < end for start, end in cdata_sections)

    events: List[Tuple[Any, ...]] = []
    events.extend(
        ("open", m.start(), m.end(), m.group("attrs") or "", m.group("tag"))
        for m in _protocol_any_open_tag_re(
            protocol.name,
            protocol.tags["root"],
            protocol.tags["invoke"],
            protocol.tags["parameter"],
        ).finditer(text)
        if not _inside_cdata(m.start())
    )
    events.extend(
        ("close", m.start(), m.end())
        for m in _protocol_any_close_tag_re(
            protocol.name,
            protocol.tags["root"],
            protocol.tags["invoke"],
            protocol.tags["parameter"],
        ).finditer(text)
        if not _inside_cdata(m.start())
    )
    events.sort(key=lambda event: event[1])
    stack: List[Tuple[Any, ...]] = []
    pairings: List[Tuple[Tuple[Any, ...], int]] = []
    for event in events:
        if event[0] == "open":
            # Nesting depth of this tag type: how many same-tag blocks are
            # already open. A block whose value contains a nested protocol
            # block (e.g. a <parameter> whose value is a whole <invoke>) must
            # treat that nested block as literal content, so only blocks at
            # depth 0 are real top-level sections.
            same_depth = sum(1 for item in stack if item[4].lower() == event[4].lower())
            stack.append(event + (same_depth,))
        elif stack:
            pairings.append((stack.pop(), event[1]))
    wanted = tag.lower()
    for opener, close_start in pairings:
        if opener[4].lower() != wanted:
            continue
        if opener[5] != 0:
            continue
        yield opener[3], text[opener[2]:close_start]


def _extract_name_attr(attributes: Any) -> str:
    match = _NAME_ATTR_RE.search(
        str(attributes or "").replace("NAME", "name"),
    )
    if not match:
        return ""
    return _html_unescape(next((value for value in match.groups() if value is not None), "").strip())


def _extract_cdata_from_value(raw: str) -> List[str]:
    """Extract CDATA contents from a parameter value string.

    Default: non-greedy findall, compatible with multiple sibling CDATA
    sections (``<![CDATA[a]]>x<![CDATA[b]]>`` -> ``["a", "b"]``).

    R24/SPEC-10/11: when more ``<![CDATA[`` openers exist than matched pairs,
    an inner ``]]>`` (from a nested protocol literal) has prematurely closed an
    outer CDATA section.  Fall back to the single spanning range computed by
    ``_compute_cdata_bounds`` and strip the ``<![CDATA[`` prefix — the span is
    shared with ``_cdata_sections`` so both functions stay consistent.
    """
    raw_str = str(raw or "")
    cdata_matches = _CDATA_RE.findall(raw_str)
    bounds = _compute_cdata_bounds(raw_str)
    if bounds is not None:
        first_open, end = bounds
        cdata_matches = [raw_str[first_open + len("<![CDATA["):end]]
    return cdata_matches


def _strip_nested_cdata_markers(raw: str, require_closer: bool = True) -> Optional[str]:
    """SPEC-15: loop-peel remaining CDATA openers + ``]]>``/``]>`` tail residue.

    Called from two paths in _decode_markup_value:
    - extracted-CDATA path (require_closer=False): closer already consumed by bounds
    - fallback path (require_closer=True): value must be CDATA-shaped end-to-end

    Returns None when the value is not CDATA-shaped (no leading opener, or — in
    the require_closer path — no trailing closer), so callers fall through to
    their plain-text handling.
    """
    text = str(raw or "")
    if not text or not _CDATA_OPENER_RE.match(text):
        return None
    if require_closer and not re.search(r"(?:]]>|]>)[ \t]*$", text):
        return None
    while True:
        match = _CDATA_OPENER_RE.match(text)
        if not match:
            break
        text = text[match.end():]
    return re.sub(r"(?:]]>|]>)[ \t]*$", "", text)


def _strip_colon_marker_residue(text: str) -> str:
    """Strip standalone ``<:>`` residue lines from content text (SPEC-17)."""
    return _COLON_MARKER_RESIDUE_RE.sub("", str(text or ""))


def _decode_markup_value(raw: Any, parameter_name: Any, config: ToolCallConfig) -> Any:
    raw_str = str(raw or "")
    cdata_matches = _extract_cdata_from_value(raw_str)
    raw_string = str(parameter_name or "").lower() in config.raw_string_params
    if cdata_matches:
        joined = "".join(cdata_matches)
        # SPEC-15: loop-peel remaining CDATA openers + tail residue from the
        # extracted content (inner opener prefix / `]]>` / `]>` residue).
        peeled = _strip_nested_cdata_markers(joined, require_closer=False)
        if peeled is not None:
            joined = peeled
        return joined if raw_string else _coerce_markup_scalar(joined, raw_string=False)
    if not raw_string:
        parsed, nested = _parse_nested_markup_value(raw_str, config)
        if parsed:
            return nested
    # SPEC-14: strip nested collapsed marker from parameter values.
    # Model sometimes embeds <||DSML|| name="command">VALUE</||DSML||>
    # inside a standard XYML parameter value (replacing CDATA).
    collapsed = _COLLAPSED_PARAM_RE.search(raw_str)
    if collapsed:
        inner = raw_str[:collapsed.start()] + collapsed.group(4) + raw_str[collapsed.end():]
        return inner if raw_string else _coerce_markup_scalar(inner, raw_string=False)
    # SPEC-15 fallback: the value is CDATA-shaped end-to-end but the closer was
    # non-standard (`]>` residue), so the widened _CDATA_RE did not match.
    peeled = _strip_nested_cdata_markers(raw_str, require_closer=True)
    if peeled is not None:
        return peeled if raw_string else _coerce_markup_scalar(peeled, raw_string=False)
    return _coerce_markup_scalar(raw, raw_string=raw_string)


def _parse_nested_markup_value(raw: str, config: ToolCallConfig) -> Tuple[bool, Any]:
    text = raw.strip()
    if not text or "<" not in text:
        return False, None
    matches = list(_XML_TAG_BODY_RE.finditer(text))
    if not matches:
        return False, None
    names = [match.group(1) for match in matches]
    values = [_decode_markup_value(match.group(2), match.group(1), config) for match in matches]
    if all(name.lower() == "item" for name in names):
        return True, values
    out: Dict[str, Any] = {}
    for name, value in zip(names, values):
        if name not in out:
            out[name] = value
        elif isinstance(out[name], list):
            out[name].append(value)
        else:
            out[name] = [out[name], value]
    return True, out


def _coerce_markup_scalar(raw: Any, raw_string: bool) -> Any:
    value = _html_unescape(str(raw or "").strip())
    if raw_string:
        return value
    if value.lower() == "true":
        return True
    if value.lower() == "false":
        return False
    if value.lower() == "null":
        return None
    ok, parsed = _try_json(value)
    return _normalize_tool_input(parsed) if ok else value


def _render_markup_value(value: Any) -> str:
    if value is None:
        return "null"
    if isinstance(value, str):
        return "<![CDATA[{}]]>".format(value.replace("]]>", "]]]]><![CDATA[>"))
    if isinstance(value, bool):
        return "true" if value else "false"
    return _json_dumps(value)


def _first_tool_marker_index(text: str, config: ToolCallConfig) -> int:
    indexes: List[int] = []
    for protocol in config.parse_protocols:
        for tag in (
            protocol.tags["root"],
            protocol.tags["invoke"],
            protocol.tags["parameter"],
        ):
            match = _protocol_open_tag_re(protocol.name, tag).search(text)
            if match:
                indexes.append(match.start())
    # Also detect bare protocol markers like <:XYML> or <|XYML> that the model
    # emits as a preamble before the actual tool_calls block. These don't carry
    # a tag name (root/invoke/parameter) so the regexes above miss them. When
    # found, treat them as a tool marker so the sieve holds the text instead of
    # leaking the bare marker into content.
    for protocol in config.parse_protocols:
        match = _bare_protocol_marker_re(protocol.name).search(text)
        if match:
            indexes.append(match.start())
    for marker_re in (_JSON_TOOL_MARKER_RE, _FUNC_NAME_MARKER_RE):
        match = marker_re.search(text)
        if match:
            indexes.append(match.start())
    # SPEC-09: mixed protocol-name tags like <||DSML|||XYML|tool_calls> escape
    # every structured regex above (they allow at most one protocol prefix).
    # Any <...|...> tag whose tag word is a known structural name is a tool
    # marker — normal prose never carries a protocol separator before one — so
    # the sieve captures the block instead of leaking its prefix as content.
    # Purely additive: it only widens the set of text that enters capturing
    # mode, never the parse output.
    match = _LOOSE_TAG_MARKER_RE.search(text)
    if match:
        indexes.append(match.start())
    return min(indexes) if indexes else -1


def _has_open_protocol_block(text: str, config: ToolCallConfig) -> bool:
    # Keep this symmetric with _first_tool_marker_index: every marker that can
    # put the sieve into capturing mode must ALSO report an open block here —
    # otherwise _consume_capture skips its hold-guard and re-parses the partial
    # capture on every chunk, emitting a premature tool_call (from a half-open
    # parameter) and leaking the block's tail as content.
    #
    # SPEC-09: mixed protocol-name tags (<||DSML|||XYML|tool_calls>) escape the
    # structured open-tag regexes above (they allow one protocol prefix).
    # SPEC-12: JSON tool_calls ({"tool_calls": ...}) and function.name: markers
    # are detected by _first_tool_marker_index too — a bare protocol marker has
    # no structural tag word and JSON markers no angle brackets, so a capture
    # held on them would otherwise re-parse on every chunk.
    return any(
        _protocol_open_tag_re(protocol.name, protocol.tags["root"]).search(text)
        or _protocol_open_tag_re(protocol.name, protocol.tags["invoke"]).search(text)
        or _protocol_open_tag_re(protocol.name, protocol.tags["parameter"]).search(text)
        for protocol in config.parse_protocols
    ) or any(
        _bare_protocol_marker_re(protocol.name).search(text)
        for protocol in config.parse_protocols
    ) or bool(_JSON_TOOL_MARKER_RE.search(text)) or bool(
        _FUNC_NAME_MARKER_RE.search(text)
    ) or bool(_LOOSE_TAG_MARKER_RE.search(text))


def _looks_structurally_closed(text: str, config: ToolCallConfig) -> bool:
    if _STRUCT_CLOSE_RE.search(text):
        return True
    for protocol in config.parse_protocols:
        if _protocol_any_close_tag_re(
                protocol.name,
                protocol.tags["root"],
                protocol.tags["invoke"],
                protocol.tags["parameter"],
            ).search(text):
            return True
    return False


def _canonicalize_markup(text: str) -> str:
    for old, new in MARKUP_REPLACEMENTS:
        text = text.replace(old, new)
    # SPEC-18: split fused protocol tags (<xymltool_calls> \u2192 <|xyml|tool_calls>)
    text = _FUSED_TAG_RE.sub(
        lambda m: m.group(1) + "|" + m.group(2) + "|" + m.group(4) + m.group(5),
        text,
    )
    return text.replace("\u200b", "").replace("\u200c", "").replace("\u200d", "").replace("\ufeff", "").replace("\u3000", " ").replace("\u00a0", " ")


def _strip_markdown_fences(text: str) -> str:
    return _MD_FENCE_RE.sub(r"\1", text)


def _strip_json_fence(text: str) -> str:
    match = _JSON_FENCE_RE.fullmatch(text.strip())
    return match.group(1).strip() if match else text.strip()


def _repair_loose_json(text: str) -> str:
    repaired = text.strip()
    repaired = _REPAIR_NAME_EQ_RE.sub('"name": "', repaired)
    repaired = _REPAIR_NAME_EQ2_RE.sub(r'"name": "\1"', repaired)
    repaired = _REPAIR_KEY_EQ_RE.sub(r'"\1": ', repaired)
    return _REPAIR_KEY_COLON_RE.sub(r'\1"\2":', repaired)


def _recover_json_like(text: str) -> str:
    repaired = text.strip()
    unclosed_braces = repaired.count("{") - repaired.count("}")
    unclosed_brackets = repaired.count("[") - repaired.count("]")
    return repaired + ("]" * max(unclosed_brackets, 0)) + ("}" * max(unclosed_braces, 0))


def _try_json(text: Any) -> Tuple[bool, Any]:
    try:
        return True, json.loads(text)
    except (TypeError, ValueError, json.JSONDecodeError):
        return False, None


def _resolve_config(config: Optional[Union[ToolCallConfig, Mapping[str, Any]]]) -> ToolCallConfig:
    if isinstance(config, ToolCallConfig):
        return config
    if config is None:
        return ToolCallConfig()
    if _is_mapping(config):
        return ToolCallConfig(config)
    raise TypeError("config must be a ToolCallConfig or mapping")


def _safe_tool_name(name: Any) -> str:
    trimmed = str(name or "").strip()
    if not trimmed:
        return ""
    if trimmed in SAFE_TOOL_ALIASES:
        return SAFE_TOOL_ALIASES[trimmed]
    if any(alias.lower() == trimmed.lower() for alias in SAFE_TOOL_ALIASES.values()):
        return trimmed
    return trimmed if trimmed.startswith("u_") else "u_{}".format(trimmed)


def _example_input_from_tool(tool: Mapping[str, Any]) -> Dict[str, Any]:
    properties = _schema_properties(tool.get("parameters") or tool.get("input_schema"))
    if not properties:
        return {"ARG": "value"}
    example = {key: _example_value(schema) for key, schema in list(properties.items())[:3]}
    return example or {"ARG": "value"}


def _example_value(schema: Any) -> Any:
    kinds = _schema_types(schema)
    if "array" in kinds:
        return []
    if "object" in kinds:
        return {}
    if "boolean" in kinds:
        return True
    if "number" in kinds or "integer" in kinds:
        return 1
    return "value"


def _summarize_schema(schema: Any) -> str:
    return "{}" if not schema else _json_dumps(schema)


def _clip(text: Any, maximum: int) -> str:
    value = str(text or "").strip()
    return "{}...".format(value[:maximum]) if len(value) > maximum else value


def _stable_stringify(value: Any) -> str:
    if isinstance(value, list):
        return "[{}]".format(",".join(_stable_stringify(item) for item in value))
    if _is_mapping(value):
        return "{{{}}}".format(
            ",".join(
                "{}:{}".format(_json_dumps(str(key)), _stable_stringify(value[key]))
                for key in sorted(value, key=lambda item: str(item))
            )
        )
    return _json_dumps(value)


def _json_dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), default=str)


def _escape_xml(value: Any) -> str:
    return (
        str(value or "")
        .replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
        .replace("'", "&apos;")
    )


def _html_unescape(value: Any) -> str:
    return (
        str(value or "")
        .replace("&quot;", '"')
        .replace("&apos;", "'")
        .replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&amp;", "&")
    )


def _tool_alias_key(value: Any) -> str:
    return _ALIAS_KEY_RE.sub("", str(value or "").strip().lower())


def _first_string(*values: Any) -> str:
    for value in values:
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def _first_defined(*values: Any) -> Any:
    return next((value for value in values if value is not None), None)


def _take_option(values: Dict[str, Any], *names: str, default: Any = None) -> Any:
    for name in names:
        if name in values:
            return values.pop(name)
    return default


def _as_list(value: Any) -> List[Any]:
    if isinstance(value, list):
        return value
    if value is None:
        return []
    return [value]


def _is_mapping(value: Any) -> bool:
    return isinstance(value, Mapping)


# P1-2: ToolCallConfig.default() is immutable-after-construction and sits on
# _parse_tool_input's hot path (called once per XML-like parameter block).
# Construct it once at module load instead of per invocation.
_DEFAULT_CFG = ToolCallConfig.default()


def _call_value(call: Any, key: str, default: Any = None) -> Any:
    if _is_mapping(call):
        return call.get(key, default)
    return getattr(call, key, default)


def _arguments_string(value: Any) -> str:
    return value if isinstance(value, str) else _json_dumps({} if value is None else value)
