//! Response emission — port of Go `internal/openai/response.go`:
//! CompletionResponse, StreamChunk (content/reasoning/final flavors),
//! WebSearchResponse, ModelsResponse, ErrorBody. The §6 error-mapping
//! table lives in [`crate::errmap`].
//!
//! Serialization uses `serde_json::Value` + `json!` with Go `omitempty`
//! semantics hand-rolled (empty strings / empty vectors omitted), because
//! the wire shape is the pinned contract.

use serde::Serialize;
use serde_json::json;

use crate::sse::SearchResult;

/// OpenAI usage object (Go `usage`). `prompt_tokens = total_tokens`
/// (OQ-3: only an accumulated total exists upstream; design keeps Go
/// parity).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct UsageOut {
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub total_tokens: i64,
}

impl UsageOut {
    /// The usage object for a completed completion (prompt == total,
    /// Go parity).
    pub fn from_total(total: i64) -> Self {
        UsageOut {
            prompt_tokens: total,
            completion_tokens: 0,
            total_tokens: total,
        }
    }
}

/// The citations array: empty means omit (Go `omitempty`); present when
/// the request ran search-enabled.
fn citations_field(citations: &[SearchResult]) -> Option<serde_json::Value> {
    if citations.is_empty() {
        return None;
    }
    Some(json!(citations))
}

/// Build the non-stream `/v1/chat/completions` response (Go
/// `NewCompletionResponse`): one assistant choice, `reasoning_content`
/// omitted when empty, `citations` omitted when empty, `usage` with
/// prompt_tokens = total_tokens.
pub fn completion_response(
    id: &str,
    model: &str,
    reasoning: &str,
    content: &str,
    total_tokens: i64,
    finish_reason: &str,
    citations: &[SearchResult],
) -> serde_json::Value {
    let mut message = json!({
        "role": "assistant",
        "content": content,
    });
    if !reasoning.is_empty() {
        message["reasoning_content"] = json!(reasoning);
    }
    if let Some(c) = citations_field(citations) {
        message["citations"] = c;
    }
    json!({
        "id": id,
        "object": "chat.completion",
        "created": now_secs(),
        "model": model,
        "choices": [{
            "index": 0,
            "message": message,
            "finish_reason": finish_reason,
        }],
        "usage": UsageOut::from_total(total_tokens),
    })
}

/// Build one content stream chunk (Go `NewStreamChunk`). `role` is
/// `"assistant"` on the first chunk of the completion, `""` after.
pub fn stream_chunk(id: &str, model: &str, content: &str, role: &str) -> serde_json::Value {
    let mut delta = json!({});
    if !role.is_empty() {
        delta["role"] = json!(role);
    }
    delta["content"] = json!(content);
    json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": now_secs(),
        "model": model,
        "choices": [{ "index": 0, "delta": delta, "finish_reason": null }],
    })
}

/// Build one reasoning (deep-thinking) stream chunk (Go
/// `NewReasoningStreamChunk`) — THINK deltas emitted before content
/// deltas.
pub fn reasoning_stream_chunk(
    id: &str,
    model: &str,
    reasoning: &str,
    role: &str,
) -> serde_json::Value {
    let mut delta = json!({});
    if !role.is_empty() {
        delta["role"] = json!(role);
    }
    delta["reasoning_content"] = json!(reasoning);
    json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": now_secs(),
        "model": model,
        "choices": [{ "index": 0, "delta": delta, "finish_reason": null }],
    })
}

/// Build the terminating stream chunk with `finish_reason` + `usage`
/// (Go `NewFinalChunk`). Citations attach on the final chunk only.
pub fn final_chunk(
    id: &str,
    model: &str,
    total_tokens: i64,
    finish_reason: &str,
    citations: &[SearchResult],
) -> serde_json::Value {
    let mut chunk = json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": now_secs(),
        "model": model,
        "choices": [{ "index": 0, "delta": {}, "finish_reason": finish_reason }],
        "usage": UsageOut::from_total(total_tokens),
    });
    if let Some(c) = citations_field(citations) {
        chunk["citations"] = c;
    }
    chunk
}

/// Build the standalone `POST /v1/web_search` response (Go
/// `NewWebSearchResponse`): nil slices become empty arrays so clients
/// see a stable shape.
pub fn web_search_response(queries: &[String], results: &[SearchResult]) -> serde_json::Value {
    json!({
        "object": "web_search",
        "queries": queries,
        "results": results,
    })
}

/// Static creation timestamp (Go: "the model list never changes").
pub const MODELS_CREATED: i64 = 1_735_689_600;

/// Build the `/v1/models` response (Go `ModelsResponse`): one entry for
/// the served model, `owned_by: "simple-chat"`. The `model` argument is
/// the unprefixed id (`deepseek-flash`); the host catalog sees the
/// prefixed form.
pub fn models_response(model: &str) -> serde_json::Value {
    json!({
        "object": "list",
        "data": [{
            "id": model,
            "object": "model",
            "created": MODELS_CREATED,
            "owned_by": "simple-chat",
        }],
    })
}

/// Build the OpenAI error envelope (Go `ErrorBody`).
pub fn error_body(message: &str, err_type: &str, code: &str) -> serde_json::Value {
    json!({
        "error": {
            "message": message,
            "type": err_type,
            "code": code,
        }
    })
}

/// An SSE frame: `data: <json>\n\n`.
pub fn sse_frame(v: &serde_json::Value) -> Vec<u8> {
    let mut out = Vec::with_capacity(64);
    out.extend_from_slice(b"data: ");
    serde_json::to_writer(&mut out, v).expect("chunk serialization is infallible");
    out.extend_from_slice(b"\n\n");
    out
}

/// The terminal SSE frame.
pub fn sse_done_frame() -> Vec<u8> {
    b"data: [DONE]\n\n".to_vec()
}

fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sr(cite: i64) -> SearchResult {
        SearchResult {
            url: format!("https://x/{cite}"),
            title: format!("t{cite}"),
            snippet: "s".into(),
            site_name: String::new(),
            site_icon: String::new(),
            cite_index: cite,
            published_at: 0.0,
            query_indexes: vec![],
            provider: None,
        }
    }

    #[test]
    fn non_stream_response_shape() {
        let v = completion_response(
            "chatcmpl-1",
            "deepseek-flash",
            "",
            "the answer",
            42,
            "stop",
            &[],
        );
        let s = v.to_string();
        for want in [
            r#""object":"chat.completion""#,
            r#""content":"the answer""#,
            r#""total_tokens":42"#,
            r#""finish_reason":"stop""#,
        ] {
            assert!(s.contains(want), "missing {want} in {s}");
        }
        assert!(!s.contains("reasoning_content"));
        assert!(!s.contains("citations"));
        // OQ-3 parity: prompt_tokens mirrors total_tokens.
        assert!(s.contains(r#""prompt_tokens":42"#));
    }

    #[test]
    fn non_stream_response_reasoning_and_citations() {
        let v = completion_response(
            "chatcmpl-1",
            "deepseek-flash",
            "because 2+2",
            "4",
            42,
            "stop",
            &[sr(1), sr(2)],
        );
        let s = v.to_string();
        assert!(s.contains(r#""reasoning_content":"because 2+2""#));
        assert!(s.contains(r#""citations""#));
        assert!(s.contains(r#""cite_index":1"#));
    }

    #[test]
    fn stream_chunk_shape() {
        let v = stream_chunk("chatcmpl-1", "deepseek-flash", "hi", "assistant");
        let s = v.to_string();
        for want in [
            r#""object":"chat.completion.chunk""#,
            r#""role":"assistant""#,
            r#""model":"deepseek-flash""#,
            r#""content":"hi""#,
            r#""finish_reason":null"#,
        ] {
            assert!(s.contains(want), "missing {want} in {s}");
        }
        assert!(!s.contains("usage"));
        assert!(!s.contains("reasoning_content"));
    }

    #[test]
    fn reasoning_chunk_shape() {
        let v = reasoning_stream_chunk("chatcmpl-1", "deepseek-flash", "ponder", "");
        let s = v.to_string();
        assert!(s.contains(r#""reasoning_content":"ponder""#));
        // Empty role must be omitted, not emitted as "".
        assert!(!s.contains(r#""role":"#));
        // Never carries content.
        assert!(!s.contains(r#""content":"#));
    }

    #[test]
    fn reasoning_chunk_role_on_first() {
        let v = reasoning_stream_chunk("chatcmpl-1", "deepseek-flash", "ponder", "assistant");
        assert!(v.to_string().contains(r#""role":"assistant""#));
    }

    #[test]
    fn final_chunk_shape() {
        let v = final_chunk("chatcmpl-1", "deepseek-flash", 41, "stop", &[]);
        let s = v.to_string();
        assert!(s.contains(r#""finish_reason":"stop""#));
        assert!(s.contains(r#""total_tokens":41"#));
        assert!(s.contains(r#""prompt_tokens":41"#));
        assert!(!s.contains("citations"));
        // Final chunk with an upstream finish_reason (mid-stream error
        // tail): never invents "stop".
        let v = final_chunk(
            "chatcmpl-1",
            "deepseek-flash",
            0,
            "parallel_chat_limit",
            &[],
        );
        assert!(v
            .to_string()
            .contains(r#""finish_reason":"parallel_chat_limit""#));
    }

    #[test]
    fn final_chunk_citations_only_here() {
        let v = final_chunk("id", "deepseek-flash", 5, "stop", &[sr(7)]);
        assert!(v.to_string().contains(r#""cite_index":7"#));
    }

    #[test]
    fn web_search_response_stable_shape() {
        let v = web_search_response(&[], &[]);
        let s = v.to_string();
        assert!(s.contains(r#""object":"web_search""#));
        assert!(s.contains(r#""queries":[]"#));
        assert!(s.contains(r#""results":[]"#));
    }

    #[test]
    fn models_response_static() {
        let v = models_response("deepseek-flash");
        let s = v.to_string();
        assert!(s.contains("deepseek-flash"));
        assert!(s.contains(r#""object":"list""#));
        assert!(s.contains(r#""created":1735689600"#));
        assert!(s.contains(r#""owned_by":"simple-chat""#));
    }

    #[test]
    fn error_body_shape() {
        let v = error_body("boom", "invalid_request_error", "bad_thing");
        let s = v.to_string();
        assert!(s.contains(r#""message":"boom""#));
        assert!(s.contains(r#""type":"invalid_request_error""#));
        assert!(s.contains(r#""code":"bad_thing""#));
    }

    #[test]
    fn sse_frames() {
        let f = sse_frame(&stream_chunk("id", "m", "x", "assistant"));
        assert!(f.starts_with(b"data: "));
        assert!(f.ends_with(b"\n\n"));
        let d = sse_done_frame();
        assert_eq!(d, b"data: [DONE]\n\n".to_vec());
    }
}
