//! LIVE end-to-end, gated behind `TH_LIVE_E2E=1` (task brief: skipped by
//! default, NEVER part of the gates). Requires `TH_ACCOUNT_1_EMAIL` /
//! `TH_ACCOUNT_1_PASSWORD` (TEST accounts only — the recon account is
//! quarantined and must not be used here).
//!
//! One login + one tiny chat + one catalog fetch against the real
//! tokenharbor.ai. Pacing is real (1.5s between calls); the test asserts
//! only wire-level facts, never model availability (the free table
//! churns — model ids come from the live catalog at run time).

use std::sync::Arc;

use serde_json::json;

use oxcost_plugin_tokenharbor::auth::AUTH_BASE;
use oxcost_plugin_tokenharbor::client::Client;
use oxcost_plugin_tokenharbor::store::{AccountRow, Store};
use oxcost_plugin_tokenharbor::{TokenHarbor, PLUGIN_ID};

use oxcost_sdk::OxcostPlugin;

#[tokio::test]
#[ignore = "live E2E is opt-in: TH_LIVE_E2E=1 + TH_ACCOUNT_1_EMAIL/PASSWORD"]
async fn live_login_catalog_and_tiny_chat() {
    let (email, password) = match (
        std::env::var("TH_ACCOUNT_1_EMAIL").ok(),
        std::env::var("TH_ACCOUNT_1_PASSWORD").ok(),
    ) {
        (Some(e), Some(p)) => (e, p),
        _ => panic!("TH_LIVE_E2E set but TH_ACCOUNT_1_EMAIL/PASSWORD missing"),
    };

    let dir = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(&dir.path().join("live-e2e.db")).unwrap());
    store
        .upsert_account(&AccountRow::seed(&email, &password))
        .unwrap();

    let plugin = TokenHarbor::connect(
        Arc::clone(&store),
        Arc::new(Client::new_live()),
        AUTH_BASE.to_string(),
    )
    .await
    .unwrap();

    // Catalog: real ids fetched and persisted.
    let snapshot = store.catalog().unwrap();
    assert!(!snapshot.models.is_empty(), "live catalog has rows");
    assert!(snapshot.server_now.is_some(), "serverNow parsed");

    // Login happened during chat; tokens persisted (rotation survived).
    let models = plugin.models();
    assert!(
        !models.is_empty(),
        "servable view non-empty for a live TEST account: {email}"
    );
    let model = models[0].id.clone();

    let outcome = plugin
        .chat(
            oxcost_sdk::ChatRequest::new(json!({
                "model": model,
                "messages": [{"role": "user", "content": "Reply with exactly: ok"}],
            }))
            .unwrap(),
            Default::default(),
        )
        .await
        .expect("live tiny chat succeeds");
    // Either outcome shape is acceptable; both must carry content.
    match &outcome {
        oxcost_sdk::ChatOutcome::Json(body) => {
            let content = body["choices"][0]["message"]["content"]
                .as_str()
                .unwrap_or_default();
            assert!(!content.is_empty(), "non-stream reply non-empty");
        }
        oxcost_sdk::ChatOutcome::Sse(bytes) => {
            let text = String::from_utf8_lossy(bytes);
            assert!(text.contains("\"content\""), "stream has content frames");
            assert!(text.ends_with("data: [DONE]\n\n"));
        }
    }

    let row = store.accounts().unwrap().pop().unwrap();
    assert!(!row.refresh_token.is_empty(), "refresh token persisted");
    assert_eq!(plugin.manifest().id, PLUGIN_ID);
}
