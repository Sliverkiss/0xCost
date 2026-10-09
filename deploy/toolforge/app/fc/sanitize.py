"""Claude Code system-prompt 净化: 剥离 CodeBuddy 黑名单指纹。"""
from __future__ import annotations
import re
from typing import Any, Dict, List

# 特征串: 任一命中才进入净化 (C 层 in 查找, 普通请求全不中 → 零开销)
_CC_FEATURES = ("x-anthropic-billing-header", "cc_entrypoint=",
                "You are Claude Code", "Main branch (")

# 剥离层: billing header 键名即触发 (与值无关)
_BILLING_HDR_RE = re.compile(r"x-anthropic-billing-header:[^;\n]*;?\s*", re.IGNORECASE)
_BILLING_KV_RE  = re.compile(r"\bcc_[a-z0-9_]+=[^;\n]*;?\s*", re.IGNORECASE)

# 改写层: 全模板句逐字替换
_TEMPLATE_REWRITES = [
    ("You are Claude Code, Anthropic's official CLI for Claude.",
     "You are Claude Code, Anthropic's official CLI tool for Claude."),
    ("Main branch (you will usually use this for PRs)",
     "Default branch (you will usually use this for PRs)"),
]

def _sanitize_text(text: str) -> str:
    """快速预检; 无特征 → 返回原串 (零分配)。"""
    if not any(f in text for f in _CC_FEATURES):
        return text
    for old, new in _TEMPLATE_REWRITES:
        if old in text:
            text = text.replace(old, new)
    if _BILLING_HDR_RE.search(text):
        text = _BILLING_HDR_RE.sub("", text)
        prev = None
        while prev != text:          # 清尾随裸 kv (cc_version=...; cc_entrypoint=...;)
            prev = text
            text = _BILLING_KV_RE.sub("", text)
    return text.strip()

def _sanitize_content(content: Any) -> Any:
    if isinstance(content, str):
        return _sanitize_text(content)
    if isinstance(content, list):     # 多模态: 只动 text part
        changed = False
        parts = []
        for p in content:
            if isinstance(p, dict) and isinstance(p.get("text"), str):
                s = _sanitize_text(p["text"])
                if s is not p["text"]:
                    p = dict(p, text=s); changed = True
            parts.append(p)
        return parts if changed else content
    return content

def sanitize_wire_body(body: Dict[str, Any]) -> Dict[str, Any]:
    """净化 wire body 的 messages。无命中 → 原对象返回 (零拷贝)。"""
    messages = body.get("messages")
    if not isinstance(messages, list):
        return body
    out: List[Dict[str, Any]] = []
    changed = False
    for msg in messages:
        if not isinstance(msg, dict):
            out.append(msg); continue
        c = msg.get("content")
        nc = _sanitize_content(c)
        if nc is c:
            out.append(msg)           # 无命中: 共享原对象
        else:
            changed = True
            m = dict(msg); m["content"] = nc; out.append(m)
    return dict(body, messages=out) if changed else body