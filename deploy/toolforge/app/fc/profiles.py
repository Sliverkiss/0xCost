"""CLI tool-profile detection and prompt augmentation (Phase 3)."""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Dict, List, Set

from ..models.canonical import ToolDef

_NAME_RE = re.compile(r"[^a-z0-9]+")


@dataclass
class ToolProfile:
    id: str
    display_name: str
    rules: List[str] = field(default_factory=list)


def _norm(name: str) -> str:
    return _NAME_RE.sub("", (name or "").lower())


def _names(tools: List[ToolDef]) -> Set[str]:
    return {_norm(t.name) for t in tools if t.name}


def detect_tool_profile(tools: List[ToolDef]) -> ToolProfile:
    names = _names(tools)
    if {"skillslist", "skillview"} & names or {"readfile", "terminal", "writefile"} <= names:
        return ToolProfile(
            id="hermes",
            display_name="Hermes",
            rules=[
                "Use exact Hermes tool names from the list; never invent Claude Code names.",
                "Prefer skills_list/skill_view when exploring skills.",
            ],
        )
    if {"sessionsspawn", "exec"} & names or ("process" in names and "exec" in names):
        return ToolProfile(
            id="openclaw",
            display_name="OpenClaw",
            rules=[
                "Map file work to available read/write/exec tools only.",
                "Use process only for background job control.",
            ],
        )
    if {"bash", "read", "write"} <= names or {"task", "agent"} & names:
        return ToolProfile(
            id="claude_code",
            display_name="Claude Code",
            rules=[
                "Use exact PascalCase tool names (Read/Write/Bash/...) as listed.",
                "Prefer direct tools over Task/Agent when a single step suffices.",
            ],
        )
    if {"bash", "read", "write", "edit"} & names and "todowrite" in names:
        return ToolProfile(
            id="opencode",
            display_name="OpenCode",
            rules=[
                "Use exact lowercase tool names from the list.",
                "todowrite is for planning only, not for file changes.",
            ],
        )
    return ToolProfile(
        id="generic",
        display_name="Generic CLI",
        rules=[
            "Use the exact action names listed; do not translate to another CLI's names.",
        ],
    )


def profile_instruction_block(profile: ToolProfile) -> str:
    lines = [
        f"[CLIENT TOOL PROFILE: {profile.display_name}]",
        *profile.rules,
    ]
    return "\n".join(lines)


def build_few_shot_block(tools: List[ToolDef], *, max_examples: int = 2) -> str:
    """Minimal few-shot using first N non-control tools."""
    skip = {"todo", "task", "agent", "cron", "skill"}
    picked: List[ToolDef] = []
    for tool in tools:
        key = _norm(tool.name)
        if any(s in key for s in skip):
            continue
        picked.append(tool)
        if len(picked) >= max_examples:
            break
    if len(picked) < 1:
        return ""
    lines = ["[FEW-SHOT]", "When you need a tool, emit the protocol block immediately."]
    for tool in picked:
        props = (tool.parameters or {}).get("properties") if isinstance(tool.parameters, dict) else {}
        sample_args: Dict[str, str] = {}
        if isinstance(props, dict):
            for key in list(props.keys())[:2]:
                sample_args[str(key)] = "..."
        arg_hint = ", ".join(f'{k}="..."' for k in sample_args) or "..."
        lines.append(f'- Example tool: {tool.name}({arg_hint})')
    return "\n".join(lines)
