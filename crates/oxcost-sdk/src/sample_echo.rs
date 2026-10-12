//! `sample-echo`: deterministic canned chat responses over the SDK path.
//!
//! **Test fixture, not a feature.** This plugin exists so host tests can
//! exercise the in-process SDK path fully offline (no sidecar, no network).
//! It is compiled into the host binary but marked clearly as synthetic; the
//! public host surface (§5) makes no promises about it. See contract §11:
//! only synthetic example fixtures live in the host repository.

use crate::plugin::ChatFuture;
use crate::types::{ChatOutcome, ChatRequest, HealthStatus, PluginError, RequestContext};
use crate::{ModelDescriptor, OxcostPlugin, SdkManifest};
use serde_json::json;

/// Deterministic echo plugin: replies with a canned chat completion echoing
/// the last user message; supports both non-stream and stream shapes so
/// host tests can cover both §5 paths.
///
/// Manifest: `dev.oxcost.sample-echo`, serving `sample-echo/echo-1` (plus
/// any extra `sample-echo/` ids passed at construction).
pub struct SampleEcho {
    manifest: SdkManifest,
    /// When true, every chat call fails with a fixed upstream error —
    /// used to test the host's SDK error mapping.
    fail_upstream: bool,
}

impl SampleEcho {
    /// Echo plugin serving `echo-1` plus any extra `sample-echo/` models.
    pub fn new(extra_models: &[&str]) -> Self {
        let mut models = vec!["sample-echo/echo-1".to_string()];
        models.extend(extra_models.iter().map(|m| m.to_string()));
        Self {
            manifest: SdkManifest::new("dev.oxcost.sample-echo")
                .version("0.1.0")
                .models(models),
            fail_upstream: false,
        }
    }

    /// Variant whose chat calls always return `502 upstream_error`.
    pub fn failing() -> Self {
        let mut p = Self::new(&[]);
        p.fail_upstream = true;
        p
    }

    /// The canned non-stream reply for one request.
    fn reply(&self, req: &ChatRequest) -> serde_json::Value {
        let body = req.body();
        let model = body
            .get("model")
            .and_then(serde_json::Value::as_str)
            .unwrap_or_default()
            .to_string();
        // Echo the last user message's text content.
        let echo = last_user_text(body);
        json!({
            "id": "chatcmpl-sample-echo",
            "object": "chat.completion",
            "created": 0,
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": format!("echo: {echo}")},
                "finish_reason": "stop"
            }],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            "oxcost_sample": true,
        })
    }

    /// The canned SSE byte sequence for one request.
    fn reply_sse(&self, req: &ChatRequest) -> Vec<u8> {
        let body = req.body();
        let model = body
            .get("model")
            .and_then(serde_json::Value::as_str)
            .unwrap_or_default();
        let echo = last_user_text(body);
        let chunk = json!({
            "id": "chatcmpl-sample-echo",
            "object": "chat.completion.chunk",
            "created": 0,
            "model": model,
            "choices": [{
                "index": 0,
                "delta": {"role": "assistant", "content": format!("echo: {echo}")},
                "finish_reason": null
            }],
        })
        .to_string();
        let final_chunk = json!({
            "id": "chatcmpl-sample-echo",
            "object": "chat.completion.chunk",
            "created": 0,
            "model": model,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        })
        .to_string();
        format!("data: {chunk}\n\ndata: {final_chunk}\n\ndata: [DONE]\n\n").into_bytes()
    }
}

/// Text of the last user message in a chat body (empty when absent).
fn last_user_text(body: &serde_json::Value) -> String {
    body.get("messages")
        .and_then(serde_json::Value::as_array)
        .and_then(|msgs| msgs.last())
        .and_then(|last| last.get("content"))
        .and_then(|c| c.as_str())
        .unwrap_or("")
        .to_string()
}

impl OxcostPlugin for SampleEcho {
    fn manifest(&self) -> &SdkManifest {
        &self.manifest
    }

    fn models(&self) -> Vec<ModelDescriptor> {
        self.manifest
            .models
            .iter()
            .map(|m| ModelDescriptor::owned(m, "sample-echo"))
            .collect()
    }

    fn chat(&self, req: ChatRequest, _ctx: RequestContext) -> ChatFuture<'_> {
        Box::pin(async move {
            if self.fail_upstream {
                return Err(PluginError::upstream("sample-echo: canned failure"));
            }
            if req.wants_stream() {
                Ok(ChatOutcome::Sse(self.reply_sse(&req)))
            } else {
                Ok(ChatOutcome::json(self.reply(&req)))
            }
        })
    }

    fn health(&self) -> HealthStatus {
        HealthStatus::Ok
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(model: &str, content: &str, stream: bool) -> ChatRequest {
        ChatRequest::new(json!({
            "model": model,
            "messages": [{"role": "user", "content": content}],
            "stream": stream,
        }))
        .unwrap()
    }

    #[tokio::test]
    async fn echoes_non_stream() {
        let p = SampleEcho::new(&[]);
        let out = p
            .chat(request("echo-1", "hello", false), RequestContext::default())
            .await
            .unwrap();
        match out {
            ChatOutcome::Json(v) => {
                assert_eq!(v["model"], "echo-1");
                assert_eq!(v["choices"][0]["message"]["content"], "echo: hello");
                assert_eq!(v["oxcost_sample"], true);
            }
            other => panic!("expected JSON outcome, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn echoes_stream_with_done_frame() {
        let p = SampleEcho::new(&[]);
        let out = p
            .chat(request("echo-1", "hi", true), RequestContext::default())
            .await
            .unwrap();
        let ChatOutcome::Sse(bytes) = out else {
            panic!("expected SSE outcome")
        };
        let text = String::from_utf8(bytes).unwrap();
        assert!(text.starts_with("data: "));
        assert!(text.ends_with("data: [DONE]\n\n"));
        assert!(text.contains("\"content\":\"echo: hi\""));
        assert!(text.contains("\"finish_reason\":\"stop\""));
    }

    #[tokio::test]
    async fn failing_variant_maps_upstream_error() {
        let p = SampleEcho::failing();
        let err = p
            .chat(request("echo-1", "x", false), RequestContext::default())
            .await
            .unwrap_err();
        assert_eq!(err.status, 502);
        assert_eq!(err.err_type, "upstream_error");
    }

    #[test]
    fn manifest_and_models_consistent() {
        let p = SampleEcho::new(&["sample-echo/echo-2"]);
        assert_eq!(p.manifest().id, "dev.oxcost.sample-echo");
        assert!(p.manifest().validate().is_ok());
        let models = p.models();
        let ids: Vec<&str> = models.iter().map(|m| m.id.as_str()).collect();
        assert_eq!(ids, vec!["sample-echo/echo-1", "sample-echo/echo-2"]);
        assert_eq!(p.health(), HealthStatus::Ok);
    }
}
