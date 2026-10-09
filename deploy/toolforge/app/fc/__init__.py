from __future__ import annotations

from functools import lru_cache

from app.engine.xyml import ToolCallConfig


@lru_cache(maxsize=8)
def _config(protocol: str = "XYML", missing_required: str = "defaulted") -> ToolCallConfig:
    return ToolCallConfig.for_prompt(protocol, missing_required=missing_required)