"""Request/response body logging middleware for debugging toolcall output.

Writes full request/response bodies to /app/reqlog/ (container fs, retrieved
via `docker cp`) AND prints a compact pointer to stdout. Dev-only, for the
R41+ format-leak testing loop.
"""
from __future__ import annotations

import json
import os
import time
import uuid
from contextvars import ContextVar

LOG_DIR = "/app/reqlog"

#: Request ID propagated to inner handlers (e.g. stream_prompt_fc) so they
#: can log intermediate data correlated with the client-side req/resp files.
REQUEST_ID: ContextVar[str] = ContextVar("toolforge_request_id", default="")


def _ensure_dir() -> None:
    try:
        os.makedirs(LOG_DIR, exist_ok=True)
    except Exception:
        pass


def _write(kind: str, rid: str, payload: str) -> None:
    _ensure_dir()
    try:
        with open(os.path.join(LOG_DIR, f"{kind}_{rid}.json"), "w", encoding="utf-8") as f:
            f.write(payload)
    except Exception:
        pass


class BodyLogMiddleware:
    """ASGI middleware that snapshots request + response bodies for logging."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            return await self.app(scope, receive, send)

        path = scope.get("path", "")
        if path not in ("/v1/chat/completions", "/chat/completions",
                        "/v1/responses", "/responses", "/v1/messages", "/messages"):
            return await self.app(scope, receive, send)

        rid = uuid.uuid4().hex[:8]
        ts = time.time()
        REQUEST_ID.set(rid)

        body_chunks = []
        async def _receive():
            msg = await receive()
            if msg["type"] == "http.request":
                body_chunks.append(msg.get("body", b""))
            return msg

        resp_chunks = []
        async def _send(msg):
            if msg["type"] == "http.response.body":
                resp_chunks.append(msg.get("body", b""))
            return await send(msg)

        try:
            await self.app(scope, _receive, _send)
        finally:
            try:
                req_text = b"".join(body_chunks).decode("utf-8", errors="replace")
                resp_text = b"".join(resp_chunks).decode("utf-8", errors="replace")
            except Exception:
                req_text, resp_text = "", ""
            _write("req", rid, req_text)
            _write("resp", rid, resp_text)
            print(json.dumps({"__tf_log": "x", "rid": rid, "reqb": len(req_text), "respb": len(resp_text)}), flush=True)