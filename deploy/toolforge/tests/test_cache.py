"""Tests for app.util.cache local prompt cache estimation."""

import pytest

from app.util.cache import _reset_cache, estimate_cache_tokens, enrich_usage


@pytest.fixture(autouse=True)
def reset_cache():
    _reset_cache()
    yield
    _reset_cache()


def test_cache_miss_first_request():
    result = estimate_cache_tokens("hello world", 10)
    assert result["cached_tokens"] == 0
    assert result["prompt_cache_miss_tokens"] == 10
    assert result["prompt_cache_hit_tokens"] == 0
    assert result["cache_creation_input_tokens"] == 10
    assert result["prompt_cache_write_tokens"] == 10
    assert result["cache_read_input_tokens"] == 0


def test_cache_hit_second_request():
    estimate_cache_tokens("hello world", 10)  # 第一次
    result = estimate_cache_tokens("hello world", 10)  # 第二次
    assert result["cached_tokens"] == 10
    assert result["prompt_cache_hit_tokens"] == 10
    assert result["prompt_cache_miss_tokens"] == 0
    assert result["cache_creation_input_tokens"] == 0
    assert result["prompt_cache_write_tokens"] == 0
    assert result["cache_read_input_tokens"] == 10


def test_enrich_usage_preserves_upstream_cache():
    usage = {"prompt_tokens": 100, "cached_tokens": 50}
    result = enrich_usage(usage, "test", 100)
    assert result["cached_tokens"] == 50  # 上游值保留
    assert "prompt_tokens_details" in result
    assert "completion_tokens_details" in result
    assert result["prompt_tokens_details"]["cached_tokens"] == 50
