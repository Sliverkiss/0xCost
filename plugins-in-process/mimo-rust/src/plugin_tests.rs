//! MimoPlugin tests: the OxcostPlugin trait surface over the chat
//! pipeline — manifest shape, models/manifest consistency, chat
//! delivery (JSON and SSE) against a mock upstream, error relay, and
//! health on empty vs seeded pool.

use std::time::Duration;

use oxcost_sdk::types::{ChatOutcome, ChatRequest, HealthStatus, RequestContext};
use oxcost_sdk::OxcostPlugin;
use serde_json::json;

use super::MimoPlugin;
use crate::chat::ChatPipeline;
use crate::login;
use crate::pool::{self, AccountRecord, Pool};
use crate::store::{Store, StoredAccount};

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

/// A plugin whose pipeline points at `base` with one pre-authenticated
/// account (in-memory store). As a child module this can assemble the
/// private fields directly — same pattern as chat_tests.rs.
fn plugin_with_account(base: &str, account_id: &str) -> MimoPlugin {
    let store = Store::open_in_memory().unwrap();
    let acct = StoredAccount {
        id: account_id.into(),
        phone: Some("13800000000".into()),
        password: Some("pw".into()),
        enabled: true,
        egress: pool::DEFAULT_EGRESS.into(),
        cookie: Some("cookie=1".into()),
        ph: Some("ph-1".into()),
        created_at: 0,
        updated_at: 0,
        ..Default::default()
    };
    store.upsert_account(&acct).unwrap();
    let records = vec![AccountRecord {
        id: account_id.into(),
        egress: pool::DEFAULT_EGRESS.into(),
        ..Default::default()
    }];
    let login_client = login::Client::new(login::Options {
        base: Some(base.to_string()),
        idle_timeout: Some(Duration::from_secs(60)),
        ..Default::default()
    })
    .unwrap();
    MimoPlugin {
        manifest: MimoPlugin::build_manifest(),
        pipeline: ChatPipeline {
            pool: Pool::new(&records, pool::Options::default(), None, None),
            store,
            login: login_client,
            thinking_default_on: true,
            default_model: "mimo-v2.6-flash".into(),
        },
        has_accounts: true,
    }
}

fn empty_plugin(base: &str) -> MimoPlugin {
    let login_client = login::Client::new(login::Options {
        base: Some(base.to_string()),
        ..Default::default()
    })
    .unwrap();
    MimoPlugin {
        manifest: MimoPlugin::build_manifest(),
        pipeline: ChatPipeline {
            pool: Pool::new(&[], pool::Options::default(), None, None),
            store: Store::open_in_memory().unwrap(),
            login: login_client,
            thinking_default_on: true,
            default_model: "mimo-v2.6-flash".into(),
        },
        has_accounts: false,
    }
}

async fn mount_chat(server: &wiremock::MockServer, status: u16, body: String) {
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

fn req(model: &str, content: &str, stream: bool) -> ChatRequest {
    ChatRequest::new(json!({
        "model": model,
        "messages": [{"role": "user", "content": content}],
        "stream": stream,
    }))
    .unwrap()
}

#[test]
fn manifest_shape_and_validation() {
    let m = MimoPlugin::build_manifest();
    assert!(m.validate().is_ok(), "manifest must pass contract checks");
    assert_eq!(
        m.models,
        vec![
            "mimo-rust/mimo-v2.6-pro",
            "mimo-rust/mimo-v2.6-flash",
            "mimo-rust/mimo-v2.6-pro-ultraspeed-studio",
        ]
    );
    assert!(m.enabled);
    // The plugin id's last segment is the name (manifest prefix rule).
    assert_eq!(m.name, "mimo-rust");
}

#[tokio::test]
async fn chat_delivers_json_outcome() {
    let server = wiremock::MockServer::start().await;
    mount_chat(&server, 200, sse_stream(HELLO_EVENTS)).await;
    let p = plugin_with_account(&server.uri(), "acct-1");
    let out = p
        .chat(
            req("mimo-v2.6-flash", "hi", false),
            RequestContext::default(),
        )
        .await
        .unwrap();
    match out {
        ChatOutcome::Json(v) => {
            assert_eq!(v["choices"][0]["message"]["content"], "hello");
            assert_eq!(v["model"], "mimo-v2.6-flash");
        }
        ChatOutcome::Sse(_) => panic!("non-stream must deliver JSON"),
    }
}

#[tokio::test]
async fn chat_delivers_sse_outcome() {
    let server = wiremock::MockServer::start().await;
    mount_chat(&server, 200, sse_stream(HELLO_EVENTS)).await;
    let p = plugin_with_account(&server.uri(), "acct-1");
    let out = p
        .chat(
            req("mimo-v2.6-flash", "hi", true),
            RequestContext::default(),
        )
        .await
        .unwrap();
    match out {
        ChatOutcome::Sse(bytes) => {
            let text = String::from_utf8(bytes).unwrap();
            assert!(text.contains("\"role\":\"assistant\""));
            assert!(text.contains("hello"));
            assert!(text.ends_with("data: [DONE]\n\n"));
        }
        ChatOutcome::Json(_) => panic!("stream must deliver SSE"),
    }
}

#[tokio::test]
async fn chat_maps_pipeline_error_to_plugin_error() {
    let server = wiremock::MockServer::start().await;
    mount_chat(&server, 429, "too many".into()).await;
    let p = plugin_with_account(&server.uri(), "acct-1");
    let err = p
        .chat(
            req("mimo-v2.6-flash", "hi", false),
            RequestContext::default(),
        )
        .await
        .err()
        .unwrap();
    assert_eq!(err.status, 429);
    assert_eq!(err.err_type, "rate_limit_error");
    assert_eq!(err.code.as_deref(), Some("upstream_rate_limited"));
}

#[tokio::test]
async fn chat_unknown_model_is_404_without_upstream_call() {
    let server = wiremock::MockServer::start().await;
    let p = plugin_with_account(&server.uri(), "acct-1");
    let err = p
        .chat(req("no-such-model", "hi", false), RequestContext::default())
        .await
        .err()
        .unwrap();
    assert_eq!(err.status, 404);
    assert_eq!(err.code.as_deref(), Some("model_not_found"));
    // The rejection happens before pool acquire: zero upstream calls.
    assert_eq!(server.received_requests().await.unwrap().len(), 0);
}

#[test]
fn models_list_matches_manifest() {
    let p = empty_plugin("http://unused");
    let manifest_models = p.manifest().models.clone();
    let models: Vec<String> = p.models().iter().map(|m| m.id.clone()).collect();
    assert_eq!(models, manifest_models);
    // Descriptors: owned by the plugin's display name.
    assert!(p.models().iter().all(|d| d.owned_by == "mimo-rust"));
}

#[tokio::test]
async fn empty_store_is_unhealthy_but_catalog_still_served() {
    let server = wiremock::MockServer::start().await;
    let p = empty_plugin(&server.uri());
    // Deployment forgot to seed accounts → probe surfaces it.
    assert_eq!(p.health(), HealthStatus::Unhealthy);
    // The model surface is static: still listed for the registry.
    assert_eq!(p.models().len(), 3);
    assert_eq!(p.models()[0].id, "mimo-rust/mimo-v2.6-pro");
}

#[tokio::test]
async fn empty_pool_chat_fails_closed() {
    let server = wiremock::MockServer::start().await;
    let p = empty_plugin(&server.uri());
    let err = p
        .chat(
            req("mimo-v2.6-flash", "hi", false),
            RequestContext::default(),
        )
        .await
        .err()
        .unwrap();
    assert_eq!(err.status, 503);
    assert_eq!(err.code.as_deref(), Some("no_accounts"));
    assert_eq!(server.received_requests().await.unwrap().len(), 0);
}
