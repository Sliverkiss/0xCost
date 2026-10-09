"""Decide native FC pass-through vs prompt-based FC."""

from __future__ import annotations

from typing import Optional

from ..config import AppConfig, FeaturesConfig, UpstreamConfig
from ..models.canonical import CanonicalRequest, ResolvedFCMode


def resolve_fc_mode(
    req: CanonicalRequest,
    upstream: UpstreamConfig,
    features: FeaturesConfig,
    *,
    header_override: Optional[str] = None,
) -> ResolvedFCMode:
    """Return the concrete path for this request.

    - passthrough: no tools on request → pure proxy
    - native: keep tools fields, trust upstream FC
    - prompt: strip tools, inject XYML instructions, parse response
    """
    if not req.tools:
        return "passthrough"

    override = (header_override or req.request_fc_override or "").strip().lower()
    global_mode = (features.fc_mode or "auto").strip().lower()

    if override in {"force_prompt", "prompt"}:
        return "prompt"
    if override in {"prefer_native", "native", "force_native"}:
        return "native" if upstream.native_fc else "prompt"

    if global_mode == "force_prompt":
        return "prompt"
    if global_mode in {"prefer_native", "auto"}:
        return "native" if upstream.native_fc else "prompt"

    # Unknown mode → auto behavior
    return "native" if upstream.native_fc else "prompt"


def apply_policy(
    req: CanonicalRequest,
    upstream: UpstreamConfig,
    config: AppConfig,
    *,
    header_override: Optional[str] = None,
) -> CanonicalRequest:
    req.fc_mode = resolve_fc_mode(req, upstream, config.features, header_override=header_override)
    return req
