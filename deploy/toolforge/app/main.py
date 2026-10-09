"""ToolForge FastAPI application."""

from __future__ import annotations

from contextlib import asynccontextmanager
from typing import Any, Dict, Optional

from fastapi import Depends, FastAPI, Header, Request
from fastapi.responses import JSONResponse

from app import __version__
from .auth import require_client_auth
from .config import AppConfig, default_config_path, load_config
from .adapters.openai_chat import handle_chat_completions
from .adapters.openai_responses import handle_responses
from .upstream.proxy import proxy_request, upstream_health
from .upstream.router import UpstreamRouter
from .util.metrics import GLOBAL_METRICS


def create_app(config: Optional[AppConfig] = None, *, config_path: Optional[str] = None) -> FastAPI:
    if config is None:
        path = config_path or str(default_config_path())
        config = load_config(path)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.config = config
        app.state.router_upstreams = UpstreamRouter(config)
        yield
        await app.state.router_upstreams.aclose()

    app = FastAPI(
        title="ToolForge",
        version=__version__,
        description="Universal LLM tool-calling middleware",
        lifespan=lifespan,
    )
    app.state.config = config
    app.state.router_upstreams = UpstreamRouter(config)

    # Dev-only: 请求/响应 body 日志（第三轮格式泄漏测试用）。生产可移除。
    from .middleware_log import BodyLogMiddleware
    app.add_middleware(BodyLogMiddleware)

    @app.get("/healthz")
    @app.get("/health")
    async def healthz() -> Dict[str, Any]:
        body: Dict[str, Any] = {
            "status": "ok",
            "version": __version__,
            "upstreams": [u.name for u in config.upstreams],
        }
        if config.features.catch_all_proxy:
            # This port answers for the deployment, so it reports on the gateway too.
            body["upstream_health"] = await upstream_health(config)
            if body["upstream_health"].get("status") != "ok":
                body["status"] = "degraded"
        return body

    @app.get("/metrics")
    async def metrics() -> Dict[str, Any]:
        if not config.features.enable_metrics:
            return {"enabled": False}
        return GLOBAL_METRICS.snapshot()

    # With catch_all_proxy the gateway owns the catalog: /v1/models is proxied like
    # every other path it serves, so a client sees the real models, not this
    # middleware's view of its upstreams.
    if not config.features.catch_all_proxy:

        @app.get("/v1/models")
        async def list_models(request: Request, _: str = Depends(require_client_auth)) -> Dict[str, Any]:
            router = request.app.state.router_upstreams
            data = await router.list_models()
            return {"object": "list", "data": data}

    @app.post("/v1/chat/completions")
    @app.post("/chat/completions")
    async def chat_completions(
        request: Request,
        _: str = Depends(require_client_auth),
        x_toolforge_fc_mode: Optional[str] = Header(default=None, alias="X-ToolForge-FC-Mode"),
    ):
        body = await request.json()
        if not isinstance(body, dict):
            return JSONResponse(status_code=400, content={"error": {"message": "body must be object"}})
        return await handle_chat_completions(
            body,
            config=request.app.state.config,
            router=request.app.state.router_upstreams,
            fc_header=x_toolforge_fc_mode,
            inbound_headers=dict(request.headers),
        )

    @app.post("/v1/responses")
    @app.post("/responses")
    async def responses(
        request: Request,
        _: str = Depends(require_client_auth),
        x_toolforge_fc_mode: Optional[str] = Header(default=None, alias="X-ToolForge-FC-Mode"),
    ):
        body = await request.json()
        if not isinstance(body, dict):
            return JSONResponse(status_code=400, content={"error": {"message": "body must be object"}})
        return await handle_responses(
            body,
            config=request.app.state.config,
            router=request.app.state.router_upstreams,
            fc_header=x_toolforge_fc_mode,
            inbound_headers=dict(request.headers),
        )

    if config.features.catch_all_proxy:

        @app.api_route(
            "/{path:path}",
            methods=["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"],
        )
        async def catch_all(request: Request, _: str = Depends(require_client_auth)):
            return await proxy_request(request, request.app.state.config)

    return app


def _load_default_app() -> FastAPI:
    try:
        return create_app()
    except FileNotFoundError:
        from .config import AppConfig, ClientAuthConfig, UpstreamConfig

        return create_app(
            AppConfig(
                client_authentication=ClientAuthConfig(enabled=False),
                upstreams=[
                    UpstreamConfig(
                        name="placeholder",
                        base_url="http://127.0.0.1:9/v1",
                        models=["*"],
                        is_default=True,
                    )
                ],
            )
        )


app = _load_default_app()
