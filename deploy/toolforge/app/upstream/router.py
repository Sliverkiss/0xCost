"""Model → upstream resolution and client factory."""

from __future__ import annotations

from typing import Dict

from ..config import AppConfig, UpstreamConfig
from .openai import OpenAICompatUpstream

UpstreamClientType = OpenAICompatUpstream


class UpstreamRouter:
    def __init__(self, config: AppConfig) -> None:
        self.config = config
        self._clients: Dict[str, UpstreamClientType] = {}

    def resolve(self, model: str) -> tuple[UpstreamConfig, str]:
        return self.config.resolve_model(model)

    def get_client(self, upstream: UpstreamConfig) -> UpstreamClientType:
        client = self._clients.get(upstream.name)
        if client is not None:
            return client

        timeout = float(self.config.server.timeout_seconds)
        client = OpenAICompatUpstream(upstream, timeout=timeout)

        self._clients[upstream.name] = client
        return client

    async def aclose(self) -> None:
        for client in self._clients.values():
            await client.aclose()
        self._clients.clear()

    async def list_models(self) -> list[dict]:
        """聚合所有上游的真实模型列表（透传上游 /models），配置模型作为 fallback。"""
        data: list[dict] = []
        seen: set[str] = set()

        # 1) 优先从各上游透传真实模型（如 cnb2api 的 deepseek-v4-*）
        for upstream in self.config.upstreams:
            client = self.get_client(upstream)
            list_method = getattr(client, "list_models", None)
            if list_method is None:
                continue
            try:
                result = await list_method()
                for m in (result or {}).get("data", []) or []:
                    mid = m.get("id") if isinstance(m, dict) else None
                    if not mid or mid in seen:
                        continue
                    seen.add(mid)
                    data.append({
                        "id": mid,
                        "object": "model",
                        "owned_by": m.get("owned_by") or upstream.name,
                        "permission": [],
                    })
            except Exception:
                # 上游透传失败 → 落到配置模型 fallback
                pass

        # 2) 配置里显式列出的模型（非 "*"）作为 fallback / 补充
        for upstream in self.config.upstreams:
            for model in upstream.models:
                if model == "*" or model in seen:
                    continue
                seen.add(model)
                data.append({
                    "id": model,
                    "object": "model",
                    "owned_by": upstream.name,
                    "permission": [],
                })

        # 3) routing aliases
        for alias in (self.config.routing.aliases or {}):
            if alias not in seen:
                data.append({
                    "id": alias,
                    "object": "model",
                    "owned_by": "routing",
                    "permission": [],
                })

        return data
