"""Tests for app.convert responses builders."""

from app.convert import build_responses_response


def test_build_responses_response_cached():
    usage = {"prompt_tokens": 100, "completion_tokens": 50, "cached_tokens": 80}
    result = build_responses_response(model="m", content="hi", usage=usage)
    assert result["usage"]["input_tokens"] == 100
    assert result["usage"]["output_tokens"] == 50
    assert result["usage"]["input_tokens_details"]["cached_tokens"] == 80
