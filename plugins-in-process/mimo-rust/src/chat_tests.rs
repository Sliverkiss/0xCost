//! chat pipeline tests: the retry ladder end-to-end against a wiremock
//! upstream, with an in-memory store and a real pool — one test per §7.1
//! classification row, plus the delivery shapes (SSE sequence / completion
//! JSON) and the creds resolution order.

use std::time::Duration;

use serde_json::json;

use crate::chat::{ChatAnswer, ChatPipeline};
use crate::pool::{self, AccountRecord, Pool};
use crate::store::{Store, StoredAccount};

/// SSE frame helpers (same dialect the upstream tests use).
fn sse_stream(events: &[&str]) -> String {
    let mut out = String::new();
    for ev in events {
        out.push_str(ev);
        out.push_str("\n\n");
    }
    out
}

const HELLO_EVENTS: &[&str] = &[
    r#"event:message
data:{"type":"delta","content":"hello"}"#,
    r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#,
];

const BUSY_EVENTS: &[&str] = &[r#"event:error
data:{"type":"error","content":"服务器繁忙，请稍后再试"}"#];

const FILTER_EVENTS: &[&str] = &[r#"event:sensitive_query
data:{"content":"bad topic"}"#];

/// One account in store + pool, session pre-stored (no login traffic).
fn one_account_pipeline(base: &str) -> ChatPipeline {
    let store = Store::open_in_memory().unwrap();
    let now = 0;
    let mut acct = StoredAccount {
        id: "acct-1".into(),
        phone: Some("13800000000".into()),
        password: Some("pw".into()),
        enabled: true,
        egress: pool::DEFAULT_EGRESS.into(),
        created_at: now,
        updated_at: now,
        ..Default::default()
    };
    acct.cookie = Some("cookie=1".into());
    acct.ph = Some("ph-1".into());
    store.upsert_account(&acct).unwrap();
    let records = vec![AccountRecord {
        id: "acct-1".into(),
        egress: pool::DEFAULT_EGRESS.into(),
        ..Default::default()
    }];
    let login = crate::login::Client::new(crate::login::Options {
        base: Some(base.to_string()),
        // Watchdogs generous: the mock answers instantly, and the
        // busy-backoff test must not trip one.
        idle_timeout: Some(Duration::from_secs(60)),
        ..Default::default()
    })
    .unwrap();
    ChatPipeline {
        pool: Pool::new(&records, pool::Options::default(), None, None),
        store,
        login,
        thinking_default_on: true,
        default_model: "mimo-v2.6-flash".into(),
    }
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

fn body(query: &str) -> serde_json::Value {
    json!({"model": "mimo-v2.6-flash", "messages": [{"role": "user", "content": query}]})
}

// --- classification rows (§7.1), one test each --------------------------

#[tokio::test]
async fn happy_path_delivers_completion_and_reports_success() {
    let server = wiremock::MockServer::start().await;
    mount(&server, 200, sse_stream(HELLO_EVENTS)).await;
    let p = one_account_pipeline(&server.uri());
    let out = p.handle(&body("hi")).await.unwrap();
    match out {
        ChatAnswer::Completion(v) => {
            assert_eq!(v["choices"][0]["message"]["content"], "hello");
            assert_eq!(v["choices"][0]["finish_reason"], "stop");
            assert_eq!(v["model"], "mimo-v2.6-flash");
            assert!(v["id"].as_str().unwrap().starts_with("chatcmpl-"));
        }
        ChatAnswer::Sse(_) => panic!("non-stream request must deliver JSON"),
    }
    // Account healthy: no failures, no cooldown.
    assert!(p.pool.failures_of("acct-1").values().all(|&n| n == 0));
    assert_eq!(p.pool.cooldown_until_of("acct-1"), 0);
}

#[tokio::test]
async fn stream_request_delivers_full_sse_sequence() {
    let server = wiremock::MockServer::start().await;
    let events = sse_stream(&[
        r#"event:message
data:{"type":"delta","content":"answer"}"#,
        r#"event:usage
data:{"promptTokens":5,"completionTokens":7,"totalTokens":12}"#,
        r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#,
    ]);
    mount(&server, 200, events).await;
    let p = one_account_pipeline(&server.uri());
    let out = p
        .handle(&json!({
            "model": "mimo-v2.6-flash", "stream": true,
            "messages": [{"role": "user", "content": "hi"}]
        }))
        .await
        .unwrap();
    let ChatAnswer::Sse(bytes) = out else {
        panic!("stream request must deliver SSE");
    };
    let text = String::from_utf8(bytes).unwrap();
    // Frame order: role → text → finish → usage → [DONE].
    let role = text.find("delta\":{\"role\"").unwrap();
    let text_at = text.find("\"content\":\"answer\"").unwrap();
    let finish = text.find("\"finish_reason\":\"stop\"").unwrap();
    let usage = text.find("\"total_tokens\":12").unwrap();
    let done = text.find("data: [DONE]").unwrap();
    assert!(role < text_at && text_at < finish && finish < usage && usage < done);
    assert!(text.ends_with("data: [DONE]\n\n"));
}

#[tokio::test]
async fn risk_control_451_disables_and_fails_closed() {
    let server = wiremock::MockServer::start().await;
    mount(&server, 451, "risky".into()).await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    // 451 is upstream-shaped: the mapper's 5xx/other arm.
    assert_eq!(err.status, 502);
    assert_eq!(err.body.error.code.as_deref(), Some("upstream_failure"));
    // Disabled, NOT permanent (the signal's form is unverified, REQ-10);
    // the risk event was recorded.
    assert_eq!(
        p.pool.state_of("acct-1"),
        crate::pool::State::Disabled,
        "451 must disable"
    );
    assert!(!p.pool.permanent_of("acct-1"));
    assert_eq!(p.pool.risk_events().len(), 1);
    let ev = &p.pool.risk_events()[0];
    assert_eq!(ev.status, 451);
    assert_eq!(ev.model.as_deref(), Some("mimo-v2.6-flash"));
    // The second request fails at the pool (no accounts) — no retry burn.
    let err2 = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err2.status, 503);
    assert_eq!(err2.body.error.code.as_deref(), Some("no_accounts"));
}

#[tokio::test]
async fn upstream_429_cooldowns_and_fails_without_retry() {
    let server = wiremock::MockServer::start().await;
    // Up to 2 calls (original + 1 retry) — the ladder must make only one.
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(wiremock::ResponseTemplate::new(429).set_body_string("too many"))
        .up_to_n_times(2)
        .mount(&server)
        .await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 429);
    assert_eq!(
        err.body.error.code.as_deref(),
        Some("upstream_rate_limited")
    );
    // Exactly one upstream call: no retry for a 429 (SDD §7.1).
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
    assert!(p.pool.cooldown_until_of("acct-1") > 0);
}

#[tokio::test]
async fn model_rejected_fails_without_retry_or_pool_action() {
    let server = wiremock::MockServer::start().await;
    let events = sse_stream(&[r#"event:error
data:{"type":"error","content":"模型名称错误"}"#]);
    mount(&server, 200, events).await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 400);
    assert_eq!(
        err.body.error.code.as_deref(),
        Some("model_not_available_on_channel")
    );
    // Deterministic refusal: exactly one upstream call, no pool action.
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
    assert_eq!(p.pool.state_of("acct-1"), crate::pool::State::Available);
}

#[tokio::test]
async fn content_filter_fails_without_retry_or_pool_action() {
    let server = wiremock::MockServer::start().await;
    mount(&server, 200, sse_stream(FILTER_EVENTS)).await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 400);
    assert_eq!(err.body.error.code.as_deref(), Some("content_filter"));
    // Content-level: no cooldown (SDD §7.1 — not the account's fault).
    assert_eq!(p.pool.cooldown_until_of("acct-1"), 0);
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
}

#[tokio::test]
async fn busy_retries_once_after_backoff() {
    let server = wiremock::MockServer::start().await;
    // First call: busy frame. Second call: the answer.
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(
            wiremock::ResponseTemplate::new(200)
                .insert_header("content-type", "text/event-stream")
                .set_body_string(sse_stream(BUSY_EVENTS)),
        )
        .up_to_n_times(1)
        .mount(&server)
        .await;
    mount(&server, 200, sse_stream(HELLO_EVENTS)).await;
    let p = one_account_pipeline(&server.uri());
    let out = p.handle(&body("hi")).await.unwrap();
    match out {
        ChatAnswer::Completion(v) => assert_eq!(v["choices"][0]["message"]["content"], "hello"),
        ChatAnswer::Sse(_) => panic!("expected JSON"),
    }
    // Exactly two upstream calls: original + the one backoff retry.
    assert_eq!(server.received_requests().await.unwrap().len(), 2);
    // Busy is platform load, not a dead account.
    assert_eq!(p.pool.cooldown_until_of("acct-1"), 0);
}

#[tokio::test]
async fn busy_twice_fails_with_upstream_busy() {
    let server = wiremock::MockServer::start().await;
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(
            wiremock::ResponseTemplate::new(200)
                .insert_header("content-type", "text/event-stream")
                .set_body_string(sse_stream(BUSY_EVENTS)),
        )
        .mount(&server)
        .await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 503);
    assert_eq!(err.body.error.code.as_deref(), Some("upstream_busy"));
    assert_eq!(
        err.body.error.message,
        "upstream is busy; retry shortly: 服务器繁忙，请稍后再试"
    );
    assert_eq!(server.received_requests().await.unwrap().len(), 2);
}

#[tokio::test]
async fn empty_response_retries_once_with_fresh_ids() {
    let server = wiremock::MockServer::start().await;
    // A finish event with no deltas: empty response (TDD EMPTY-001). Then
    // the real answer on the retry.
    let empty = sse_stream(&[r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#]);
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(
            wiremock::ResponseTemplate::new(200)
                .insert_header("content-type", "text/event-stream")
                .set_body_string(empty),
        )
        .up_to_n_times(1)
        .mount(&server)
        .await;
    mount(&server, 200, sse_stream(HELLO_EVENTS)).await;
    let p = one_account_pipeline(&server.uri());
    let out = p.handle(&body("hi")).await.unwrap();
    match out {
        ChatAnswer::Completion(v) => assert_eq!(v["choices"][0]["message"]["content"], "hello"),
        ChatAnswer::Sse(_) => panic!("expected JSON"),
    }
    assert_eq!(server.received_requests().await.unwrap().len(), 2);
}

#[tokio::test]
async fn upstream_500_cools_account_and_fails_over_exhausted() {
    let server = wiremock::MockServer::start().await;
    // One account: the sick attempt cools it; the retry's acquire —
    // with it excluded — finds no enabled account → 503 no_accounts
    // (Go parity: excluded ids are skipped before the enabled count).
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(wiremock::ResponseTemplate::new(500).set_body_string("boom"))
        .mount(&server)
        .await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 503);
    assert_eq!(err.body.error.code.as_deref(), Some("no_accounts"));
    // Exactly one upstream call — the retry never reached the wire.
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
    // The account cooled under the 5xx ladder with status recorded.
    assert!(p.pool.cooldown_until_of("acct-1") > 0);
    let (class, status, _) = p.pool.cooldown_info_of("acct-1").unwrap();
    assert_eq!(class, crate::pool::FailureClass::Upstream5xx);
    assert_eq!(status, 500);
}

#[tokio::test]
async fn transport_failure_cools_account_then_retries_exhausted() {
    // Connect-refused base: a transport-shaped failure (no HTTP
    // response) — Sick class, transport ladder. The retry finds the
    // account excluded → 503 no_accounts (Go parity).
    let p = one_account_pipeline("http://127.0.0.1:1");
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 503);
    assert_eq!(err.body.error.code.as_deref(), Some("no_accounts"));
    // Transport class cooldown recorded, no disable.
    assert!(p.pool.cooldown_until_of("acct-1") > 0);
    assert_eq!(p.pool.state_of("acct-1"), crate::pool::State::Cooldown);
}

#[tokio::test]
async fn auth_401_surviving_refresh_fails_401() {
    let server = wiremock::MockServer::start().await;
    // Upstream 401; the account has no stored passToken → refresh fails
    // → disable → the client sees the 401.
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(wiremock::ResponseTemplate::new(401).set_body_string("expired"))
        .mount(&server)
        .await;
    let p = one_account_pipeline(&server.uri());
    let err = p.handle(&body("hi")).await.err().unwrap();
    assert_eq!(err.status, 401);
    assert_eq!(
        err.body.error.code.as_deref(),
        Some("upstream_auth_expired")
    );
    // The refresh failure disabled the account (no passToken to renew).
    assert_eq!(p.pool.state_of("acct-1"), crate::pool::State::Disabled);
    assert_eq!(server.received_requests().await.unwrap().len(), 1);
}

// --- pre-pool validation (fails before any upstream call) ---------------

#[tokio::test]
async fn malformed_body_is_400_without_pool_interaction() {
    let server = wiremock::MockServer::start().await;
    let p = one_account_pipeline(&server.uri());
    let err = p
        .handle(&json!({"model": "mimo-v2.6-flash"}))
        .await
        .err()
        .unwrap();
    assert_eq!(err.status, 400);
    assert_eq!(
        err.body.error.code.as_deref(),
        Some("malformed_request_body")
    );
    assert_eq!(server.received_requests().await.unwrap().len(), 0);
}

#[tokio::test]
async fn unknown_model_is_404_without_upstream_call() {
    let server = wiremock::MockServer::start().await;
    let p = one_account_pipeline(&server.uri());
    let err = p
        .handle(&json!({
            "model": "no-such-model",
            "messages": [{"role": "user", "content": "hi"}]
        }))
        .await
        .err()
        .unwrap();
    assert_eq!(err.status, 404);
    assert_eq!(err.body.error.code.as_deref(), Some("model_not_found"));
    assert_eq!(server.received_requests().await.unwrap().len(), 0);
}

#[tokio::test]
async fn query_too_long_is_400_without_upstream_call() {
    let server = wiremock::MockServer::start().await;
    let p = one_account_pipeline(&server.uri());
    let long = "x".repeat(102_401);
    let err = p.handle(&body(&long)).await.err().unwrap();
    assert_eq!(err.status, 400);
    assert_eq!(err.body.error.code.as_deref(), Some("context_too_long"));
    assert_eq!(server.received_requests().await.unwrap().len(), 0);
}

#[tokio::test]
async fn no_user_message_is_400_without_upstream_call() {
    let server = wiremock::MockServer::start().await;
    let p = one_account_pipeline(&server.uri());
    let err = p
        .handle(&json!({
            "model": "mimo-v2.6-flash",
            "messages": [{"role": "system", "content": "sys"}]
        }))
        .await
        .err()
        .unwrap();
    assert_eq!(err.status, 400);
    assert_eq!(err.body.error.code.as_deref(), Some("no_user_message"));
    assert_eq!(server.received_requests().await.unwrap().len(), 0);
}

// --- multi-turn replay + identity ----------------------------------------

#[tokio::test]
async fn multi_turn_replays_then_delivers_only_final_answer() {
    let server = wiremock::MockServer::start().await;
    // Two user turns → two upstream calls in one conversation; only the
    // second turn's answer is delivered.
    let first = sse_stream(&[
        r#"event:dialogId
data:{"type":"id","content":"dlg-1"}"#,
        r#"event:message
data:{"type":"delta","content":"first"}"#,
        r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#,
    ]);
    let second = sse_stream(&[
        r#"event:message
data:{"type":"delta","content":"second"}"#,
        r#"event:usage
data:{"promptTokens":3,"completionTokens":4,"totalTokens":7}"#,
        r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#,
    ]);
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .and(wiremock::matchers::body_string_contains("turn one"))
        .respond_with(
            wiremock::ResponseTemplate::new(200)
                .insert_header("content-type", "text/event-stream")
                .set_body_string(first),
        )
        .mount(&server)
        .await;
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .and(wiremock::matchers::body_string_contains("turn two"))
        .respond_with(
            wiremock::ResponseTemplate::new(200)
                .insert_header("content-type", "text/event-stream")
                .set_body_string(second),
        )
        .mount(&server)
        .await;
    let p = one_account_pipeline(&server.uri());
    let out = p
        .handle(&json!({
            "model": "mimo-v2.6-flash",
            "messages": [
                {"role": "user", "content": "turn one"},
                {"role": "assistant", "content": "first"},
                {"role": "user", "content": "turn two"}
            ]
        }))
        .await
        .unwrap();
    match out {
        ChatAnswer::Completion(v) => {
            assert_eq!(v["choices"][0]["message"]["content"], "second");
            assert_eq!(v["usage"]["total_tokens"], 7);
        }
        ChatAnswer::Sse(_) => panic!("expected JSON"),
    }
    // Both upstream turns ran, and the second chained the first's
    // dialogId (SDD §5.4 replay).
    let reqs = server.received_requests().await.unwrap();
    assert_eq!(reqs.len(), 2);
    let body2 = String::from_utf8(reqs[1].body.clone()).unwrap();
    assert!(body2.contains("dlg-1"), "second turn must chain dialogId");
}

#[tokio::test]
async fn thinking_off_strips_reasoning_from_delivery() {
    let server = wiremock::MockServer::start().await;
    let events = sse_stream(&[
        r#"event:message
data:{"type":"delta","content":"<think>internal reasoning</think>answer"}"#,
        r#"event:finish
data:{"type":"finish","content":"[DONE]"}"#,
    ]);
    mount(&server, 200, events).await;
    let p = one_account_pipeline(&server.uri());
    let out = p
        .handle(&json!({
            "model": "mimo-v2.6-flash",
            "reasoning_effort": "none",
            "messages": [{"role": "user", "content": "hi"}]
        }))
        .await
        .unwrap();
    match out {
        ChatAnswer::Completion(v) => {
            assert_eq!(v["choices"][0]["message"]["content"], "answer");
            // API-010: thinking off → no reasoning_content key at all.
            assert!(v["choices"][0]["message"]
                .get("reasoning_content")
                .is_none());
        }
        ChatAnswer::Sse(_) => panic!("expected JSON"),
    }
}

#[tokio::test]
async fn echo_model_id_is_what_the_client_named() {
    let server = wiremock::MockServer::start().await;
    mount(&server, 200, sse_stream(HELLO_EVENTS)).await;
    let p = one_account_pipeline(&server.uri());
    let out = p
        .handle(&json!({
            "model": "mimo-v2.6-flash-studio",
            "messages": [{"role": "user", "content": "hi"}]
        }))
        .await
        .unwrap();
    match out {
        ChatAnswer::Completion(v) => {
            // The -studio alias resolves upstream but echoes as named.
            assert_eq!(v["model"], "mimo-v2.6-flash-studio");
        }
        ChatAnswer::Sse(_) => panic!("expected JSON"),
    }
}

#[tokio::test]
async fn two_accounts_fail_over_on_upstream_failure() {
    let server = wiremock::MockServer::start().await;
    // First call: 503 (upstream sick). Then the answer — the ladder's
    // retry must exclude the sick account and land on the other.
    wiremock::Mock::given(wiremock::matchers::method("POST"))
        .and(wiremock::matchers::path("/open-apis/bot/chat"))
        .respond_with(wiremock::ResponseTemplate::new(503).set_body_string("sick"))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    mount(&server, 200, sse_stream(HELLO_EVENTS)).await;

    let store = Store::open_in_memory().unwrap();
    let mut records = Vec::new();
    for i in 1..=2 {
        let mut a = StoredAccount {
            id: format!("acct-{i}"),
            phone: Some("13800000000".into()),
            password: Some("pw".into()),
            enabled: true,
            egress: pool::DEFAULT_EGRESS.into(),
            created_at: 0,
            updated_at: 0,
            ..Default::default()
        };
        a.cookie = Some(format!("cookie={i}"));
        a.ph = Some(format!("ph-{i}"));
        store.upsert_account(&a).unwrap();
        records.push(AccountRecord {
            id: format!("acct-{i}"),
            egress: pool::DEFAULT_EGRESS.into(),
            ..Default::default()
        });
    }
    let login = crate::login::Client::new(crate::login::Options {
        base: Some(server.uri()),
        idle_timeout: Some(Duration::from_secs(60)),
        ..Default::default()
    })
    .unwrap();
    let p = ChatPipeline {
        pool: Pool::new(&records, pool::Options::default(), None, None),
        store,
        login,
        thinking_default_on: true,
        default_model: "mimo-v2.6-flash".into(),
    };
    let out = p.handle(&body("hi")).await.unwrap();
    match out {
        ChatAnswer::Completion(v) => assert_eq!(v["choices"][0]["message"]["content"], "hello"),
        ChatAnswer::Sse(_) => panic!("expected JSON"),
    }
    // Exactly two upstream calls: the sick one + the fail-over.
    assert_eq!(server.received_requests().await.unwrap().len(), 2);
    // The sick account cooled (which one got the 503 is selection's
    // choice); the other stayed healthy.
    assert!(p.pool.cooldown_until_of("acct-1") > 0 || p.pool.cooldown_until_of("acct-2") > 0);
}

#[tokio::test]
async fn creds_without_session_or_login_fails_as_transport() {
    let server = wiremock::MockServer::start().await;
    mount(&server, 200, sse_stream(HELLO_EVENTS)).await;
    // A stored account with no session and no phone/password.
    let store = Store::open_in_memory().unwrap();
    let records = vec![AccountRecord {
        id: "acct-bare".into(),
        egress: pool::DEFAULT_EGRESS.into(),
        ..Default::default()
    }];
    let login = crate::login::Client::new(crate::login::Options {
        base: Some(server.uri()),
        ..Default::default()
    })
    .unwrap();
    let p = ChatPipeline {
        pool: Pool::new(&records, pool::Options::default(), None, None),
        store,
        login,
        thinking_default_on: true,
        default_model: "mimo-v2.6-flash".into(),
    };
    let err = p.handle(&body("hi")).await.err().unwrap();
    // The creds failure is transport-shaped (Sick): attempt 1 reports it
    // under the transport ladder, and attempt 2's acquire finds the
    // account cooling and excluded → 503 no_accounts (Go parity: the
    // retry never re-burns a creds failure on the same account).
    assert_eq!(err.status, 503);
    assert_eq!(err.body.error.code.as_deref(), Some("no_accounts"));
    assert!(p.pool.cooldown_until_of("acct-bare") > 0);
}
