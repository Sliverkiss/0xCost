//! Chat-flow unit tests: the prepare/parse boundary and the §6 rows the
//! facade relies on, offline (no server). The end-to-end ladder tests
//! live in `plugin/tests.rs` (mock upstream).

use super::*;
use oxcost_sdk::ChatOutcome;
use serde_json::json;

fn body() -> serde_json::Value {
    json!({
        "model": "deepseek-flash",
        "messages": [{"role": "user", "content": "hi"}],
    })
}

#[test]
fn prepare_parses_flattens_and_caps() {
    let job = prepare(&body(), DEFAULT_MAX_PROMPT_CHARS).unwrap();
    // flatten_messages joins per-message lines (Go parity: trailing \n).
    assert_eq!(job.prompt, "user: hi\n");
    assert!(job.images.is_empty());
}

#[test]
fn prepare_enforces_cap() {
    let we = prepare(&body(), 1).unwrap_err();
    assert_eq!(we.error.status, 400);
    assert_eq!(we.error.code.as_deref(), Some("context_length_exceeded"));
}

#[test]
fn prepare_cap_zero_means_disabled() {
    let big = json!({
        "model": "deepseek-flash",
        "messages": [{"role": "user", "content": "x".repeat(100)}],
    });
    assert!(prepare(&big, 0).is_ok());
}

#[test]
fn prepare_model_row_is_404() {
    let we = prepare(
        &json!({"model": "other", "messages": [{"role":"user","content":"x"}]}),
        0,
    )
    .unwrap_err();
    assert_eq!(we.error.status, 404);
    assert_eq!(we.error.code.as_deref(), Some("model_not_found"));
}

#[test]
fn prepare_bad_switch_is_400_bad_request() {
    let mut b = body();
    b["thinking"] = json!("bogus");
    let we = prepare(&b, 0).unwrap_err();
    assert_eq!(we.error.status, 400);
    assert_eq!(we.error.code.as_deref(), Some("bad_request"));
    assert!(we.error.message.contains("thinking"));
}

#[tokio::test]
async fn resolve_images_bad_url_is_400_image_fetch_failed() {
    let mut job = prepare(&body(), 0).unwrap();
    // Inject an http image URL pointing at a dead port: the fetch fails
    // fast (connection refused) — the row is image_fetch_failed.
    job.parsed.messages[0]
        .parts
        .push(crate::openai::ContentPart {
            kind: crate::openai::PartKind::ImageUrl,
            image_url: "http://127.0.0.1:1/img.png".into(),
            text: String::new(),
        });
    let we = resolve_images(&mut job).await.unwrap_err();
    assert_eq!(we.error.status, 400);
    assert_eq!(we.error.code.as_deref(), Some("image_fetch_failed"));
}

#[tokio::test]
async fn run_chat_empty_pool_is_503_no_accounts() {
    // The empty ring answers 503 no_accounts through the ladder.
    let pool = crate::pool::Pool::new(
        vec![],
        crate::pool::PoolConfig {
            random_seed: 1,
            queue_wait: std::time::Duration::from_millis(50),
            ..Default::default()
        },
    )
    .unwrap();
    let job = prepare(&body(), 0).unwrap();
    let we = run_chat(&pool, job).await.unwrap_err();
    assert_eq!(we.error.status, 503);
    assert_eq!(we.error.code.as_deref(), Some("no_accounts"));
}

#[test]
fn outcome_enum_shape() {
    // Type-level sanity: the Done arm carries both success and error.
    let ok: Result<ChatOutcome, WireError> = Ok(ChatOutcome::Sse(vec![b'x']));
    assert!(matches!(ok.unwrap(), ChatOutcome::Sse(_)));
}
