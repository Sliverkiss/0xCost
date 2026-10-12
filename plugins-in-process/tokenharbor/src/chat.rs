//! Chat flow: consent gate → temp session → stream → OpenAI mapping
//! (dossier §2/§6; contract §4 error vocabulary).
//!
//! OpenAI mapping decisions (single-turn stateless v1):
//! - The OpenAI `messages` array is flattened into ONE upstream `content`
//!   string (system prompt prefixed, prior turns labeled) — a fresh
//!   temporary session per request keeps the gateway stateless; the
//!   client's own message list carries the multi-turn context.
//! - `thinking` deltas are dropped from content (chat.completions has no
//!   reasoning channel; noted as a limitation in PROTOCOL.md).
//! - `chunk` deltas map to `content` deltas (stream) or concatenated
//!   content (JSON).
//! - Unknown frames (`tool_use`, `citation`, image/file events) are
//!   ignored in v1 (chat-only, no web_search — manifest capabilities).
//!
//! Error mapping (dossier §6 → contract §4):
//! - `account_suspended` → 429 rate_limit_error (login-wall class) AND
//!   the account is quarantined in the store (skipped onward).
//! - `session_unknown` / `missing_session` → 502 upstream_error (internal
//!   session bookkeeping, not the client's fault; client may retry).
//! - `model_unavailable` → 404 invalid_request_error, code
//!   `model_not_found` (bad model id — client's fixable input).
//! - Anything else → 502 upstream_error with `th_<code>`.
//!
//! §4 rule: NO upstream sentinel text in client-facing messages — the
//! message is ours; the code is the machine-readable part.

use std::sync::Arc;

use serde_json::{json, Value};

use oxcost_sdk::{ChatOutcome, ChatRequest, PluginError, RequestContext};

use crate::auth::{Auth, AuthError};
use crate::client::{Client, TransportError};
use crate::sse::{self, Frame, UpstreamError};
use crate::store::Store;

/// Everything the chat flow needs, assembled once by the plugin.
pub struct ChatFlow {
    pub store: Arc<Store>,
    pub auth: Arc<Auth>,
    pub client: Arc<Client>,
}

/// Map an upstream SSE error code onto the §4 plugin error (dossier §6 →
/// contract §4 strings). Upstream message text never leaks (§4).
pub fn map_upstream_error(code: &str) -> PluginError {
    match code {
        "account_suspended" => {
            PluginError::rate_limit("account is suspended upstream; the plugin has quarantined it")
        }
        "model_unavailable" => {
            let mut e = PluginError::new(
                404,
                "invalid_request_error",
                "model not available on this upstream",
            );
            e.code = Some("model_not_found".into());
            e
        }
        "session_unknown" | "missing_session" => PluginError::upstream(
            "upstream rejected the chat session (unknown or missing); retry the request",
        ),
        other => {
            let mut e = PluginError::upstream(format!("upstream stream error code `{other}`"));
            e.code = Some(format!("th_{other}"));
            e
        }
    }
}

/// Flatten an OpenAI `messages` array into one upstream `content` string.
/// System messages become a prefix; prior assistant/user turns are
/// labeled so the model can follow the thread. Image parts are dropped
/// (v1 text-only — chat capability, no image understanding).
pub fn flatten_messages(messages: &Value) -> Result<String, PluginError> {
    let array = messages.as_array().ok_or_else(|| {
        PluginError::invalid_request("`messages` must be an array", Some("messages"))
    })?;
    let mut system = String::new();
    let mut turns: Vec<String> = Vec::new();
    for msg in array {
        let role = msg.get("role").and_then(Value::as_str).unwrap_or("user");
        let content = message_text(msg);
        if content.is_empty() {
            continue;
        }
        match role {
            "system" => {
                if !system.is_empty() {
                    system.push('\n');
                }
                system.push_str(&content);
            }
            "assistant" => turns.push(format!("Assistant: {content}")),
            "tool" | "function" => turns.push(format!("Tool result: {content}")),
            _ => turns.push(format!("User: {content}")),
        }
    }
    let mut out = String::new();
    if !system.is_empty() {
        out.push_str(&system);
        out.push_str("\n\n");
    }
    out.push_str(&turns.join("\n\n"));
    if out.is_empty() {
        return Err(PluginError::invalid_request(
            "no text content found in `messages`",
            Some("messages"),
        ));
    }
    Ok(out)
}

/// Text content of one message: string content directly, parts arrays
/// keep their text parts joined.
fn message_text(msg: &Value) -> String {
    match msg.get("content") {
        Some(Value::String(s)) => s.clone(),
        Some(Value::Array(parts)) => parts
            .iter()
            .filter_map(|p| p.get("text").and_then(Value::as_str))
            .collect::<Vec<_>>()
            .join("\n"),
        _ => String::new(),
    }
}

/// The OpenAI chat.completion id for one request (stable per stream).
fn chunk_id() -> String {
    format!("chatcmpl-th-{}", rand_suffix())
}

/// Short random suffix (no rand dep: time + counter).
fn rand_suffix() -> String {
    use std::sync::atomic::{AtomicU64, Ordering};
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let n = COUNTER.fetch_add(1, Ordering::Relaxed);
    format!("{:x}", n.wrapping_mul(0x9E37_79B9) & 0xffff_ffff)
}

/// Build one OpenAI chunk frame line (`data: {...}\n\n`).
fn chunk_frame(id: &str, created: u64, model: &str, delta: Value, finish: Option<&str>) -> Vec<u8> {
    let chunk = json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
        "choices": [{
            "index": 0,
            "delta": delta,
            "finish_reason": finish,
        }],
    });
    format!("data: {chunk}\n\n").into_bytes()
}

/// Frames → OpenAI SSE byte sequence (terminal `[DONE]` included).
///
/// Emits: one role-frame, one content frame per `chunk`, one final
/// finish frame with usage from `done` (OpenAI streams put usage on the
/// final frame with `stream_options.include_usage`, but the plugin adds
/// it unconditionally — harmless, informative).
pub fn frames_to_openai_sse(
    frames: &[Frame],
    model: &str,
    created: u64,
) -> Result<Vec<u8>, PluginError> {
    let id = chunk_id();
    let mut out = Vec::new();
    out.extend_from_slice(&chunk_frame(
        &id,
        created,
        model,
        json!({"role": "assistant"}),
        None,
    ));
    for frame in frames {
        match frame {
            Frame::Chunk(delta) => {
                if !delta.is_empty() {
                    out.extend_from_slice(&chunk_frame(
                        &id,
                        created,
                        model,
                        json!({"content": delta}),
                        None,
                    ));
                }
            }
            Frame::Done(done) => {
                out.extend_from_slice(&final_frame(&id, created, model, done));
            }
            // thinking/tool_use/citation/image/file: not mapped (v1) —
            // Frame::Error is handled by the caller before this point.
            _ => {}
        }
    }
    out.extend_from_slice(b"data: [DONE]\n\n");
    Ok(out)
}

/// The finish frame: empty delta, `finish_reason: "stop"`, usage from the
/// upstream `done` payload.
fn final_frame(id: &str, created: u64, model: &str, done: &crate::sse::DonePayload) -> Vec<u8> {
    let mut chunk = json!({
        "id": id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": done.model.clone().unwrap_or_else(|| model.to_string()),
        "choices": [{
            "index": 0,
            "delta": {},
            "finish_reason": "stop",
        }],
    });
    if done.input_tokens.is_some() || done.output_tokens.is_some() {
        let input = done.input_tokens.unwrap_or(0);
        let output = done.output_tokens.unwrap_or(0);
        chunk["usage"] = json!({
            "prompt_tokens": input,
            "completion_tokens": output,
            "total_tokens": input + output,
        });
    }
    format!("data: {chunk}\n\n").into_bytes()
}

/// Frames → one non-streaming OpenAI chat.completion JSON body.
pub fn frames_to_openai_json(
    frames: &[Frame],
    model: &str,
    created: u64,
) -> Result<Value, PluginError> {
    let id = chunk_id();
    let mut content = String::new();
    let mut done = None;
    for frame in frames {
        match frame {
            Frame::Chunk(delta) => content.push_str(delta),
            Frame::Done(d) => done = Some(d.clone()),
            _ => {}
        }
    }
    let mut body = json!({
        "id": id,
        "object": "chat.completion",
        "created": created,
        "model": done
            .as_ref()
            .and_then(|d| d.model.clone())
            .unwrap_or_else(|| model.to_string()),
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": content},
            "finish_reason": "stop",
        }],
    });
    if let Some(d) = done {
        if d.input_tokens.is_some() || d.output_tokens.is_some() {
            let input = d.input_tokens.unwrap_or(0);
            let output = d.output_tokens.unwrap_or(0);
            body["usage"] = json!({
                "prompt_tokens": input,
                "completion_tokens": output,
                "total_tokens": input + output,
            });
        }
    }
    Ok(body)
}

impl ChatFlow {
    /// One full chat request → OpenAI outcome (§4 errors on failure).
    pub async fn chat(
        &self,
        req: &ChatRequest,
        _ctx: &RequestContext,
    ) -> Result<ChatOutcome, PluginError> {
        let body = req.body();
        let model = body
            .get("model")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let messages = body
            .get("messages")
            .cloned()
            .ok_or_else(|| PluginError::invalid_request("missing `messages`", Some("messages")))?;
        let content = flatten_messages(&messages)?;
        let wants_stream = req.wants_stream();
        let created = chrono::Utc::now().timestamp().unsigned_abs();

        // Pick the first non-quarantined account (v1: single account is
        // sufficient; the loop is the LB seam for later).
        let account = self
            .store
            .accounts()
            .map_err(internal)?
            .into_iter()
            .find(|a| a.quarantined_reason.is_none())
            .ok_or_else(|| {
                PluginError::rate_limit("no usable TokenHarbor account (all quarantined)")
            })?;

        // Auth + consent.
        let cookie = self.ensure_ready(&account).await?;

        // Temp session (upstream cap hygiene: deleted after the stream).
        let session_id = self.create_session(&account, &cookie, model).await?;

        // Stream.
        let stream_body = json!({
            "sessionId": session_id,
            "content": content,
            "model": model,
            "webSearch": "off",
            "tz": "UTC+0000",
        });
        let resp = self
            .client
            .post_stream("/api/direct-chat/stream", &stream_body, &cookie)
            .await
            .map_err(transport)?;

        // Parse BEFORE mapping (dossier §6: non-200 bodies are still SSE
        // error frames with machine-readable codes).
        let frames = sse::parse(&resp.body);
        if let Some(err) = sse::first_error(&frames) {
            return Err(self.handle_stream_error(&account, err).await);
        }
        if resp.status != 200 && resp.status != 201 {
            return Err(PluginError::upstream(format!(
                "upstream stream returned status {}",
                resp.status
            )));
        }

        // Cleanup (best-effort; temporary sessions age out upstream anyway).
        let cleanup_cookie = cookie.clone();
        let client = Arc::clone(&self.client);
        let sid = session_id.clone();
        tokio::spawn(async move {
            let _ = client
                .delete(&format!("/api/direct-chat/sessions/{sid}"), &cleanup_cookie)
                .await;
        });

        if wants_stream {
            let bytes = frames_to_openai_sse(&frames, model, created)?;
            Ok(ChatOutcome::Sse(bytes))
        } else {
            let value = frames_to_openai_json(&frames, model, created)?;
            Ok(ChatOutcome::Json(value))
        }
    }

    /// Ensure the account has a live cookie AND consented to free models
    /// (dossier §2: the privacy gate is real and default-off; the POST is
    /// idempotent, so we do it once per account, persisted in the store).
    async fn ensure_ready(
        &self,
        account: &crate::store::AccountRow,
    ) -> Result<String, PluginError> {
        let cookie = self.auth.ensure_session(account).await.map_err(auth_err)?;
        if !account.consent_accepted {
            self.client
                .post_json(
                    "/api/me/privacy",
                    &json!({"free_models_enabled": true}),
                    &cookie,
                )
                .await
                .map_err(transport)?;
            self.store
                .set_consent(&account.email, true)
                .map_err(internal)?;
        }
        Ok(cookie)
    }

    /// Create a temporary session; maps 401 → one refresh+retry.
    async fn create_session(
        &self,
        account: &crate::store::AccountRow,
        cookie: &str,
        model: &str,
    ) -> Result<String, PluginError> {
        let body = json!({"model": model, "temporary": true});
        let mut resp = self
            .client
            .post_json("/api/direct-chat/sessions", &body, cookie)
            .await
            .map_err(transport)?;
        if resp.status == 401 {
            // Refresh-on-401 (single retry): the cookie's access token may
            // have expired between ensure_ready and this call.
            let refreshed = self.auth.ensure_session(account).await.map_err(auth_err)?;
            resp = self
                .client
                .post_json("/api/direct-chat/sessions", &body, &refreshed)
                .await
                .map_err(transport)?;
        }
        if resp.status != 201 && resp.status != 200 {
            return Err(PluginError::upstream(format!(
                "session create returned status {}: {}",
                resp.status,
                truncate(&resp.text(), 120)
            )));
        }
        let parsed: Value = serde_json::from_slice(&resp.body)
            .map_err(|_| PluginError::upstream("session create response was not valid JSON"))?;
        let id = parsed
            .pointer("/session/id")
            .and_then(Value::as_str)
            .ok_or_else(|| PluginError::upstream("session create response missing session.id"))?;
        Ok(id.to_string())
    }

    /// Map an upstream error frame: quarantine on `account_suspended` so
    /// the account is skipped from the next request onward.
    async fn handle_stream_error(
        &self,
        account: &crate::store::AccountRow,
        err: &UpstreamError,
    ) -> PluginError {
        if err.code == "account_suspended" {
            let now = chrono::Utc::now().timestamp();
            if let Err(e) =
                self.store
                    .set_quarantine(&account.email, Some("account_suspended"), now)
            {
                tracing::warn!(email = %account.email, error = %e, "failed to persist quarantine");
            }
            tracing::warn!(email = %account.email, "account quarantined upstream (account_suspended)");
        }
        map_upstream_error(&err.code)
    }
}

fn internal(e: crate::store::StoreError) -> PluginError {
    tracing::error!(error = %e, "plugin store failure");
    PluginError::upstream("plugin internal storage failure")
}

fn auth_err(e: AuthError) -> PluginError {
    // Distinguish login-wall (429) from transport (502): a failed login is
    // a credentials problem the operator must fix; surface as 502 with a
    // distinct code so it is visible without leaking the detail.
    tracing::error!(error = %e, "auth failure");
    let mut err = PluginError::upstream("plugin upstream authentication failed");
    err.code = Some("th_auth_failed".into());
    err
}

fn transport(e: TransportError) -> PluginError {
    match e {
        TransportError::Status { status, detail } => {
            PluginError::upstream(format!("upstream returned {status}: {detail}"))
        }
        other => {
            tracing::error!(error = %other, "transport failure");
            PluginError::upstream("plugin upstream transport failure")
        }
    }
}

fn truncate(s: &str, n: usize) -> String {
    if s.len() <= n {
        s.to_string()
    } else {
        format!("{}…", &s[..n])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SUCCESS: &str = include_str!("../fixtures/sse-success.txt");

    fn frames() -> Vec<Frame> {
        sse::parse(SUCCESS.as_bytes())
    }

    #[test]
    fn map_account_suspended() {
        let e = map_upstream_error("account_suspended");
        assert_eq!(e.status, 429);
        assert_eq!(e.err_type, "rate_limit_error");
    }

    #[test]
    fn map_model_unavailable() {
        let e = map_upstream_error("model_unavailable");
        assert_eq!(e.status, 404);
        assert_eq!(e.err_type, "invalid_request_error");
        assert_eq!(e.code.as_deref(), Some("model_not_found"));
    }

    #[test]
    fn map_session_codes() {
        for code in ["session_unknown", "missing_session"] {
            let e = map_upstream_error(code);
            assert_eq!(e.status, 502, "{code}");
            assert_eq!(e.err_type, "upstream_error");
        }
    }

    #[test]
    fn map_unknown_code_is_upstream_with_code() {
        let e = map_upstream_error("too_many_sessions");
        assert_eq!(e.status, 502);
        assert_eq!(e.code.as_deref(), Some("th_too_many_sessions"));
    }

    #[test]
    fn flatten_labels_roles_and_prefixes_system() {
        let messages = json!([
            {"role": "system", "content": "Be terse."},
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "hello"},
            {"role": "user", "content": "bye"},
        ]);
        let out = flatten_messages(&messages).unwrap();
        assert!(out.starts_with("Be terse.\n\n"));
        assert!(out.contains("User: hi"));
        assert!(out.contains("Assistant: hello"));
        assert!(out.ends_with("User: bye"));
    }

    #[test]
    fn flatten_joins_text_parts_and_drops_images() {
        let messages = json!([
            {"role": "user", "content": [
                {"type": "text", "text": "look"},
                {"type": "image_url", "image_url": {"url": "https://x.test/a.png"}},
                {"type": "text", "text": "here"},
            ]},
        ]);
        assert_eq!(flatten_messages(&messages).unwrap(), "User: look\nhere");
    }

    #[test]
    fn flatten_rejects_textless_input() {
        let messages = json!([{"role": "user", "content": [
            {"type": "image_url", "image_url": {"url": "https://x.test/a.png"}}
        ]}]);
        let e = flatten_messages(&messages).unwrap_err();
        assert_eq!(e.status, 400);
        assert_eq!(e.param.as_deref(), Some("messages"));
    }

    #[test]
    fn sse_outcome_is_openai_shaped() {
        let bytes = frames_to_openai_sse(&frames(), "xiaomi/mimo-v2.6-flash:free", 1000).unwrap();
        let text = String::from_utf8(bytes.clone()).unwrap();
        assert!(text.starts_with("data: {"));
        assert!(text.ends_with("data: [DONE]\n\n"));
        assert!(text.contains("\"object\":\"chat.completion.chunk\""));
        assert!(text.contains("\"content\":\"OK\""));
        assert!(text.contains("\"content\":\" — done.\""));
        assert!(text.contains("\"finish_reason\":\"stop\""));
        assert!(text.contains("\"prompt_tokens\":12"));
        // thinking deltas must NOT appear as content.
        assert!(!text.contains("Let me think"));
    }

    #[test]
    fn json_outcome_is_openai_shaped() {
        let body = frames_to_openai_json(&frames(), "xiaomi/mimo-v2.6-flash:free", 1000).unwrap();
        assert_eq!(body["object"], "chat.completion");
        assert_eq!(body["choices"][0]["message"]["content"], "OK — done.");
        assert_eq!(body["choices"][0]["finish_reason"], "stop");
        assert_eq!(body["usage"]["prompt_tokens"], 12);
        assert_eq!(body["usage"]["completion_tokens"], 3);
        assert_eq!(body["usage"]["total_tokens"], 15);
        // Model comes from the done frame when present.
        assert_eq!(body["model"], "xiaomi/mimo-v2.6-flash:free");
    }

    #[test]
    fn json_outcome_without_usage_omits_field() {
        let frames = vec![
            Frame::Chunk("hi".into()),
            Frame::Done(crate::sse::DonePayload {
                message_id: Some("m".into()),
                model: None,
                search_count: None,
                input_tokens: None,
                output_tokens: None,
            }),
        ];
        let body = frames_to_openai_json(&frames, "m", 0).unwrap();
        assert!(body.get("usage").is_none());
    }
}
