"""YAML config loading with ${ENV} expansion."""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Optional

import yaml

_ENV_PATTERN = re.compile(r"\$\{([^}]+)\}")


def _expand_env(value: str) -> str:
    """Expand ${VAR} or ${VAR:-default} placeholders."""

    def repl(match: re.Match[str]) -> str:
        body = match.group(1)
        if ":-" in body:
            name, default = body.split(":-", 1)
            return os.environ.get(name.strip(), default)
        return os.environ.get(body.strip(), "")

    return _ENV_PATTERN.sub(repl, value)


def _expand_tree(node: Any) -> Any:
    if isinstance(node, str):
        return _expand_env(node)
    if isinstance(node, list):
        return [_expand_tree(item) for item in node]
    if isinstance(node, dict):
        return {key: _expand_tree(value) for key, value in node.items()}
    return node


@dataclass
class ServerConfig:
    host: str = "0.0.0.0"
    port: int = 8080
    timeout_seconds: int = 180


@dataclass
class ClientAuthConfig:
    enabled: bool = True
    allowed_keys: List[str] = field(default_factory=list)


@dataclass
class UpstreamConfig:
    name: str
    type: str = "openai_compat"  # openai_compat | anthropic | gemini
    base_url: str = ""
    api_key: str = ""
    models: List[str] = field(default_factory=lambda: ["*"])
    native_fc: bool = True
    is_default: bool = False
    headers: Dict[str, str] = field(default_factory=dict)


@dataclass
class FeaturesConfig:
    fc_mode: str = "auto"  # auto | prefer_native | force_prompt
    enable_streaming: bool = True
    enable_fc_error_retry: bool = True
    fc_error_retry_max: int = 2
    strip_think_tags: bool = True
    inject_protocol: str = "XYML"
    missing_required: str = "defaulted"  # drop | defaulted | error — 缺必填参数时如何处理协议块
    convert_developer_to_system: bool = True
    key_passthrough: bool = False
    # Header names forwarded from the client request to the upstream call.
    # Exact names, or a trailing "*" for a prefix match ("X-Mimo-*").
    # key_passthrough=true is shorthand for adding "authorization".
    passthrough_headers: List[str] = field(default_factory=list)
    model_passthrough: bool = False
    sanitize_claude_code: bool = True
    # Phase 3
    enable_cli_profiles: bool = True
    obfuscate_tool_names: bool = False
    enable_metrics: bool = True


@dataclass
class RoutingConfig:
    aliases: Dict[str, Dict[str, str]] = field(default_factory=dict)


@dataclass
class AppConfig:
    server: ServerConfig = field(default_factory=ServerConfig)
    client_authentication: ClientAuthConfig = field(default_factory=ClientAuthConfig)
    upstreams: List[UpstreamConfig] = field(default_factory=list)
    routing: RoutingConfig = field(default_factory=RoutingConfig)
    features: FeaturesConfig = field(default_factory=FeaturesConfig)

    def default_upstream(self) -> Optional[UpstreamConfig]:
        for item in self.upstreams:
            if item.is_default:
                return item
        return self.upstreams[0] if self.upstreams else None

    def find_upstream(self, name: str) -> Optional[UpstreamConfig]:
        for item in self.upstreams:
            if item.name == name:
                return item
        return None

    def resolve_model(self, model: str) -> tuple[UpstreamConfig, str]:
        """Map client model name to (upstream, upstream_model)."""
        alias = self.routing.aliases.get(model)
        if alias:
            upstream_name = alias.get("upstream") or ""
            upstream_model = alias.get("model") or model
            upstream = self.find_upstream(upstream_name)
            if upstream is None:
                raise KeyError(f"routing alias '{model}' points to unknown upstream '{upstream_name}'")
            return upstream, upstream_model

        for item in self.upstreams:
            if model in item.models:
                return item, model

        for item in self.upstreams:
            if "*" in item.models:
                return item, model

        default = self.default_upstream()
        if default is None:
            raise KeyError(f"no upstream configured for model '{model}'")
        return default, model


def _as_dict(value: Any) -> Dict[str, Any]:
    return value if isinstance(value, dict) else {}


def load_config(path: str | Path) -> AppConfig:
    config_path = Path(path)
    if not config_path.is_file():
        raise FileNotFoundError(f"config not found: {config_path}")

    raw = yaml.safe_load(config_path.read_text(encoding="utf-8")) or {}
    data = _expand_tree(raw)
    if not isinstance(data, dict):
        raise ValueError("config root must be a mapping")

    server_raw = _as_dict(data.get("server"))
    auth_raw = _as_dict(data.get("client_authentication"))
    features_raw = _as_dict(data.get("features"))
    routing_raw = _as_dict(data.get("routing"))

    upstreams: List[UpstreamConfig] = []
    for item in data.get("upstreams") or []:
        if not isinstance(item, dict):
            continue
        upstreams.append(
            UpstreamConfig(
                name=str(item.get("name") or "").strip() or "unnamed",
                type=str(item.get("type") or "openai_compat").strip(),
                base_url=str(item.get("base_url") or "").rstrip("/"),
                api_key=str(item.get("api_key") or ""),
                models=[str(m) for m in (item.get("models") or ["*"])],
                native_fc=bool(item.get("native_fc", True)),
                is_default=bool(item.get("is_default", False)),
                headers={str(k): str(v) for k, v in _as_dict(item.get("headers")).items()},
            )
        )

    aliases: Dict[str, Dict[str, str]] = {}
    for key, value in _as_dict(routing_raw.get("aliases")).items():
        if isinstance(value, dict):
            aliases[str(key)] = {str(k): str(v) for k, v in value.items()}

    return AppConfig(
        server=ServerConfig(
            host=str(server_raw.get("host") or "0.0.0.0"),
            port=int(server_raw.get("port") or 8080),
            timeout_seconds=int(server_raw.get("timeout_seconds") or 180),
        ),
        client_authentication=ClientAuthConfig(
            enabled=bool(auth_raw.get("enabled", True)),
            allowed_keys=[str(k) for k in (auth_raw.get("allowed_keys") or [])],
        ),
        upstreams=upstreams,
        routing=RoutingConfig(aliases=aliases),
        features=FeaturesConfig(
            fc_mode=str(features_raw.get("fc_mode") or "auto").strip(),
            enable_streaming=bool(features_raw.get("enable_streaming", True)),
            enable_fc_error_retry=bool(features_raw.get("enable_fc_error_retry", True)),
            fc_error_retry_max=int(features_raw.get("fc_error_retry_max") or 2),
            strip_think_tags=bool(features_raw.get("strip_think_tags", True)),
            inject_protocol=str(features_raw.get("inject_protocol") or "XYML").strip(),
            missing_required=str(features_raw.get("missing_required") or "defaulted").strip(),
            convert_developer_to_system=bool(features_raw.get("convert_developer_to_system", True)),
            key_passthrough=bool(features_raw.get("key_passthrough", False)),
            passthrough_headers=[
                str(h) for h in (features_raw.get("passthrough_headers") or [])
            ],
            model_passthrough=bool(features_raw.get("model_passthrough", False)),
            sanitize_claude_code=bool(features_raw.get("sanitize_claude_code", True)),
            enable_cli_profiles=bool(features_raw.get("enable_cli_profiles", True)),
            obfuscate_tool_names=bool(features_raw.get("obfuscate_tool_names", False)),
            enable_metrics=bool(features_raw.get("enable_metrics", True)),
        ),
    )


def default_config_path() -> Path:
    env = os.environ.get("TOOLFORGE_CONFIG")
    if env:
        return Path(env)
    cwd = Path.cwd() / "config.yaml"
    if cwd.is_file():
        return cwd
    return Path(__file__).resolve().parents[1] / "config.example.yaml"
