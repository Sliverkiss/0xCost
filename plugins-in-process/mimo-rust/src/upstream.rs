//! Upstream chat client (Go `internal/mimo/client.go` Chat path):
//! payload build, channel routing, browser headers, watchdogs, and the
//! error taxonomy the retry ladder consumes.
//!
//! Watchdog port note: the Go client runs two timers — a pre-header guard
//! (no bytes at all → ErrSilentUpstream, no account penalty, ADR 0036) and
//! a body idle watchdog (silence between events → ErrIdleStall, transport
//! class). With reqwest the request future itself is the pre-header
//! window: the call future is raced against the silence window instead of
//! a separate timer. The body watchdog maps onto reading the SSE body —
//! in this single-buffer port the whole body is read under one silence
//! window (PORT_DESIGN §4.2).
//!
//! `chat` mints a fresh msgId every call — upstream dedupes by msgId, so
//! retries re-enter `chat` rather than resend (SDD §2.4).

use std::time::Duration;

use serde::Serialize;

use crate::login::Client as LoginClient;
use crate::models::is_fastchat_model;
use crate::openai::random_hex32;
use crate::sse::InBandError;
use crate::sse::{parse_events, Streamer};

/// One upstream chat turn request (Go ChatRequest).
#[derive(Debug, Clone, Default)]
pub struct UpstreamChatRequest {
    pub query: String,
    /// Normalized upstream model id.
    pub model: String,
    /// 32-hex; empty = mint fresh here.
    pub conversation_id: String,
    /// Chaining anchor; empty = null (first turn).
    pub previous_dialog_id: String,
    pub enable_thinking: bool,
    /// Empty = "disabled" (search is opt-in, M19).
    pub web_search_status: String,
    pub temperature: Option<f64>,
    pub top_p: Option<f64>,
    /// Per-turn silence window override (the pool lease window; zero = the
    /// login client's configured default, ADR 0036).
    pub idle_timeout: Duration,
}

/// The completed turn (Go ChatResult).
#[derive(Debug, Clone, Default)]
pub struct ChatResult {
    /// This turn's dialogId → next turn's previousDialogueId.
    pub dialog_id: String,
    pub reasoning: String,
    pub content: String,
    pub usage: Option<crate::sse::MiMoUsage>,
    /// Web-search results the upstream returned inline (M19).
    pub citations: Vec<crate::sse::Citation>,
    /// Reasons a web_search payload was skipped — surfaced, never dropped
    /// silently.
    pub citation_warnings: Vec<String>,
}

/// A non-200 chat response (Go UpstreamError). `status` drives pool health
/// (429/5xx cooldown, 401 auth refresh, 451/461 disable) and API error
/// mapping (TDD ERRMAP-006..008). Retry-After is kept for diagnostics only
/// — it is never echoed to the client (TDD ERRMAP-011).
#[derive(Debug, Clone)]
pub struct UpstreamError {
    pub status: u16,
    pub body: String,
    pub retry_after: Option<String>,
}

impl std::fmt::Display for UpstreamError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "upstream {}: {}", self.status, self.body)
    }
}

impl std::error::Error for UpstreamError {}

/// Upstream error taxonomy (Go chat-path sentinels). The retry ladder
/// (chat.rs) matches on these variants; they carry no OpenAI mapping —
/// that is errmap.rs's job.
#[derive(Debug, thiserror::Error)]
pub enum UpstreamChatError {
    /// Transport-level failure (connect/timeout/read).
    #[error("chat request: {0}")]
    Transport(String),
    /// The upstream sent nothing at all within the silence window — not
    /// even headers. Our own patience ran out; the account is healthy
    /// (ADR 0036, POOLATTR-012): no pool penalty, surfaced as our 504.
    #[error("chat request: upstream sent no bytes within the silence window ({window:?})")]
    SilentUpstream { window: Duration },
    /// The upstream stream produced no SSE event within the idle window
    /// (SDD §7.3) — a transport-class problem: cooldown ladder applies.
    #[error("upstream stream stalled (no SSE event within idle timeout)")]
    IdleStall,
    /// A non-200 response.
    #[error(transparent)]
    Upstream(UpstreamError),
    /// 401 from the chat endpoint — retry layer refreshes the session once.
    #[error("auth expired: {0}")]
    AuthExpired(Box<UpstreamError>),
    /// The stream finished with zero message deltas (TDD SSE-013): the
    /// caller retries once with fresh ids.
    #[error("upstream returned an empty response")]
    EmptyResponse,
    /// In-band SSE error (the platform answered with an error event):
    /// busy / model-rejected / content-filter / stream-shape.
    #[error("upstream in-band error: {0:?}")]
    InBand(InBandError),
}

impl UpstreamChatError {
    /// Auth-expired signal (Go IsAuthExpiredErr): the retry layer refreshes
    /// the session once, transparently.
    pub fn is_auth_expired(&self) -> bool {
        matches!(self, UpstreamChatError::AuthExpired(_))
    }
}

/// Upstream modelConfig (TDD API-022: temperature/top_p are optional so
/// they are omitted entirely when the caller gave none).
#[derive(Debug, Clone, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
struct ModelConfig {
    model: String,
    enable_thinking: bool,
    web_search_status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    temperature: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    top_p: Option<f64>,
}

/// Upstream chatPayload (SDD §2.4). `multi_medias` must serialize as `[]`
/// — never null (web_client.go:121-125). v1 chat has no attachments, so
/// the array is always empty but always present.
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ChatPayload {
    msg_id: String,
    conversation_id: String,
    query: String,
    is_edited_query: bool,
    #[serde(rename = "previousDialogueId")]
    previous_dialog_id: Option<String>,
    scene_type: Option<serde_json::Value>,
    params: serde_json::Value,
    model_config: ModelConfig,
    multi_medias: Vec<serde_json::Value>,
}

/// searchStatusOrDefault (M19): empty/unknown → "disabled".
fn search_status_or_default(status: &str) -> String {
    match status.trim().to_lowercase().as_str() {
        "auto" => "auto".into(),
        "enabled" => "enabled".into(),
        _ => "disabled".into(),
    }
}

/// Build the upstream POST body for one chat turn.
fn build_payload(req: &UpstreamChatRequest) -> ChatPayload {
    ChatPayload {
        msg_id: random_hex32(),
        conversation_id: if req.conversation_id.is_empty() {
            random_hex32()
        } else {
            req.conversation_id.clone()
        },
        query: req.query.clone(),
        is_edited_query: false,
        previous_dialog_id: if req.previous_dialog_id.is_empty() {
            None
        } else {
            Some(req.previous_dialog_id.clone())
        },
        scene_type: None,
        params: serde_json::json!({}),
        model_config: ModelConfig {
            model: req.model.clone(),
            enable_thinking: req.enable_thinking,
            web_search_status: search_status_or_default(&req.web_search_status),
            temperature: req.temperature,
            top_p: req.top_p,
        },
        multi_medias: Vec::new(),
    }
}

/// Channel routing (SDD §2.4): ultraspeed models must use the fastchat
/// prefix — the normal channel rejects them with a model-name error.
fn chat_path(model: &str) -> &'static str {
    if is_fastchat_model(model) {
        "/fastchat/open-apis/bot/chat"
    } else {
        "/open-apis/bot/chat"
    }
}

/// The per-request upstream client (Go Client.Chat bound to cookie+ph).
#[derive(Clone)]
pub struct ChatClient {
    login: LoginClient,
}

impl ChatClient {
    /// Wrap a login client (identity is the Cookie header plus the
    /// xiaomichatbot_ph query parameter, never an Authorization header).
    pub fn new(login: LoginClient) -> Self {
        Self { login }
    }

    /// One upstream chat turn (Go Chat, SDD §2.4). No retry logic lives
    /// here: account switch / auth refresh / empty-response retries are the
    /// pipeline's job (SDD §7.1/§7.2).
    pub async fn chat(
        &self,
        cookie: &str,
        ph: &str,
        req: &UpstreamChatRequest,
    ) -> Result<ChatResult, UpstreamChatError> {
        let payload = build_payload(req);
        let body = serde_json::to_vec(&payload)
            .map_err(|e| UpstreamChatError::Transport(format!("payload encode: {e}")))?;
        let path = chat_path(&req.model);
        // xiaomichatbot_ph is url-encoded: the ph is cookie-shaped and may
        // contain characters a raw query would break on.
        let ph_enc = url::form_urlencoded::byte_serialize(ph.as_bytes()).collect::<String>();
        let target = format!("{}{}?xiaomichatbot_ph={}", self.login.base(), path, ph_enc);

        // Silence window (ADR 0036): per-turn override, else the client's
        // default. This one number bounds both the turn and the pool slot
        // it holds.
        let idle = if req.idle_timeout.is_zero() {
            self.login.idle_timeout()
        } else {
            req.idle_timeout
        };

        // Pre-first-byte guard (Go preHeader timer): the request future is
        // raced against the window; a platform still thinking (search
        // fetching pages, before anything reaches the wire) trips it as
        // SilentUpstream — no account penalty. A zero window disables the
        // guard (tests).
        let send_fut = self.login.chat_post(target, cookie, body);
        let resp = if idle.is_zero() {
            send_fut
                .await
                .map_err(|e| UpstreamChatError::Transport(format!("chat request: {e}")))?
        } else {
            match tokio::time::timeout(idle, send_fut).await {
                Err(_) => return Err(UpstreamChatError::SilentUpstream { window: idle }),
                Ok(r) => {
                    r.map_err(|e| UpstreamChatError::Transport(format!("chat request: {e}")))?
                }
            }
        };

        let status = resp.status().as_u16();
        if status != 200 {
            // Retry-After is harvested before the body consumes the
            // response (headers are diagnostics-only, TDD ERRMAP-011).
            let retry_after = resp
                .headers()
                .get(reqwest::header::RETRY_AFTER)
                .and_then(|v| v.to_str().ok())
                .map(str::to_string);
            // Error body capped at 64 KiB (Go io.LimitReader).
            let body_bytes = resp
                .bytes()
                .await
                .map_err(|e| UpstreamChatError::Transport(format!("error body: {e}")))?;
            let body = String::from_utf8_lossy(&body_bytes[..body_bytes.len().min(64 * 1024)])
                .into_owned();
            let ue = UpstreamError {
                status,
                body,
                retry_after,
            };
            if status == 401 {
                return Err(UpstreamChatError::AuthExpired(Box::new(ue)));
            }
            return Err(UpstreamChatError::Upstream(ue));
        }

        // Body watchdog (SDD §7.3): headers have arrived, so silence while
        // reading the SSE body is a stall (transport class, ADR 0036).
        let body_fut = resp.bytes();
        let body = if idle.is_zero() {
            body_fut
                .await
                .map_err(|e| UpstreamChatError::Transport(format!("chat body: {e}")))?
        } else {
            match tokio::time::timeout(idle, body_fut).await {
                Err(_) => return Err(UpstreamChatError::IdleStall),
                Ok(r) => r.map_err(|e| UpstreamChatError::Transport(format!("chat body: {e}")))?,
            }
        };

        // Parse the SSE byte stream. A stream that ends without a finish
        // [DONE] frame is an error (EOF-before-finish, TDD SSE-014);
        // in-band error events fail the turn with their classification.
        let text = String::from_utf8_lossy(&body).into_owned();
        let mut streamer = Streamer::new();
        if let Err(e) = parse_events(&text, &mut streamer) {
            let _ = streamer.finish(); // flush splitter state even on failure
            return Err(UpstreamChatError::InBand(e));
        }
        let turn = streamer.finish();
        if turn.reasoning.is_empty() && turn.content.is_empty() {
            return Err(UpstreamChatError::EmptyResponse);
        }
        Ok(ChatResult {
            dialog_id: turn.dialog_id,
            reasoning: turn.reasoning,
            content: turn.content,
            usage: turn.usage,
            citations: turn.citations,
            citation_warnings: streamer.citations_parse_errors,
        })
    }
}

/// In-band error payload preserved for the pipeline's error mapping: a
/// busy/model-rejected/content-filter event carries the upstream message.
pub fn in_band_message(e: &InBandError) -> String {
    e.upstream_message()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn req(model: &str) -> UpstreamChatRequest {
        UpstreamChatRequest {
            query: "hello".into(),
            model: model.into(),
            ..Default::default()
        }
    }

    fn sse_stream(events: &[String]) -> String {
        let mut out = String::new();
        for ev in events {
            out.push_str(ev);
            out.push_str("\n\n");
        }
        out
    }

    async fn mock_client(base: &str, idle: Duration) -> ChatClient {
        let login = LoginClient::new(crate::login::Options {
            base: Some(base.to_string()),
            idle_timeout: Some(idle),
            ..Default::default()
        })
        .unwrap();
        ChatClient::new(login)
    }

    async fn mount(server: &wiremock::MockServer, status: u16, body: String) {
        wiremock::Mock::given(wiremock::matchers::method("POST"))
            .and(wiremock::matchers::path("/open-apis/bot/chat"))
            .respond_with(
                wiremock::ResponseTemplate::new(status)
                    .insert_header("content-type", "text/event-stream")
                    .set_body_string(body),
            )
            .mount(server)
            .await;
    }

    #[test]
    fn search_status_defaults_to_disabled() {
        assert_eq!(search_status_or_default(""), "disabled");
        assert_eq!(search_status_or_default("  "), "disabled");
        assert_eq!(search_status_or_default("enabled"), "enabled");
        assert_eq!(search_status_or_default(" AUTO "), "auto");
        assert_eq!(search_status_or_default("weird"), "disabled");
    }

    // 1.1/1.2: payload shape — multiMedias [] not null, previousDialogueId
    // null on first turn, temperature omitted when absent (TDD API-022).
    #[test]
    fn payload_shape_first_turn() {
        let p = serde_json::to_value(build_payload(&req("mimo-v2.6-pro"))).unwrap();
        assert_eq!(p["msgId"].as_str().unwrap().len(), 32);
        assert_eq!(p["conversationId"].as_str().unwrap().len(), 32);
        assert_eq!(p["query"], "hello");
        assert_eq!(p["isEditedQuery"], false);
        assert!(p["previousDialogueId"].is_null());
        assert!(p["sceneType"].is_null());
        assert_eq!(p["params"], serde_json::json!({}));
        assert_eq!(p["multiMedias"], serde_json::json!([]));
        assert_eq!(p["modelConfig"]["model"], "mimo-v2.6-pro");
        assert_eq!(p["modelConfig"]["enableThinking"], false);
        assert_eq!(p["modelConfig"]["webSearchStatus"], "disabled");
        assert!(p["modelConfig"].get("temperature").is_none());
        assert!(p["modelConfig"].get("topP").is_none());
    }

    #[test]
    fn payload_shape_chained_turn() {
        let mut r = req("mimo-v2.6-flash");
        r.conversation_id = "c".repeat(32);
        r.previous_dialog_id = "d".repeat(32);
        r.enable_thinking = true;
        r.web_search_status = "auto".into();
        r.temperature = Some(0.7);
        r.top_p = Some(0.9);
        let p = serde_json::to_value(build_payload(&r)).unwrap();
        assert_eq!(p["conversationId"], "c".repeat(32));
        assert_eq!(p["previousDialogueId"], "d".repeat(32));
        assert_eq!(p["modelConfig"]["enableThinking"], true);
        assert_eq!(p["modelConfig"]["webSearchStatus"], "auto");
        assert_eq!(p["modelConfig"]["temperature"], 0.7);
        assert_eq!(p["modelConfig"]["topP"], 0.9);
    }

    // 1.2: channel routing — ultraspeed → fastchat path.
    #[test]
    fn channel_routing() {
        assert_eq!(chat_path("mimo-v2.6-pro"), "/open-apis/bot/chat");
        assert_eq!(chat_path("mimo-v2.6-flash"), "/open-apis/bot/chat");
        assert_eq!(
            chat_path("mimo-v2.6-pro-ultraspeed-studio"),
            "/fastchat/open-apis/bot/chat"
        );
    }

    #[test]
    fn upstream_error_display() {
        let e = UpstreamError {
            status: 502,
            body: "bad".into(),
            retry_after: None,
        };
        assert_eq!(e.to_string(), "upstream 502: bad");
    }

    #[test]
    fn auth_expired_flag() {
        let e = UpstreamChatError::AuthExpired(Box::new(UpstreamError {
            status: 401,
            body: String::new(),
            retry_after: None,
        }));
        assert!(e.is_auth_expired());
        assert!(!UpstreamChatError::EmptyResponse.is_auth_expired());
    }

    #[test]
    fn in_band_message_extraction() {
        assert_eq!(in_band_message(&InBandError::Busy("m".into())), "m");
        assert_eq!(
            in_band_message(&InBandError::ContentFilter {
                reason: "sensitive_query".into(),
                message: "m".into()
            }),
            "content filtered by upstream (sensitive_query): m"
        );
        assert_eq!(in_band_message(&InBandError::Stream("eof".into())), "eof");
    }

    #[tokio::test]
    async fn chat_happy_path_assembles_turn() {
        let server = wiremock::MockServer::start().await;
        let events = sse_stream(&[
            r#"event:message
data:{"type":"delta","content":"hello"}"#
                .into(),
            r#"event:message
data:{"type":"delta","content":" world"}"#
                .into(),
            r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#
                .into(),
        ]);
        mount(&server, 200, events).await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let r = cc
            .chat("cookie=1", "ph-token", &req("mimo-v2.6-pro"))
            .await
            .unwrap();
        assert_eq!(r.content, "hello world");
        assert!(r.dialog_id.is_empty());
        assert!(r.citations.is_empty());
    }

    #[tokio::test]
    async fn chat_happy_path_reasoning_usage_dialog_and_citations() {
        let server = wiremock::MockServer::start().await;
        let events = sse_stream(&[
            r#"event:dialogId
data:{"type":"id","content":"dlg-9"}"#
                .into(),
            r#"event:message
data:{"type":"delta","content":"<think>why"}"#
                .into(),
            r#"event:message
data:{"type":"delta","content":"</think>answer"}"#
                .into(),
            r#"event:usage
data:{"promptTokens":5,"completionTokens":7,"totalTokens":12}"#
                .into(),
            r#"event:web_search
data:[{"url":"https://a"}]"#
                .into(),
            r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#
                .into(),
        ]);
        mount(&server, 200, events).await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let r = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-flash"))
            .await
            .unwrap();
        assert_eq!(r.reasoning, "why");
        assert_eq!(r.content, "answer");
        assert_eq!(r.dialog_id, "dlg-9");
        let usage = r.usage.unwrap();
        assert_eq!(usage.total_tokens, 12);
        assert_eq!(r.citations.len(), 1);
        assert_eq!(r.citations[0].url, "https://a");
    }

    #[tokio::test]
    async fn chat_401_maps_to_auth_expired() {
        let server = wiremock::MockServer::start().await;
        mount(&server, 401, "expired".into()).await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        match e {
            UpstreamChatError::AuthExpired(ue) => {
                assert_eq!(ue.status, 401);
                assert_eq!(ue.body, "expired");
            }
            other => panic!("expected AuthExpired, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn chat_5xx_maps_to_upstream_error_with_retry_after_kept() {
        let server = wiremock::MockServer::start().await;
        wiremock::Mock::given(wiremock::matchers::method("POST"))
            .and(wiremock::matchers::path("/open-apis/bot/chat"))
            .respond_with(
                wiremock::ResponseTemplate::new(502)
                    .insert_header("retry-after", "7")
                    .set_body_string("upstream exploded"),
            )
            .mount(&server)
            .await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        match e {
            UpstreamChatError::Upstream(ue) => {
                assert_eq!(ue.status, 502);
                assert_eq!(ue.body, "upstream exploded");
                assert_eq!(ue.retry_after.as_deref(), Some("7"));
            }
            other => panic!("expected Upstream, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn chat_empty_response_when_no_deltas() {
        let server = wiremock::MockServer::start().await;
        let events = sse_stream(&[r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#
            .into()]);
        mount(&server, 200, events).await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        assert!(matches!(e, UpstreamChatError::EmptyResponse));
    }

    #[tokio::test]
    async fn chat_in_band_error_fails_turn() {
        let server = wiremock::MockServer::start().await;
        let events = sse_stream(&[r#"event:error
data:{"type":"error","content":"当前访问较为繁忙，请稍后再试"}"#
            .into()]);
        mount(&server, 200, events).await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        match e {
            UpstreamChatError::InBand(InBandError::Busy { .. }) => {}
            other => panic!("expected InBand Busy, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn chat_eof_before_done_is_stream_error() {
        let server = wiremock::MockServer::start().await;
        let events = sse_stream(&[r#"event:message
data:{"type":"delta","content":"partial"}"#
            .into()]);
        mount(&server, 200, events).await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        assert!(matches!(
            e,
            UpstreamChatError::InBand(InBandError::Stream(_))
        ));
    }

    #[tokio::test]
    async fn chat_silence_window_trips_silent_upstream() {
        // A mock that never answers: the request future is raced against a
        // tiny window → SilentUpstream (no account penalty class).
        let server = wiremock::MockServer::start().await;
        wiremock::Mock::given(wiremock::matchers::method("POST"))
            .and(wiremock::matchers::path("/open-apis/bot/chat"))
            .respond_with(wiremock::ResponseTemplate::new(200).set_delay(Duration::from_secs(30)))
            .mount(&server)
            .await;
        let cc = mock_client(&server.uri(), Duration::from_millis(50)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        assert!(matches!(e, UpstreamChatError::SilentUpstream { .. }));
    }

    #[tokio::test]
    async fn chat_body_stall_trips_idle_stall() {
        // Headers arrive (200 + headers sent) but the body never completes:
        // the body read races the window → IdleStall (transport class).
        let server = wiremock::MockServer::start().await;
        wiremock::Mock::given(wiremock::matchers::method("POST"))
            .and(wiremock::matchers::path("/open-apis/bot/chat"))
            .respond_with(
                wiremock::ResponseTemplate::new(200)
                    .set_delay(Duration::from_secs(30))
                    .set_body_string("data:...\n\n"),
            )
            .mount(&server)
            .await;
        let cc = mock_client(&server.uri(), Duration::from_millis(50)).await;
        let e = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro"))
            .await
            .err()
            .unwrap();
        // Either the whole response (headers+body) was delayed — then it
        // is SilentUpstream — or headers arrived and the body stalled:
        // IdleStall. Both are the no-bytes/stall taxonomy under one
        // wiremock delay; assert the class, not the arm.
        match e {
            UpstreamChatError::SilentUpstream { .. } | UpstreamChatError::IdleStall => {}
            other => panic!("expected watchdog class, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn chat_fastchat_model_routes_to_fastchat_path() {
        let server = wiremock::MockServer::start().await;
        let events = sse_stream(&[
            r#"event:message
data:{"type":"delta","content":"hi"}"#
                .into(),
            r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#
                .into(),
        ]);
        wiremock::Mock::given(wiremock::matchers::method("POST"))
            .and(wiremock::matchers::path("/fastchat/open-apis/bot/chat"))
            .respond_with(
                wiremock::ResponseTemplate::new(200)
                    .insert_header("content-type", "text/event-stream")
                    .set_body_string(events),
            )
            .expect(1..)
            .mount(&server)
            .await;
        let cc = mock_client(&server.uri(), Duration::from_secs(60)).await;
        let r = cc
            .chat("cookie=1", "ph", &req("mimo-v2.6-pro-ultraspeed-studio"))
            .await
            .unwrap();
        assert_eq!(r.content, "hi");
    }
}
