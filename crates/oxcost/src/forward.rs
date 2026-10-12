//! Request forwarding: client request → owning plugin → upstream call →
//! response relay. Contract §5 external surface behavior.
//!
//! M3 seams (deliberate, no rework needed later):
//! - **Load balancing**: `pick_target` currently returns the first servable
//!   instance; M3 replaces the body with strategy selection
//!   (round-robin/fill-first/failover) — same signature.
//! - **Circuit breaker**: upstream failures surface through `HostError::Upstream`
//!   today; M3 hooks consecutive-failure counting there, feeding the state
//!   machine's `trip_breaker`/`cooldown_elapsed` transitions.
//! - **Sticky preferred plugin**: `pick_target` takes the whole state, so a
//!   request-scoped preference can slot into the same signature.

use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use futures_util::StreamExt;
use serde_json::{json, Value};

use crate::state::AppState;
use oxcost_core::{KeySource, PluginInstance, RouteDecision};

/// Parse `stream` from a JSON body without full deserialization: the host
/// relays bodies byte-faithfully; it only needs this one field.
pub fn wants_stream(body: &Value) -> bool {
    body.get("stream").and_then(Value::as_bool).unwrap_or(false)
}

/// OpenAI-shaped error body (contract §4/§5).
pub fn openai_error(message: &str, err_type: &str, code: &str) -> Value {
    json!({
        "error": {
            "message": message,
            "type": err_type,
            "param": null,
            "code": code
        }
    })
}

/// Host-generated error responses (contract §4/§5 mapping).
pub enum HostError {
    /// Body failed validation → 400 invalid_request.
    InvalidRequest(String),
    /// No plugin serves the model → 404 model_not_found.
    ModelNotFound(String),
    /// Upstream connection failure → 502 upstream_error.
    Upstream(String),
    /// Client auth failed → 401 invalid_api_key.
    Unauthorized,
}

impl IntoResponse for HostError {
    fn into_response(self) -> Response {
        // §4 (revised M3): type strings match OpenAI's actual spellings —
        // 4xx request/auth/model classes are `invalid_request_error`.
        let (status, body) = match self {
            HostError::InvalidRequest(msg) => (
                StatusCode::BAD_REQUEST,
                openai_error(&msg, "invalid_request_error", "invalid_request"),
            ),
            HostError::ModelNotFound(model) => (
                StatusCode::NOT_FOUND,
                openai_error(
                    &format!("model `{model}` not found on any registered plugin"),
                    "invalid_request_error",
                    "model_not_found",
                ),
            ),
            HostError::Upstream(msg) => (
                StatusCode::BAD_GATEWAY,
                openai_error(&msg, "upstream_error", "upstream_error"),
            ),
            HostError::Unauthorized => (
                StatusCode::UNAUTHORIZED,
                openai_error(
                    "missing or invalid API key",
                    "invalid_request_error",
                    "invalid_api_key",
                ),
            ),
        };
        (status, axum::Json(body)).into_response()
    }
}

/// Resolve the client bearer token from headers.
pub fn bearer_token(headers: &HeaderMap) -> Option<String> {
    let value = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())?;
    let token = value
        .strip_prefix("Bearer ")
        .or_else(|| value.strip_prefix("bearer "))?;
    let token = token.trim();
    (!token.is_empty()).then(|| token.to_string())
}

/// Pick the plugin that should serve this model. M3 LB seam (see module
/// docs): first servable owner wins today.
pub fn pick_target(state: &AppState, model: &str) -> Option<Arc<PluginInstance>> {
    state.find_owner(model)
}

/// Apply the model mapping to the outbound body: replace `model` with the
/// upstream id. Everything else is relayed verbatim.
pub fn rewrite_model(body: &Value, upstream_model: &str) -> String {
    let mut out = body.clone();
    out["model"] = Value::String(upstream_model.to_string());
    out.to_string()
}

/// Resolve the plugin key for the Authorization header from the instance's
/// key source. Key material is read here and never logged.
fn plugin_key(instance: &PluginInstance) -> Option<String> {
    match instance.key_source()? {
        KeySource::Env(var) => std::env::var(var).ok(),
        // host-spawn keyring arrives with host-spawn support (M-later).
        KeySource::HostGenerated | KeySource::None => None,
    }
}

/// Dispatch one chat request through an SDK plugin (contract §10): strip
/// the `name/` prefix (already resolved by the caller), call the trait,
/// map the outcome onto a Response with the same byte-faithful semantics
/// as the sidecar path.
async fn dispatch_sdk(
    plugin: std::sync::Arc<dyn oxcost_sdk::OxcostPlugin>,
    upstream_model: &str,
    body: &Value,
) -> Result<Response, HostError> {
    // The request the plugin sees carries the unprefixed model id — same
    // view an external sidecar gets (§5 model routing).
    let mut outbound = body.clone();
    outbound["model"] = Value::String(upstream_model.to_string());
    let req = oxcost_sdk::ChatRequest::new(outbound).map_err(|e| {
        // A body that passed host validation should never fail plugin-side
        // validation; surface as 400 with the plugin's parameter detail.
        HostError::InvalidRequest(e.message)
    })?;

    let ctx = oxcost_sdk::RequestContext::default();
    match plugin.chat(req, ctx).await {
        Ok(oxcost_sdk::ChatOutcome::Json(value)) => {
            let bytes = value.to_string();
            Response::builder()
                .status(StatusCode::OK)
                .header(header::CONTENT_TYPE, "application/json")
                .body(Body::from(bytes))
                .map_err(|e| HostError::Upstream(format!("relay failed: {e}")))
        }
        Ok(oxcost_sdk::ChatOutcome::Sse(bytes)) => Response::builder()
            .status(StatusCode::OK)
            .header(header::CONTENT_TYPE, "text/event-stream")
            .body(Body::from(bytes))
            .map_err(|e| HostError::Upstream(format!("stream relay failed: {e}"))),
        // §4 error mapping: the plugin's typed error becomes the OpenAI
        // error body verbatim (same strings a sidecar would emit).
        Err(e) => {
            let status =
                StatusCode::from_u16(e.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
            let payload = openai_error(&e.message, &e.err_type, e.code.as_deref().unwrap_or(""));
            let payload = set_error_param(payload, e.param.as_deref());
            (status, axum::Json(payload)).into_response().pipe_ok()
        }
    }
}

/// Attach the `param` field to an error body when present.
fn set_error_param(mut body: Value, param: Option<&str>) -> Value {
    if let Some(p) = param {
        body["error"]["param"] = Value::String(p.to_string());
    }
    body
}

/// Helper trait: keep `Result<Response, HostError>` ergonomics in
/// `dispatch_sdk` without nesting matches.
trait IntoResult {
    fn pipe_ok(self) -> Result<Response, HostError>;
}

impl IntoResult for Response {
    fn pipe_ok(self) -> Result<Response, HostError> {
        Ok(self)
    }
}

/// Forward one chat completion request.
///
/// - Resolves model → owning plugin (404 when none).
/// - Rewrites `model` to the upstream id, relays everything else verbatim.
/// - Sidecar form: HTTP with `Authorization: Bearer <plugin key>`.
/// - SDK form: direct in-process trait call (contract §10).
/// - `stream: false` → JSON pass-through; `stream: true` → SSE bytes
///   relayed as they arrive (byte-faithful).
pub async fn forward_chat(
    state: Arc<AppState>,
    headers: HeaderMap,
    body: Value,
) -> Result<Response, HostError> {
    // Host auth: enforced only when keys are configured (dev mode allows all).
    if !state.config.api_keys.is_empty() {
        match bearer_token(&headers) {
            Some(token) if state.config.accepts_key(&token) => {}
            _ => return Err(HostError::Unauthorized),
        }
    }

    // Validate the body shape: object with model + non-empty messages.
    let model = body
        .get("model")
        .and_then(Value::as_str)
        .map(str::to_string)
        .ok_or_else(|| HostError::InvalidRequest("missing `model` field".into()))?;
    let messages_ok = body
        .get("messages")
        .map(|m| m.as_array().is_some_and(|a| !a.is_empty()))
        .unwrap_or(false);
    if !messages_ok {
        return Err(HostError::InvalidRequest(
            "`messages` must be a non-empty array".into(),
        ));
    }

    // Routing.
    let instance =
        pick_target(&state, &model).ok_or_else(|| HostError::ModelNotFound(model.clone()))?;
    let RouteDecision::Forward { upstream_model } = instance.route(&model) else {
        // pick_target only returns Forward-capable instances.
        return Err(HostError::ModelNotFound(model));
    };

    // Transport dispatch: the single seam where plugin forms differ.
    match instance.handle() {
        oxcost_core::PluginHandle::Sdk(plugin) => {
            dispatch_sdk(Arc::clone(plugin), &upstream_model, &body).await
        }
        oxcost_core::PluginHandle::Sidecar { .. } => {
            forward_sidecar(
                &instance,
                &upstream_model,
                &body,
                is_stream_flag(&body),
                state.config.request_timeout_secs,
            )
            .await
        }
    }
}

/// Read the `stream` flag for the sidecar path (the SDK path asks the
/// plugin's own request wrapper instead).
fn is_stream_flag(body: &Value) -> bool {
    body.get("stream").and_then(Value::as_bool).unwrap_or(false)
}

/// Sidecar transport: build the outbound HTTP request, relay the response
/// (JSON bytes or SSE stream, byte-faithful).
async fn forward_sidecar(
    instance: &PluginInstance,
    upstream_model: &str,
    body: &Value,
    is_stream: bool,
    timeout_secs: u64,
) -> Result<Response, HostError> {
    let url = instance.url_for("/v1/chat/completions");
    let out_body = rewrite_model(body, upstream_model);
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(timeout_secs))
        .build()
        .map_err(|e| HostError::Upstream(format!("client build failed: {e}")))?;

    let mut request = client
        .post(&url)
        .header(header::CONTENT_TYPE, "application/json")
        .body(out_body);
    if let Some(key) = plugin_key(instance) {
        request = request.bearer_auth(key);
    }

    let upstream = request
        .send()
        .await
        .map_err(|e| HostError::Upstream(format!("plugin `{}` unreachable: {e}", instance.id())))?;

    let status = StatusCode::from_u16(upstream.status().as_u16())
        .map_err(|e| HostError::Upstream(format!("bad upstream status: {e}")))?;

    // Relay client-relevant headers; never relay plugin auth upstream-ward
    // and never leak plugin identity headers.
    let mut relay_headers = HeaderMap::new();
    for name in [&header::CONTENT_TYPE, &header::CACHE_CONTROL] {
        if let Some(v) = upstream.headers().get(name) {
            relay_headers.insert(name, v.clone());
        }
    }
    if relay_headers.get(header::CONTENT_TYPE).is_none() {
        let fallback = if is_stream {
            HeaderValue::from_static("text/event-stream")
        } else {
            HeaderValue::from_static("application/json")
        };
        relay_headers.insert(header::CONTENT_TYPE, fallback);
    }

    if is_stream {
        // Byte-faithful SSE relay: forward raw chunks as they arrive.
        let body_stream = upstream
            .bytes_stream()
            .map(|chunk| chunk.map_err(stream_err));
        match Response::builder()
            .status(status)
            .body(Body::from_stream(body_stream))
        {
            Ok(mut response) => {
                response.headers_mut().extend(relay_headers);
                Ok(response)
            }
            Err(e) => Err(HostError::Upstream(format!("stream relay failed: {e}"))),
        }
    } else {
        // JSON pass-through: raw bytes, status preserved.
        let bytes = upstream.bytes().await.map_err(|e| {
            HostError::Upstream(format!("plugin `{}` body read failed: {e}", instance.id()))
        })?;
        match Response::builder().status(status).body(Body::from(bytes)) {
            Ok(mut response) => {
                response.headers_mut().extend(relay_headers);
                Ok(response)
            }
            Err(e) => Err(HostError::Upstream(format!("relay failed: {e}"))),
        }
    }
}

fn stream_err(e: reqwest::Error) -> std::io::Error {
    std::io::Error::other(e.to_string())
}

/// Aggregate `GET /v1/models` from all servable instances (contract §5):
/// every declared model of every routable plugin, OpenAI list shape.
/// Disabled and non-routable plugins are excluded.
pub fn aggregate_models(state: &AppState) -> Value {
    let mut data = Vec::new();
    for inst in state.instance_list() {
        if !inst.is_servable() {
            continue;
        }
        for model in inst.advertised_models() {
            data.push(json!({
                "id": model,
                "object": "model",
                "created": 0,
                "owned_by": inst.name(),
            }));
        }
    }
    json!({ "object": "list", "data": data })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wants_stream_reads_flag() {
        assert!(wants_stream(&json!({"stream": true})));
        assert!(!wants_stream(&json!({"stream": false})));
        assert!(!wants_stream(&json!({})));
        assert!(!wants_stream(&json!({"stream": "yes"})));
    }

    #[test]
    fn rewrite_model_only_touches_model() {
        let body = json!({"model": "mimo2api/mimo-v2.6-flash", "messages": [{"role":"user","content":"hi"}], "stream": true});
        let out: Value = serde_json::from_str(&rewrite_model(&body, "mimo-v2.6-flash")).unwrap();
        assert_eq!(out["model"], "mimo-v2.6-flash");
        assert_eq!(out["stream"], true);
        assert_eq!(out["messages"], body["messages"]);
    }

    #[test]
    fn bearer_token_parses_header() {
        let mut h = HeaderMap::new();
        assert!(bearer_token(&h).is_none());
        h.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Bearer abc"),
        );
        assert_eq!(bearer_token(&h).as_deref(), Some("abc"));
        h.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("bearer def"),
        );
        assert_eq!(bearer_token(&h).as_deref(), Some("def"));
        h.insert(header::AUTHORIZATION, HeaderValue::from_static("Basic xyz"));
        assert!(bearer_token(&h).is_none());
        h.insert(header::AUTHORIZATION, HeaderValue::from_static("Bearer "));
        assert!(bearer_token(&h).is_none());
    }

    #[test]
    fn openai_error_shape() {
        let e = openai_error("boom", "upstream_error", "upstream_error");
        assert_eq!(e["error"]["type"], "upstream_error");
        assert_eq!(e["error"]["code"], "upstream_error");
        assert!(e["error"]["param"].is_null());
    }

    #[test]
    fn host_error_type_strings_match_openai() {
        // §4 (revised M3): 4xx request/auth/model classes spell
        // `invalid_request_error`, matching OpenAI's actual strings.
        use http_body_util::BodyExt;
        async fn error_body(e: HostError) -> Value {
            let response = e.into_response();
            let bytes = response.into_body().collect().await.unwrap().to_bytes();
            serde_json::from_slice(&bytes).unwrap()
        }

        let runtime = tokio::runtime::Runtime::new().unwrap();
        let body = runtime.block_on(error_body(HostError::InvalidRequest("bad".into())));
        assert_eq!(body["error"]["type"], "invalid_request_error");
        let body = runtime.block_on(error_body(HostError::ModelNotFound("m".into())));
        assert_eq!(body["error"]["type"], "invalid_request_error");
        assert_eq!(body["error"]["code"], "model_not_found");
        let body = runtime.block_on(error_body(HostError::Unauthorized));
        assert_eq!(body["error"]["type"], "invalid_request_error");
        assert_eq!(body["error"]["code"], "invalid_api_key");
        let body = runtime.block_on(error_body(HostError::Upstream("u".into())));
        assert_eq!(body["error"]["type"], "upstream_error");
    }
}
