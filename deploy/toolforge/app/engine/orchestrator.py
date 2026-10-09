"""Cross-protocol orchestration: resolve upstream, wire, execute, recover."""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Union

import httpx
from fastapi import HTTPException
from fastapi.responses import JSONResponse, StreamingResponse

from ..config import AppConfig, UpstreamConfig
from ..fc.obfuscation import deobfuscate_calls, obfuscate_tools
from ..fc.policy import apply_policy
from ..fc.profiles import build_few_shot_block, detect_tool_profile, profile_instruction_block
from ..fc.recovery import build_retry_user_message, parse_with_recovery_hint
from ..models.canonical import CanonicalRequest, CanonicalResponse, Message, tool_defs_to_openai
from ..stream.openai_sse import stream_native_passthrough, stream_prompt_fc
from ..stream.responses_sse import stream_responses_from_openai_chat
from ..upstream.router import UpstreamRouter
from ..util.metrics import GLOBAL_METRICS
from ..util.tokens import messages_text
from .wire import (
    build_openai_wire,
    encode_client_response,
    openai_result_to_canonical,
)
from .xyml import build_legacy_name_map


def _extra_instructions(req: CanonicalRequest, config: AppConfig) -> str:
    chunks: List[str] = []
    if config.features.enable_cli_profiles and req.tools:
        profile = detect_tool_profile(req.tools)
        chunks.append(profile_instruction_block(profile))
        few = build_few_shot_block(req.tools)
        if few:
            chunks.append(few)
    return "\n\n".join(chunks)


def _prepare_tools_for_prompt(req: CanonicalRequest, config: AppConfig) -> Dict[str, str]:
    mapping: Dict[str, str] = {}
    if config.features.obfuscate_tool_names and req.tools:
        tools, mapping = obfuscate_tools(req.tools)
        req.tools = tools
    return mapping


def forwarded_headers(req: CanonicalRequest, config: AppConfig) -> Dict[str, str]:
    """Headers to carry from the client request to the upstream call.

    The gateway behind us scopes its session state per credential, so a middleware
    that always authenticated with its own key would collapse every caller into one
    namespace. `key_passthrough` forwards the caller's Authorization (already
    verified by our own client auth); `passthrough_headers` forwards named ones,
    a trailing "*" matching a prefix ("X-Mimo-*").
    """
    inbound = {str(k).lower(): str(v) for k, v in (req.inbound_headers or {}).items()}
    if not inbound:
        return {}
    wanted = [str(h) for h in (config.features.passthrough_headers or [])]
    if config.features.key_passthrough:
        wanted.append("authorization")
    out: Dict[str, str] = {}
    for name in wanted:
        name = name.strip().lower()
        if not name:
            continue
        if name.endswith("*"):
            prefix = name[:-1]
            for key, value in inbound.items():
                if key.startswith(prefix) and value:
                    out["-".join(part.capitalize() for part in key.split("-"))] = value
        elif name in inbound and inbound[name]:
            out["-".join(part.capitalize() for part in name.split("-"))] = inbound[name]
    return out


def _stream_headers(fc_mode: str) -> Dict[str, str]:
    return {
        "Cache-Control": "no-cache",
        "Connection": "keep-alive",
        "X-Accel-Buffering": "no",
        "X-ToolForge-FC-Mode": fc_mode,
    }


async def _execute_once(
    req: CanonicalRequest,
    *,
    config: AppConfig,
    router: UpstreamRouter,
    upstream_cfg: UpstreamConfig,
    upstream_model: str,
) -> Union[CanonicalResponse, StreamingResponse]:
    instructions_extra = _extra_instructions(req, config)
    # 响应侧反向规范化表: 以本次请求真实注册的工具名集合为唯一事实源
    legacy_map = build_legacy_name_map([t.name for t in req.tools])

    client = router.get_client(upstream_cfg)

    # openai_compat default
    wire = build_openai_wire(
        req,
        upstream_model=upstream_model,
        protocol=config.features.inject_protocol,
        convert_developer=config.features.convert_developer_to_system,
        instructions_extra=instructions_extra,
        sanitize=config.features.sanitize_claude_code,
    )
    if config.features.model_passthrough:
        wire["model"] = req.model

    forward = forwarded_headers(req, config)

    if req.stream:
        line_iter = await client.chat_completions(
            wire, stream=True, forward_headers=forward
        )

        if req.surface == "responses":
            generator = stream_responses_from_openai_chat(
                line_iter,  # type: ignore[arg-type]
                model=req.model,
                tools=req.tools,
                protocol=config.features.inject_protocol,
                strip_think=config.features.strip_think_tags,
                prompt_fc=(req.fc_mode == "prompt"),
                legacy_map=legacy_map,
            )
            return StreamingResponse(
                generator,
                media_type="text/event-stream",
                headers=_stream_headers(req.fc_mode),
            )

        # surface openai
        prompt_text = messages_text(req)
        if req.fc_mode == "prompt":
            generator = stream_prompt_fc(
                line_iter,  # type: ignore[arg-type]
                model=req.model,
                tools=req.tools,
                protocol=config.features.inject_protocol,
                strip_think=config.features.strip_think_tags,
                prompt_text=prompt_text,
                legacy_map=legacy_map,
            )
        else:
            generator = stream_native_passthrough(
                line_iter,  # type: ignore[arg-type]
                model=req.model,
                prompt_text=prompt_text,
                legacy_map=legacy_map,
                tools=tool_defs_to_openai(req.tools),
            )
        return StreamingResponse(
            generator,
            media_type="text/event-stream",
            headers=_stream_headers(req.fc_mode),
        )

    result = await client.chat_completions(
        wire, stream=False, forward_headers=forward
    )
    assert isinstance(result, dict)
    return openai_result_to_canonical(
        result,
        req=req,
        protocol=config.features.inject_protocol,
        strip_think=config.features.strip_think_tags,
        legacy_map=legacy_map,
    )


async def handle_canonical(
    req: CanonicalRequest,
    *,
    config: AppConfig,
    router: UpstreamRouter,
    fc_header: Optional[str] = None,
) -> Any:
    try:
        upstream_cfg, upstream_model = router.resolve(req.model)
    except KeyError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc

    req.upstream_model = upstream_model
    apply_policy(req, upstream_cfg, config, header_override=fc_header)

    if req.stream and not config.features.enable_streaming:
        raise HTTPException(status_code=400, detail="streaming disabled by server config")

    name_map: Dict[str, str] = {}
    if req.fc_mode == "prompt":
        name_map = _prepare_tools_for_prompt(req, config)

    # 响应侧反向规范化表(与 _execute_once 内构建一致, 供最终 encode 使用)
    legacy_map = build_legacy_name_map([t.name for t in req.tools])

    try:
        result = await _execute_once(
            req,
            config=config,
            router=router,
            upstream_cfg=upstream_cfg,
            upstream_model=upstream_model,
        )
        if isinstance(result, StreamingResponse):
            GLOBAL_METRICS.record_request(surface=req.surface, fc_mode=req.fc_mode, ok=True)
            return result

        if (
            req.fc_mode == "prompt"
            and config.features.enable_fc_error_retry
            and not result.tool_calls
            and result.content
        ):
            calls, reason = parse_with_recovery_hint(
                result.content,
                req.tools,
                protocol=config.features.inject_protocol,
                strip_think=config.features.strip_think_tags,
            )
            if reason:
                attempts = max(1, int(config.features.fc_error_retry_max))
                for _ in range(attempts):
                    GLOBAL_METRICS.record_retry()
                    retry_msg = build_retry_user_message(
                        original_output=result.content, reason=reason
                    )
                    retry_req = CanonicalRequest(
                        model=req.model,
                        messages=list(req.messages)
                        + [
                            Message(role="assistant", content=result.content),
                            Message(role="user", content=retry_msg),
                        ],
                        tools=req.tools,
                        tool_choice=req.tool_choice,
                        stream=False,
                        surface=req.surface,
                        temperature=req.temperature,
                        max_tokens=req.max_tokens,
                        top_p=req.top_p,
                        extra=req.extra,
                        fc_mode="prompt",
                        upstream_model=req.upstream_model,
                    )
                    retry_result = await _execute_once(
                        retry_req,
                        config=config,
                        router=router,
                        upstream_cfg=upstream_cfg,
                        upstream_model=upstream_model,
                    )
                    if isinstance(retry_result, StreamingResponse):
                        break
                    result = retry_result
                    if result.tool_calls:
                        break
                    calls, reason = parse_with_recovery_hint(
                        result.content,
                        req.tools,
                        protocol=config.features.inject_protocol,
                        strip_think=config.features.strip_think_tags,
                    )
                    if not reason:
                        break

        if name_map and result.tool_calls:
            result.tool_calls = deobfuscate_calls(result.tool_calls, name_map)

        payload = encode_client_response(
            req, result, prompt_text=messages_text(req), legacy_map=legacy_map
        )
        GLOBAL_METRICS.record_request(surface=req.surface, fc_mode=req.fc_mode, ok=True)
        return JSONResponse(
            content=payload,
            headers={"X-ToolForge-FC-Mode": req.fc_mode},
        )
    except httpx.HTTPStatusError as exc:
        GLOBAL_METRICS.record_request(surface=req.surface, fc_mode=req.fc_mode, ok=False)
        status = exc.response.status_code if exc.response is not None else 502
        raise HTTPException(status_code=status, detail=str(exc)) from exc
    except httpx.HTTPError as exc:
        GLOBAL_METRICS.record_request(surface=req.surface, fc_mode=req.fc_mode, ok=False)
        raise HTTPException(status_code=502, detail=f"upstream error: {exc}") from exc
    except Exception:
        GLOBAL_METRICS.record_request(
            surface=req.surface, fc_mode=getattr(req, "fc_mode", "auto"), ok=False
        )
        raise
