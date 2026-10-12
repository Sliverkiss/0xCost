//! SDK-facing request/response types: thin OpenAI-shape wrappers over
//! `serde_json::Value`.
//!
//! Deliberate choice: the host relays chat bodies byte-faithfully, so SDK
//! plugins see exactly what a client sent (`serde_json::Value`) — no
//! host-side struct decoding can drop or reorder fields the plugin passes
//! through. `ChatRequest` still guards the shape (`model` + non-empty
//! `messages`) at construction, so `chat()` implementations never see a
//! body the host itself would have rejected.

use serde::{Deserialize, Serialize};

/// A validated chat completion request: OpenAI wire shape as JSON.
///
/// Construct via `TryFrom<serde_json::Value>` (errors on shape violations)
/// or [`ChatRequest::new`]. The `model` field at this layer is the
/// **upstream** (unprefixed) model id — the host strips the
/// `plugin-name/` prefix before the request reaches the plugin, exactly as
/// it does for external sidecars (contract §5 model routing).
///
/// `Deserialize` is hand-written (not derived) so deserialization validates
/// the shape — a payload that would not pass `ChatRequest::new` cannot
/// exist as a `ChatRequest`.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct ChatRequest {
    inner: serde_json::Value,
}

impl ChatRequest {
    /// Validate and wrap a JSON chat body (`model` string + non-empty
    /// `messages` array required — contract §4 invalid_params rules).
    pub fn new(body: serde_json::Value) -> Result<Self, PluginError> {
        Self::validate(&body)?;
        Ok(Self { inner: body })
    }

    /// The wrapped body. Read-only: plugins may relay it, the host keeps
    /// ownership of what it forwarded.
    pub fn body(&self) -> &serde_json::Value {
        &self.inner
    }

    /// Does the client want an SSE stream?
    pub fn wants_stream(&self) -> bool {
        self.inner
            .get("stream")
            .and_then(serde_json::Value::as_bool)
            .unwrap_or(false)
    }

    fn validate(body: &serde_json::Value) -> Result<(), PluginError> {
        if !body.is_object() {
            return Err(PluginError::invalid_request(
                "request body must be a JSON object",
                Some("body"),
            ));
        }
        if body
            .get("model")
            .and_then(serde_json::Value::as_str)
            .is_none()
        {
            return Err(PluginError::invalid_request(
                "missing `model` field",
                Some("model"),
            ));
        }
        let messages_ok = body
            .get("messages")
            .map(|m| m.as_array().is_some_and(|a| !a.is_empty()))
            .unwrap_or(false);
        if !messages_ok {
            return Err(PluginError::invalid_request(
                "`messages` must be a non-empty array",
                Some("messages"),
            ));
        }
        Ok(())
    }
}

impl TryFrom<serde_json::Value> for ChatRequest {
    type Error = PluginError;

    fn try_from(body: serde_json::Value) -> Result<Self, Self::Error> {
        ChatRequest::new(body)
    }
}

impl<'de> Deserialize<'de> for ChatRequest {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let body = serde_json::Value::deserialize(deserializer)?;
        ChatRequest::new(body).map_err(serde::de::Error::custom)
    }
}

/// The result of one plugin chat call.
///
/// Both streaming and non-streaming outcomes carry the payload the host
/// relays verbatim (contract §4/§5: the host never reshapes plugin
/// payloads). Streaming bodies are the SSE byte sequence including the
/// terminal `data: [DONE]` frame.
#[derive(Debug, Clone, PartialEq)]
pub enum ChatOutcome {
    /// Non-streaming JSON response (one complete chat completion body).
    Json(serde_json::Value),
    /// Streaming response: the full SSE byte sequence as a buffer.
    ///
    /// A chunked-stream variant can be added later without breaking the
    /// trait; canned/deterministic plugins produce single buffers today.
    Sse(Vec<u8>),
}

impl ChatOutcome {
    /// Convenience: wrap a JSON value.
    pub fn json(value: serde_json::Value) -> Self {
        ChatOutcome::Json(value)
    }
}

/// Per-plugin error surface (contract §4 error semantics). Plugins return
/// this; the host maps it onto the OpenAI error body + status verbatim —
/// the same strings a sidecar would emit over HTTP.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("plugin error {status} type={err_type} code={code:?}: {message}")]
pub struct PluginError {
    /// HTTP status the host should relay (400/401/404/429/502 per §4).
    pub status: u16,
    /// OpenAI error `type` string (fixed §4 vocabulary).
    pub err_type: String,
    /// Human-readable message (no upstream sentinel text — §4).
    pub message: String,
    /// Offending parameter name, `None` → `null` on the wire (§4 `param`).
    pub param: Option<String>,
    /// OpenAI error `code` string.
    pub code: Option<String>,
}

impl PluginError {
    /// Build an error with explicit §4 strings.
    pub fn new(status: u16, err_type: &str, message: impl Into<String>) -> Self {
        Self {
            status,
            err_type: err_type.to_string(),
            message: message.into(),
            param: None,
            code: None,
        }
    }

    /// `400 invalid_request_error` — empty `messages` / invalid params.
    pub fn invalid_request(message: impl Into<String>, param: Option<&str>) -> Self {
        Self {
            status: 400,
            err_type: "invalid_request_error".into(),
            message: message.into(),
            param: param.map(str::to_string),
            code: Some("invalid_request".into()),
        }
    }

    /// `429 rate_limit_error` — quota/login-wall sentinel.
    pub fn rate_limit(message: impl Into<String>) -> Self {
        Self {
            status: 429,
            err_type: "rate_limit_error".into(),
            message: message.into(),
            param: None,
            code: Some("rate_limit_error".into()),
        }
    }

    /// `502 upstream_error` — upstream empty response / non-2xx.
    pub fn upstream(message: impl Into<String>) -> Self {
        Self {
            status: 502,
            err_type: "upstream_error".into(),
            message: message.into(),
            param: None,
            code: Some("upstream_error".into()),
        }
    }
}

/// Per-call request context the host passes to the plugin. Reserved for
/// host metadata (request id, deadline, tracing) — M3 keeps it minimal.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RequestContext {
    /// Host-assigned request id (trace correlation), when set.
    pub request_id: Option<String>,
}

/// Plugin health, as the plugin itself reports it. The host folds this
/// into `/healthz` and the lifecycle probe (contract §6).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HealthStatus {
    /// Healthy and ready to serve.
    Ok,
    /// Temporarily not ready (still registered, not routed).
    Unhealthy,
}

impl HealthStatus {
    /// Stable lowercase name (logs, /healthz).
    pub const fn as_str(self) -> &'static str {
        match self {
            HealthStatus::Ok => "ok",
            HealthStatus::Unhealthy => "unhealthy",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn valid_body() -> serde_json::Value {
        json!({"model": "sample-echo/echo-1", "messages": [{"role":"user","content":"hi"}]})
    }

    #[test]
    fn valid_request_wraps_body() {
        let body = valid_body();
        let req = ChatRequest::new(body.clone()).unwrap();
        assert_eq!(req.body(), &body);
        assert!(!req.wants_stream());
    }

    #[test]
    fn stream_flag_detected() {
        let mut body = valid_body();
        body["stream"] = json!(true);
        let req = ChatRequest::new(body).unwrap();
        assert!(req.wants_stream());
    }

    #[test]
    fn missing_model_rejected() {
        let err =
            ChatRequest::new(json!({"messages": [{"role":"user","content":"x"}]})).unwrap_err();
        assert_eq!(err.status, 400);
        assert_eq!(err.err_type, "invalid_request_error");
        assert_eq!(err.param.as_deref(), Some("model"));
    }

    #[test]
    fn empty_messages_rejected() {
        let err = ChatRequest::new(json!({"model": "m", "messages": []})).unwrap_err();
        assert_eq!(err.status, 400);
        assert_eq!(err.param.as_deref(), Some("messages"));
    }

    #[test]
    fn non_object_body_rejected() {
        let err = ChatRequest::new(json!([1, 2, 3])).unwrap_err();
        assert_eq!(err.status, 400);
        assert_eq!(err.param.as_deref(), Some("body"));
    }

    #[test]
    fn try_from_matches_new() {
        assert!(ChatRequest::try_from(valid_body()).is_ok());
        assert!(ChatRequest::try_from(json!({})).is_err());
    }

    #[test]
    fn deserialize_validates() {
        assert!(serde_json::from_value::<ChatRequest>(valid_body()).is_ok());
        assert!(serde_json::from_value::<ChatRequest>(json!({"model": "m"})).is_err());
    }

    #[test]
    fn error_constructors_pin_contract_strings() {
        let e = PluginError::rate_limit("sentinel hit");
        assert_eq!(e.status, 429);
        assert_eq!(e.err_type, "rate_limit_error");

        let e = PluginError::upstream("empty body");
        assert_eq!(e.status, 502);
        assert_eq!(e.err_type, "upstream_error");

        let e = PluginError::invalid_request("no messages", Some("messages"));
        assert_eq!(e.status, 400);
        assert_eq!(e.err_type, "invalid_request_error");
        assert_eq!(e.param.as_deref(), Some("messages"));
    }

    #[test]
    fn health_status_names() {
        assert_eq!(HealthStatus::Ok.as_str(), "ok");
        assert_eq!(HealthStatus::Unhealthy.as_str(), "unhealthy");
    }
}
