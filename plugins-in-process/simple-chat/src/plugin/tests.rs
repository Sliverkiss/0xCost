//! Facade + chat-flow tests: the full offline pipeline through
//! `OxcostPlugin::chat` against a raw-TCP mock DeepSeek upstream that
//! speaks login → session → PoW → completion-SSE, plus the §4 boundary
//! rows (parse/cap/model) and the manifest/health surface.
//!
//! The mock server is one `tokio` task per connection: it parses the
//! request line + headers, routes by path, and answers fixed JSON or SSE
//! bytes. `difficulty: 1` PoW challenges make the Keccak solve trivial
//! (nonce 0 hits), so the ladder runs at real speed offline.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

use oxcost_sdk::types::{ChatOutcome, ChatRequest, HealthStatus, RequestContext};
use oxcost_sdk::OxcostPlugin;
use serde_json::json;

use super::*;
use crate::account::Account;
use crate::pow::hash_v1;

// ---- the mock upstream ----

/// What the completion endpoint should answer (per test).
#[derive(Clone)]
enum CompletionScript {
    /// A clean SSE stream with THINK + RESPONSE fragments and a close.
    Sse {
        think: &'static str,
        text: &'static str,
    },
    /// HTTP 200 with a JSON error envelope (pre-stream Biz).
    Biz {
        code: i64,
        biz_code: i64,
        biz_msg: &'static str,
    },
    /// An empty SSE body (triggers the empty-output retry then honest
    /// delivery on the final attempt).
    EmptySse,
}

/// One mock upstream: counts completion calls, answers per the installed
/// script.
struct MockUpstream {
    completion_calls: Arc<AtomicUsize>,
    script: CompletionScript,
}

impl MockUpstream {
    /// Serve on an ephemeral port; returns the base URL.
    async fn start(script: CompletionScript, completion_calls: Arc<AtomicUsize>) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let this = Arc::new(MockUpstream {
            completion_calls,
            script,
        });
        let this = Arc::clone(&this);
        tokio::spawn(async move {
            // One connection per HTTP request is fine for the ladder's
            // serial flow (login, session, pow, completion are
            // sequential; the pool serializes within an account too).
            while let Ok((sock, _)) = listener.accept().await {
                let this = Arc::clone(&this);
                tokio::spawn(async move {
                    this.serve_connection(sock).await;
                });
            }
        });
        format!("http://127.0.0.1:{port}")
    }

    async fn serve_connection(&self, mut sock: tokio::net::TcpStream) {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let mut buf = Vec::new();
        let mut tmp = [0u8; 4096];
        // Read until end of headers plus the content-length body.
        loop {
            let n = sock.read(&mut tmp).await.unwrap_or(0);
            if n == 0 {
                return;
            }
            buf.extend_from_slice(&tmp[..n]);
            if let Some(head_end) = find_header_end(&buf) {
                let body_len = content_length(&buf[..head_end]).unwrap_or(0);
                if buf.len() >= head_end + body_len {
                    break;
                }
            }
        }
        let head_end = find_header_end(&buf).unwrap_or(buf.len());
        let head = String::from_utf8_lossy(&buf[..head_end]).into_owned();
        let path = head.split_whitespace().nth(1).unwrap_or("/").to_string();
        let resp = self.route(&path);
        let _ = sock.write_all(&resp).await;
        let _ = sock.shutdown().await;
    }

    fn route(&self, path: &str) -> Vec<u8> {
        match path {
            "/api/v0/users/login" => json_response(&json!({
                "code": 0,
                "data": {"biz_code": 0, "biz_data": {"user": {"token": "tok-1"}}}
            })),
            "/api/v0/chat_session/create" => json_response(&json!({
                "code": 0,
                "data": {"biz_code": 0, "biz_data": {"chat_session": {"id": "sess-1"}}}
            })),
            "/api/v0/chat/create_pow_challenge" => {
                // A challenge solvable at nonce 0.
                let salt = "s0";
                let expire_at = 4_102_444_800i64; // far future
                let preimage = format!("{salt}_{expire_at}_0");
                let digest = hex::encode(hash_v1(preimage.as_bytes()));
                json_response(&json!({
                    "code": 0,
                    "data": {"biz_code": 0, "biz_data": {"challenge": {
                        "algorithm": "HashV1",
                        "challenge": digest,
                        "salt": salt,
                        "expire_at": expire_at,
                        "difficulty": 1,
                        "expire_after": 60,
                        "signature": "sig",
                        "target_path": "/api/v0/chat/completion"
                    }}}
                }))
            }
            "/api/v0/chat/completion" => {
                self.completion_calls.fetch_add(1, Ordering::SeqCst);
                match &self.script {
                    CompletionScript::Sse { think, text } => sse_response(think, text),
                    CompletionScript::Biz {
                        code,
                        biz_code,
                        biz_msg,
                    } => json_response(
                        &json!({"code": code, "msg": "", "data": {"biz_code": biz_code, "biz_msg": biz_msg}}),
                    ),
                    CompletionScript::EmptySse => sse_raw(b""),
                }
            }
            _ => json_response(&json!({"code": 0, "data": {"biz_code": 0}})),
        }
    }
}

fn find_header_end(buf: &[u8]) -> Option<usize> {
    buf.windows(4).position(|w| w == b"\r\n\r\n").map(|p| p + 4)
}

fn content_length(head: &[u8]) -> Option<usize> {
    let s = String::from_utf8_lossy(head);
    for line in s.lines() {
        let l = line.to_ascii_lowercase();
        if let Some(v) = l.strip_prefix("content-length:") {
            return v.trim().parse().ok();
        }
    }
    None
}

fn json_response(v: &serde_json::Value) -> Vec<u8> {
    let body = v.to_string();
    format!(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    )
    .into_bytes()
}

/// The Go-style completion SSE: initial response object with THINK +
/// RESPONSE fragments, then a close event. Fixed content-length framing
/// suffices for reqwest.
fn sse_response(think: &str, text: &str) -> Vec<u8> {
    let body = format!(
        "data: {}\n\nevent: close\ndata: {{\"k\":\"\"}}\n\n",
        json!({"v": {"response": {"fragments": [
            {"id": 1, "type": "THINK", "content": think},
            {"id": 2, "type": "RESPONSE", "content": text}
        ]}}})
    );
    sse_raw(body.as_bytes())
}

fn sse_raw(body: &[u8]) -> Vec<u8> {
    let head = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    let mut out = head.into_bytes();
    out.extend_from_slice(body);
    out
}

// ---- plugin construction helpers ----

fn account(mobile: &str) -> Account {
    Account {
        mobile: mobile.into(),
        password: "p".into(),
        ..Default::default()
    }
}

fn plugin_test_cfg() -> SimpleChatConfig {
    SimpleChatConfig {
        accounts: vec![account("13800000001")],
        base_url: "http://127.0.0.1:1".into(),
        random_seed: 7,
        ..Default::default()
    }
}

fn plugin_with(base_url: &str) -> SimpleChatPlugin {
    SimpleChatPlugin::new(SimpleChatConfig {
        base_url: base_url.into(),
        ..plugin_test_cfg()
    })
    .unwrap()
}

fn chat_req(body: serde_json::Value) -> ChatRequest {
    ChatRequest::new(body).unwrap()
}

fn base_body(stream: bool) -> serde_json::Value {
    json!({
        "model": "deepseek-flash",
        "messages": [{"role": "user", "content": "hello"}],
        "stream": stream,
    })
}

// ---- §4 boundary rows (parse/cap/model) + health ----

#[tokio::test]
async fn chat_rejects_wrong_model_with_404() {
    let p = plugin_with("http://127.0.0.1:1");
    let err = p
        .chat(
            chat_req(json!({"model": "gpt-4", "messages": [{"role":"user","content":"x"}]})),
            RequestContext::default(),
        )
        .await
        .unwrap_err();
    assert_eq!(err.status, 404);
    assert_eq!(err.code.as_deref(), Some("model_not_found"));
}

#[tokio::test]
async fn chat_rejects_over_cap_prompt_with_400() {
    let p = SimpleChatPlugin::new(SimpleChatConfig {
        max_prompt_chars: 10,
        ..plugin_test_cfg()
    })
    .unwrap();
    let mut body = base_body(false);
    body["messages"][0]["content"] = json!("this is much longer than ten characters");
    let err = p
        .chat(chat_req(body), RequestContext::default())
        .await
        .unwrap_err();
    assert_eq!(err.status, 400);
    assert_eq!(err.code.as_deref(), Some("context_length_exceeded"));
}

#[tokio::test]
async fn empty_ring_health_unhealthy_and_chat_503() {
    let p = SimpleChatPlugin::new(SimpleChatConfig::default()).unwrap();
    assert_eq!(p.health(), HealthStatus::Unhealthy);
    let err = p
        .chat(chat_req(base_body(false)), RequestContext::default())
        .await
        .unwrap_err();
    assert_eq!(err.status, 503);
    assert_eq!(err.code.as_deref(), Some("no_accounts"));
}

#[test]
fn manifest_and_models_pin_the_contract() {
    let p = SimpleChatPlugin::new(plugin_test_cfg()).unwrap();
    assert_eq!(p.manifest().id, "dev.oxcost.simple-chat");
    assert_eq!(p.manifest().name, "simple-chat");
    assert!(
        p.manifest().validate().is_ok(),
        "manifest must pass contract validation"
    );
    let models = p.models();
    assert_eq!(models.len(), 1);
    assert_eq!(models[0].id, "simple-chat/deepseek-flash");
    assert_eq!(models[0].owned_by, "simple-chat");
}

// ---- end-to-end through the mock upstream ----

#[tokio::test]
async fn non_stream_end_to_end_with_reasoning() {
    let calls = Arc::new(AtomicUsize::new(0));
    let base = MockUpstream::start(
        CompletionScript::Sse {
            think: "because",
            text: "42",
        },
        Arc::clone(&calls),
    )
    .await;
    let p = plugin_with(&base);
    assert_eq!(p.health(), HealthStatus::Ok);
    let out = p
        .chat(chat_req(base_body(false)), RequestContext::default())
        .await
        .unwrap();
    match out {
        ChatOutcome::Json(v) => {
            assert_eq!(v["object"], "chat.completion");
            assert_eq!(v["choices"][0]["message"]["content"], "42");
            // Default-on reasoning (no opt-out switch in the request).
            assert_eq!(v["choices"][0]["message"]["reasoning_content"], "because");
            assert_eq!(v["choices"][0]["finish_reason"], "stop");
        }
        other => panic!("expected JSON outcome, got {other:?}"),
    }
    assert_eq!(calls.load(Ordering::SeqCst), 1, "one completion call");
}

#[tokio::test]
async fn non_stream_thinking_opt_out_suppresses_reasoning() {
    let calls = Arc::new(AtomicUsize::new(0));
    let base = MockUpstream::start(
        CompletionScript::Sse {
            think: "secret thoughts",
            text: "ok",
        },
        Arc::clone(&calls),
    )
    .await;
    let p = plugin_with(&base);
    let mut body = base_body(false);
    body["thinking"] = json!({"type": "disabled"});
    let out = p
        .chat(chat_req(body), RequestContext::default())
        .await
        .unwrap();
    let ChatOutcome::Json(v) = out else {
        panic!("expected JSON")
    };
    assert_eq!(v["choices"][0]["message"]["content"], "ok");
    assert!(!v.to_string().contains("reasoning_content"));
}

#[tokio::test]
async fn stream_end_to_end_frames_and_done() {
    let calls = Arc::new(AtomicUsize::new(0));
    let base = MockUpstream::start(
        CompletionScript::Sse {
            think: "step",
            text: "answer",
        },
        Arc::clone(&calls),
    )
    .await;
    let p = plugin_with(&base);
    let out = p
        .chat(chat_req(base_body(true)), RequestContext::default())
        .await
        .unwrap();
    let ChatOutcome::Sse(bytes) = out else {
        panic!("expected SSE")
    };
    let s = String::from_utf8(bytes).unwrap();
    assert!(s.starts_with("data: "), "first frame is a chunk");
    assert!(s.ends_with("data: [DONE]\n\n"));
    assert!(
        s.contains(r#""reasoning_content":"step""#),
        "THINK delta emitted: {s}"
    );
    assert!(s.contains(r#""content":"answer""#));
    // role only on the first chunk.
    assert_eq!(s.matches(r#""role":"assistant""#).count(), 1);
    // final chunk carries finish_reason stop + usage.
    assert!(s.contains(r#""finish_reason":"stop""#));
    assert!(s.contains(r#""total_tokens""#));
}

#[tokio::test]
async fn pre_stream_biz_error_maps_to_502_without_retry() {
    // A Biz answer is terminal (IsRetryable = false): one completion
    // call, generic 502 upstream_failure, no biz_msg leak.
    let calls = Arc::new(AtomicUsize::new(0));
    let base = MockUpstream::start(
        CompletionScript::Biz {
            code: 0,
            biz_code: 475,
            biz_msg: "internal detail",
        },
        Arc::clone(&calls),
    )
    .await;
    let p = plugin_with(&base);
    let err = p
        .chat(chat_req(base_body(false)), RequestContext::default())
        .await
        .unwrap_err();
    assert_eq!(err.status, 502);
    assert_eq!(err.code.as_deref(), Some("upstream_failure"));
    assert!(
        !err.message.contains("internal detail"),
        "biz_msg must not leak"
    );
    assert_eq!(calls.load(Ordering::SeqCst), 1, "Biz never retries");
}

#[tokio::test]
async fn empty_stream_retries_once_then_answers_empty_honestly() {
    // Empty-but-clean: attempt 1 retries (R1); attempt 2 delivers the
    // honest empty completion (Go: empty text, stop).
    let calls = Arc::new(AtomicUsize::new(0));
    let base = MockUpstream::start(CompletionScript::EmptySse, Arc::clone(&calls)).await;
    let p = plugin_with(&base);
    let out = p
        .chat(chat_req(base_body(false)), RequestContext::default())
        .await
        .unwrap();
    let ChatOutcome::Json(v) = out else {
        panic!("expected JSON")
    };
    assert_eq!(v["choices"][0]["message"]["content"], "");
    assert_eq!(v["choices"][0]["finish_reason"], "stop");
    assert_eq!(calls.load(Ordering::SeqCst), 2, "exactly one retry");
}

#[tokio::test]
async fn search_enabled_empty_citations_omitted() {
    // The mock's SSE has no SEARCH fragment, so citations are empty —
    // pinning the empty-citations-omitted shape; the structured
    // citations passthrough itself is covered by sse.rs fixtures.
    let calls = Arc::new(AtomicUsize::new(0));
    let base = MockUpstream::start(
        CompletionScript::Sse {
            think: "",
            text: "cited answer",
        },
        Arc::clone(&calls),
    )
    .await;
    let p = plugin_with(&base);
    let mut body = base_body(false);
    body["search"] = json!({"type": "enabled"});
    let out = p
        .chat(chat_req(body), RequestContext::default())
        .await
        .unwrap();
    let ChatOutcome::Json(v) = out else {
        panic!("expected JSON")
    };
    assert!(!v.to_string().contains("citations"));
}
