"""Rough token estimation without hard tiktoken dependency."""

from __future__ import annotations

from typing import Any, Dict, List, Optional

from ..models.canonical import CanonicalRequest
from .cache import enrich_usage


def estimate_tokens(text: Any) -> int:
    if text is None:
        return 0
    if not isinstance(text, str):
        text = str(text)
    text = text.strip()
    if not text:
        return 0
    # ~4 chars per token heuristic
    return max(1, (len(text) + 3) // 4)


def messages_text(req: CanonicalRequest) -> str:
    """Serialize request messages to a single string for prompt-token estimation."""
    parts: List[str] = []
    for msg in req.messages:
        if msg.raw:
            parts.append(str(msg.raw))
            continue
        content = msg.content
        if isinstance(content, list):
            content = " ".join(str(c) for c in content)
        parts.append(f"{msg.role}: {content}")
        for tc in msg.tool_calls:
            parts.append(f"{tc.name}: {tc.arguments}")
    return "\n".join(parts)


def _is_zero_usage(usage: Optional[Dict[str, Any]]) -> bool:
    if not usage:
        return True
    values = [
        usage.get("prompt_tokens"),
        usage.get("completion_tokens"),
        usage.get("total_tokens"),
        usage.get("input_tokens"),
        usage.get("output_tokens"),
    ]
    return all(not v for v in values)


def resolve_usage(
    *,
    upstream: Optional[Dict[str, Any]],
    prompt_text: str,
    completion_text: str,
) -> Dict[str, Any]:
    """Return best-effort usage: real upstream values if present, else local estimate.

    Real upstream usage wins (never overwritten); fall back to a char-count
    estimate so CPA monitoring always sees non-zero token stats.

    Passthrough branch copies the entire upstream dict and normalizes the three
    primary keys (prompt_tokens / completion_tokens / total_tokens), preserving
    any extra fields (e.g. cached_tokens, prompt_tokens_details) so downstream
    surfaces can render cache statistics. Then enriches with local cache
    estimation for any missing cache fields.
    """
    if not _is_zero_usage(upstream):
        u = upstream or {}
        out: Dict[str, Any] = dict(u)
        pt = u.get("prompt_tokens") or u.get("input_tokens") or 0
        ct = u.get("completion_tokens") or u.get("output_tokens") or 0
        out["prompt_tokens"] = int(pt)
        out["completion_tokens"] = int(ct)
        out["total_tokens"] = int(u.get("total_tokens") or (pt + ct))
        # 本地缓存丰富（上游有缓存字段则保留，无则本地估算）
        out = enrich_usage(out, prompt_text, int(pt))
        return out

    pt = estimate_tokens(prompt_text)
    ct = estimate_tokens(completion_text)
    out = {
        "prompt_tokens": pt,
        "completion_tokens": ct,
        "total_tokens": pt + ct,
    }
    # 本地估算分支也添加缓存字段
    out = enrich_usage(out, prompt_text, pt)
    return out
