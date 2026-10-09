"""Tests for app.util.tokens.resolve_usage."""

import pytest

from app.util.cache import _reset_cache
from app.util.tokens import resolve_usage


@pytest.fixture(autouse=True)
def reset_cache():
    _reset_cache()
    yield
    _reset_cache()


def test_resolve_usage_preserves_extra_fields():
    upstream = {
        "prompt_tokens": 100,
        "completion_tokens": 50,
        "total_tokens": 150,
        "cached_tokens": 80,
        "prompt_tokens_details": {"cached_tokens": 80},
    }
    result = resolve_usage(upstream=upstream, prompt_text="", completion_text="")
    assert result["prompt_tokens"] == 100
    assert result["cached_tokens"] == 80
    assert result["prompt_tokens_details"] == {"cached_tokens": 80}


def test_resolve_usage_estimate_no_extra_fields():
    result = resolve_usage(upstream=None, prompt_text="hello", completion_text="world")
    # 估算分支现在也包含本地缓存字段
    assert result["prompt_tokens"] > 0
    assert result["completion_tokens"] > 0
    assert result["total_tokens"] == result["prompt_tokens"] + result["completion_tokens"]
    assert "cached_tokens" in result
    assert "prompt_cache_miss_tokens" in result


def test_resolve_usage_includes_cache_fields():
    from app.util.tokens import resolve_usage

    # 第一次调用（缓存未命中）
    result1 = resolve_usage(upstream=None, prompt_text="hello", completion_text="hi")
    assert result1["prompt_cache_miss_tokens"] > 0
    assert result1["cached_tokens"] == 0
    # 第二次调用（缓存命中）
    result2 = resolve_usage(upstream=None, prompt_text="hello", completion_text="hi")
    assert result2["prompt_cache_hit_tokens"] > 0
    assert result2["cached_tokens"] > 0


def test_resolve_usage_upstream_cache_preserved():
    from app.util.tokens import resolve_usage

    upstream = {"prompt_tokens": 100, "completion_tokens": 50, "cached_tokens": 80}
    result = resolve_usage(upstream=upstream, prompt_text="hello", completion_text="hi")
    assert result["cached_tokens"] == 80  # 上游值保留

