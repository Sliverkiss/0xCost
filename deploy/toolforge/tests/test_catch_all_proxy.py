"""Tests for the catch-all passthrough (deploy profile: this port is the only one).

The paths this carries are the ones the middleware does not implement, so the
properties that matter are: the request arrives verbatim (method, path, query, body,
the caller's own credential), the response comes back verbatim (status, bytes,
content type), and a dead upstream is an error rather than an empty answer.
"""

import httpx
import pytest

from app.upstream import proxy
from fastapi import FastAPI, Request
from fastapi.testclient import TestClient

from app.config import AppConfig, ClientAuthConfig, FeaturesConfig, UpstreamConfig
from app.upstream.proxy import proxy_request, upstream_origin


class _Chunks(httpx.AsyncByteStream):
    """A streamed body: the proxy relays with aiter_raw(), so mocks must stream."""

    def __init__(self, *chunks: bytes) -> None:
        self._chunks = chunks

    async def __aiter__(self):
        for chunk in self._chunks:
            yield chunk


def _client_factory(transport: httpx.MockTransport):
    def factory(*, timeout=None):
        return httpx.AsyncClient(transport=transport)

    return factory


def _json_response(payload: bytes, status: int = 200) -> httpx.Response:
    return httpx.Response(
        status, stream=_Chunks(payload), headers={"content-type": "application/json"}
    )


def _config(base_url: str = "http://gateway:8080/v1", *, keys=None) -> AppConfig:
    return AppConfig(
        client_authentication=ClientAuthConfig(enabled=bool(keys), allowed_keys=keys or []),
        features=FeaturesConfig(catch_all_proxy=True),
        upstreams=[UpstreamConfig(name="gateway", base_url=base_url, models=["*"], is_default=True)],
    )


def _app(config: AppConfig) -> FastAPI:
    app = FastAPI()

    @app.api_route("/{path:path}", methods=["GET", "POST", "PUT", "PATCH", "DELETE"])
    async def catch_all(request: Request):
        return await proxy_request(request, config)

    return app


def test_the_flag_loads_from_config(tmp_path):
    """The loader builds FeaturesConfig field by field: a missing line = silently off."""
    from app.config import load_config

    path = tmp_path / "config.yaml"
    path.write_text(
        """
server:
  port: 8080
client_authentication:
  enabled: false
upstreams:
  - name: gateway
    base_url: "http://gateway:8080/v1"
    is_default: true
features:
  catch_all_proxy: true
""",
        encoding="utf-8",
    )
    config = load_config(str(path))
    assert config.features.catch_all_proxy is True

    path.write_text(path.read_text(encoding="utf-8").replace("true", "false"), encoding="utf-8")
    assert load_config(str(path)).features.catch_all_proxy is False


def test_upstream_origin_drops_the_path():
    """The incoming path is appended verbatim, so /v1/models reaches /v1/models."""
    assert upstream_origin(_config("http://gateway:8080/v1")) == "http://gateway:8080"
    assert upstream_origin(_config("https://gateway.example")) == "https://gateway.example"


def test_get_is_relayed_with_header_body_and_status(monkeypatch):
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["method"] = request.method
        seen["url"] = str(request.url)
        seen["auth"] = request.headers.get("authorization")
        seen["x_mimo"] = request.headers.get("x-mimo-session-id")
        return _json_response(b'{"object": "list", "data": [{"id": "mimo-v2.6-flash"}]}')

    monkeypatch.setattr(proxy, "make_client", _client_factory(httpx.MockTransport(handler)))

    with TestClient(_app(_config())) as client:
        response = client.get(
            "/v1/models?limit=7",
            headers={"Authorization": "Bearer caller-key", "X-Mimo-Session-Id": "s-1"},
        )

    assert response.status_code == 200
    assert response.json()["data"][0]["id"] == "mimo-v2.6-flash"
    # verbatim: the path the client asked for, the query, and the caller's own key
    assert seen["url"] == "http://gateway:8080/v1/models?limit=7"
    assert seen["auth"] == "Bearer caller-key"
    assert seen["x_mimo"] == "s-1"


def test_binary_body_and_response_survive(monkeypatch):
    """Audio uploads and audio downloads are bytes, not JSON."""
    audio = b"RIFF\x00\x00\x00\x00WAVEfmt "

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.content == audio
        assert request.headers.get("content-type", "").startswith("multipart/form-data")
        return httpx.Response(200, stream=_Chunks(b"\xff\xfb\x90\x00"), headers={"content-type": "audio/mpeg"})

    monkeypatch.setattr(proxy, "make_client", _client_factory(httpx.MockTransport(handler)))

    with TestClient(_app(_config())) as client:
        response = client.post(
            "/v1/audio/transcriptions",
            content=audio,
            headers={"Content-Type": "multipart/form-data; boundary=x"},
        )

    assert response.status_code == 200
    assert response.content == b"\xff\xfb\x90\x00"
    assert response.headers["content-type"] == "audio/mpeg"


def test_upstream_error_status_and_body_are_relayed(monkeypatch):
    transport = httpx.MockTransport(
        lambda request: _json_response(
            '{"error": {"message": "服务器繁忙，请稍后再试"}}'.encode(), status=429
        )
    )
    monkeypatch.setattr(proxy, "make_client", _client_factory(transport))

    with TestClient(_app(_config())) as client:
        response = client.get("/v1/models")

    assert response.status_code == 429
    assert "服务器繁忙" in response.json()["error"]["message"]


def test_unreachable_upstream_is_502_not_an_empty_answer(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused", request=request)

    monkeypatch.setattr(proxy, "make_client", _client_factory(httpx.MockTransport(handler)))

    with TestClient(_app(_config())) as client:
        response = client.get("/v1/models")

    assert response.status_code == 502
    assert response.json()["error"]["type"] == "upstream_error"
