"""Catch-all passthrough to the gateway behind us (ADR 0012).

With `features.catch_all_proxy` the middleware is the deployment's only published
port: it answers `/v1/chat/completions` and `/v1/responses` itself (that is what it
is for) and hands every other path — `/v1/models`, `/v1/audio/*`, `/v1/admin/*` —
to the upstream gateway verbatim: method, path, query, body and headers, including
the caller's own `Authorization`. One address for clients, one key, and the paths
the middleware does not implement behave exactly as if the gateway were called
directly.

Responses are streamed through unbuffered and unmodified (raw bytes, original
status and headers), because the paths this carries include SSE streams from the
gateway's own chat, binary audio from `/v1/audio/speech`, and multipart uploads to
`/v1/audio/transcriptions`.
"""

from __future__ import annotations

from typing import Dict
from urllib.parse import urlsplit

import httpx
from fastapi import Request
from fastapi.responses import JSONResponse, Response, StreamingResponse

from ..config import AppConfig, UpstreamConfig

# Per RFC 7230: these belong to a single hop and must not be forwarded.
HOP_BY_HOP = {
    "host",
    "content-length",
    "connection",
    "keep-alive",
    "transfer-encoding",
    "upgrade",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
}

# No read timeout: this carries /v1/audio/speech, whose synthesis budget is minutes.
PROXY_TIMEOUT = httpx.Timeout(None, connect=10.0, write=60.0)


def make_client(*, timeout: httpx.Timeout = PROXY_TIMEOUT) -> httpx.AsyncClient:
    """The one place an HTTP client is built here, so tests can replace it."""
    return httpx.AsyncClient(timeout=timeout, follow_redirects=False)


def default_upstream(config: AppConfig) -> UpstreamConfig:
    for upstream in config.upstreams:
        if upstream.is_default:
            return upstream
    if not config.upstreams:
        raise RuntimeError("catch_all_proxy is on but no upstream is configured")
    return config.upstreams[0]


def upstream_origin(config: AppConfig) -> str:
    """The scheme://host:port of the default upstream, without its path.

    The incoming path is appended verbatim, so `/v1/models` reaches the gateway's
    `/v1/models` regardless of whether the configured base_url carries `/v1`.
    """
    parts = urlsplit(default_upstream(config).base_url)
    return f"{parts.scheme}://{parts.netloc}"


def forwarded_headers(request: Request) -> Dict[str, str]:
    return {k: v for k, v in request.headers.items() if k.lower() not in HOP_BY_HOP}


async def proxy_request(request: Request, config: AppConfig) -> Response:
    url = upstream_origin(config) + request.url.path
    if request.url.query:
        url = f"{url}?{request.url.query}"

    body = await request.body()
    client = make_client()
    upstream_request = client.build_request(
        request.method, url, headers=forwarded_headers(request), content=body or None
    )
    try:
        response = await client.send(upstream_request, stream=True)
    except httpx.HTTPError as exc:
        await client.aclose()
        return JSONResponse(
            status_code=502,
            content={
                "error": {
                    "message": f"upstream unreachable: {exc.__class__.__name__}",
                    "type": "upstream_error",
                }
            },
        )

    headers = {k: v for k, v in response.headers.items() if k.lower() not in HOP_BY_HOP}

    async def relay():
        try:
            async for chunk in response.aiter_raw():
                yield chunk
        finally:
            await response.aclose()
            await client.aclose()

    return StreamingResponse(relay(), status_code=response.status_code, headers=headers)


async def upstream_health(config: AppConfig) -> Dict[str, object]:
    """Probe the gateway's /healthz, so this port can answer for the deployment."""
    url = upstream_origin(config) + "/healthz"
    try:
        async with make_client(timeout=httpx.Timeout(5.0)) as client:
            response = await client.get(url)
        return {"status": "ok" if response.status_code == 200 else "degraded", "code": response.status_code}
    except httpx.HTTPError as exc:
        return {"status": "unreachable", "error": exc.__class__.__name__}
