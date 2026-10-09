"""Local prompt cache estimation for usage stats."""

from __future__ import annotations

import hashlib
import time
from typing import Any, Dict, Tuple

# 内存缓存: {prompt_hash: (timestamp, prompt_tokens)}
_CACHE: Dict[str, Tuple[float, int]] = {}
_CACHE_TTL = 3600  # 1小时过期


def _reset_cache() -> None:
    """清空缓存（仅用于测试）."""
    _CACHE.clear()


def _hash_prompt(prompt_text: str) -> str:
    """生成 prompt 内容哈希."""
    return hashlib.sha256(prompt_text.encode()).hexdigest()[:32]


def _cleanup_expired() -> None:
    """清理过期缓存."""
    now = time.time()
    expired = [k for k, (ts, _) in _CACHE.items() if now - ts > _CACHE_TTL]
    for k in expired:
        del _CACHE[k]


def estimate_cache_tokens(
    prompt_text: str,
    prompt_tokens: int,
) -> Dict[str, int]:
    """估算缓存 token 统计.

    相同 prompt 第二次请求时视为缓存命中.
    返回 OpenAI 标准缓存字段.
    """
    _cleanup_expired()

    key = _hash_prompt(prompt_text)
    now = time.time()

    if key in _CACHE:
        # 缓存命中
        _, cached_pt = _CACHE[key]
        return {
            "cached_tokens": cached_pt,
            "cache_read_input_tokens": cached_pt,
            "cache_creation_input_tokens": 0,
            "prompt_cache_hit_tokens": cached_pt,
            "prompt_cache_miss_tokens": 0,
            "prompt_cache_write_tokens": 0,
        }

    # 缓存未命中，记录本次 prompt
    _CACHE[key] = (now, prompt_tokens)
    return {
        "cached_tokens": 0,
        "cache_read_input_tokens": 0,
        "cache_creation_input_tokens": prompt_tokens,
        "prompt_cache_hit_tokens": 0,
        "prompt_cache_miss_tokens": prompt_tokens,
        "prompt_cache_write_tokens": prompt_tokens,
    }


def enrich_usage(
    usage: Dict[str, Any],
    prompt_text: str,
    prompt_tokens: int,
) -> Dict[str, Any]:
    """丰富 usage dict，添加本地缓存估算字段.

    如果上游已有缓存字段，保留上游值；否则用本地估算.
    """
    cache_stats = estimate_cache_tokens(prompt_text, prompt_tokens)

    for key, value in cache_stats.items():
        if key not in usage or not usage[key]:
            usage[key] = value

    # 确保 prompt_tokens_details 存在且包含 cached_tokens
    if "prompt_tokens_details" not in usage:
        usage["prompt_tokens_details"] = {}
    if "cached_tokens" not in usage["prompt_tokens_details"] or not usage["prompt_tokens_details"]["cached_tokens"]:
        usage["prompt_tokens_details"]["cached_tokens"] = usage.get("cached_tokens", 0)

    # 确保 completion_tokens_details 存在
    if "completion_tokens_details" not in usage:
        usage["completion_tokens_details"] = {
            "accepted_prediction_tokens": 0,
            "audio_tokens": 0,
            "reasoning_tokens": 0,
            "rejected_prediction_tokens": 0,
            "cached_tokens": 0,
        }

    return usage
