"""Client API key authentication."""

from __future__ import annotations

from typing import Optional

from fastapi import Header, HTTPException, Request

from .config import AppConfig


def extract_bearer(authorization: Optional[str]) -> Optional[str]:
    if not authorization:
        return None
    parts = authorization.strip().split(None, 1)
    if len(parts) == 2 and parts[0].lower() == "bearer":
        return parts[1].strip()
    return authorization.strip() or None


def resolve_client_key(
    authorization: Optional[str] = None,
    x_api_key: Optional[str] = None,
    x_goog_api_key: Optional[str] = None,
) -> Optional[str]:
    return extract_bearer(authorization) or (x_api_key or "").strip() or (x_goog_api_key or "").strip() or None


def verify_client_key(config: AppConfig, key: Optional[str]) -> None:
    auth = config.client_authentication
    if not auth.enabled:
        return
    if not auth.allowed_keys:
        # Auth enabled but no keys configured → deny all.
        raise HTTPException(status_code=401, detail="client authentication enabled but no keys configured")
    if not key or key not in auth.allowed_keys:
        raise HTTPException(status_code=401, detail="invalid api key")


async def require_client_auth(
    request: Request,
    authorization: Optional[str] = Header(default=None),
    x_api_key: Optional[str] = Header(default=None, alias="x-api-key"),
    x_goog_api_key: Optional[str] = Header(default=None, alias="x-goog-api-key"),
) -> str:
    config: AppConfig = request.app.state.config
    key = resolve_client_key(authorization, x_api_key, x_goog_api_key)
    verify_client_key(config, key)
    return key or ""
