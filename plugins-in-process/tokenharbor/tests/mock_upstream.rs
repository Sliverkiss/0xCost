//! Offline integration suite: a local axum mock of tokenharbor.ai +
//! its Supabase auth host, driven through the REAL plugin code paths
//! (client → auth → chat → mapping). No network; runs in every gate.
//!
//! The mock reproduces the live-verified wire facts (dossier
//! PROTOCOL.md/VERIFICATION.md):
//! - password grant + rotating refresh grant (old token single-use);
//! - cookie-only app auth (401 `{"ok":false}` without the valid
//!   `sb-auth-auth-token.0` value);
//! - privacy gate default-off, POST flips it;
//! - sessions 201 `{session:{id,...}}`;
//! - stream errors delivered as SSE frames with non-200 status
//!   (`account_suspended` 403, `session_unknown` 404,
//!   `model_unavailable` 403, `missing_session` 400);
//! - happy-path SSE: thinking/chunk/done (fixture-shaped).

use std::sync::Arc;
use std::time::Duration;

use axum::extract::{RawQuery, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine as _;
use serde_json::{json, Value};
use tokio::sync::Mutex;

use oxcost_plugin_tokenharbor::auth::Auth;
use oxcost_plugin_tokenharbor::client::Client;
use oxcost_plugin_tokenharbor::store::{AccountRow, FreeTier, Store};
use oxcost_plugin_tokenharbor::TokenHarbor;

use oxcost_sdk::{ChatOutcome, ChatRequest, HealthStatus, OxcostPlugin};

// ---- mock upstream state ----

struct MockState {
    /// Issued refresh tokens → still valid? (single-use rotation).
    refresh_tokens: Mutex<std::collections::BTreeMap<String, bool>>,
    /// Accounts that exist (email → password).
    accounts: std::collections::BTreeMap<String, String>,
    /// The cookie value app endpoints require (set on login/refresh).
    valid_cookie: Mutex<String>,
    /// Forced stream behavior.
    stream_mode: Mutex<StreamMode>,
}

#[derive(Debug, Clone, PartialEq)]
enum StreamMode {
    Success,
    AccountSuspended,
    SessionUnknown,
    ModelUnavailable,
}

impl MockState {
    fn new(email: &str, password: &str) -> Self {
        Self {
            refresh_tokens: Mutex::new(Default::default()),
            accounts: [(email.to_string(), password.to_string())].into(),
            valid_cookie: Mutex::new(String::new()),
            stream_mode: Mutex::new(StreamMode::Success),
        }
    }
}

type SharedMock = Arc<MockState>;

fn app(state: SharedMock) -> Router {
    Router::new()
        .route("/auth/v1/token", post(auth_token))
        .route("/api/me/privacy", get(privacy_get).post(privacy_post))
        .route("/api/direct-chat/sessions", post(sessions_create))
        .route("/api/direct-chat/stream", post(stream))
        .route("/models", get(models_page))
        .with_state(state)
}

async fn auth_token(
    State(state): State<SharedMock>,
    RawQuery(query): RawQuery,
    Json(body): Json<Value>,
) -> Response {
    let refresh_grant = query
        .as_deref()
        .unwrap_or_default()
        .contains("grant_type=refresh_token");
    if refresh_grant {
        let token = body["refresh_token"]
            .as_str()
            .unwrap_or_default()
            .to_string();
        let mut tokens = state.refresh_tokens.lock().await;
        if !tokens.get(&token).copied().unwrap_or(false) {
            return unauthorized_json();
        }
        // Single-use rotation: consume, issue a fresh pair.
        tokens.insert(token, false);
        let new_refresh = format!("rt-{}", tokens.len());
        tokens.insert(new_refresh.clone(), true);
        let grant = grant_json(
            &format!("at-{new_refresh}"),
            &new_refresh,
            "mock@example.com",
        );
        *state.valid_cookie.lock().await = cookie_from_grant(&grant);
        (StatusCode::OK, Json(grant)).into_response()
    } else {
        let email = body["email"].as_str().unwrap_or_default().to_string();
        let password = body["password"].as_str().unwrap_or_default().to_string();
        match state.accounts.get(&email) {
            Some(pw) if *pw == password => {
                let refresh = format!("rt-login-{}", state.refresh_tokens.lock().await.len());
                state
                    .refresh_tokens
                    .lock()
                    .await
                    .insert(refresh.clone(), true);
                let grant = grant_json(&format!("at-login-{refresh}"), &refresh, &email);
                *state.valid_cookie.lock().await = cookie_from_grant(&grant);
                (StatusCode::OK, Json(grant)).into_response()
            }
            _ => unauthorized_json(),
        }
    }
}

fn grant_json(access: &str, refresh: &str, email: &str) -> Value {
    json!({
        "access_token": access,
        "refresh_token": refresh,
        "expires_in": 3600,
        "expires_at": chrono::Utc::now().timestamp() + 3600,
        "token_type": "bearer",
        "user": {"id": "u-1", "email": email},
    })
}

fn unauthorized_json() -> Response {
    (StatusCode::UNAUTHORIZED, Json(json!({"ok": false}))).into_response()
}

/// Cookie value the app endpoints demand from a grant (same encoding as
/// the plugin's auth.rs: `sb-auth-auth-token.0=base64-<urlsafe-b64>`).
fn cookie_from_grant(grant: &Value) -> String {
    let payload = json!({
        "access_token": grant["access_token"],
        "token_type": "bearer",
        "expires_in": grant["expires_in"],
        "expires_at": grant["expires_at"],
        "refresh_token": grant["refresh_token"],
        "user": grant["user"],
    });
    format!(
        "sb-auth-auth-token.0=base64-{}",
        URL_SAFE_NO_PAD.encode(payload.to_string())
    )
}

async fn privacy_get(State(state): State<SharedMock>, headers: HeaderMap) -> Response {
    if !authed(&state, &headers).await {
        return unauthorized_json();
    }
    (
        StatusCode::OK,
        Json(json!({"ok": true, "free_models_enabled": true, "consent_version": 2})),
    )
        .into_response()
}

async fn privacy_post(State(state): State<SharedMock>, headers: HeaderMap) -> Response {
    if !authed(&state, &headers).await {
        return unauthorized_json();
    }
    (
        StatusCode::OK,
        Json(json!({"ok": true, "free_models_enabled": true, "consent_version": 2})),
    )
        .into_response()
}

async fn sessions_create(
    State(state): State<SharedMock>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Response {
    if !authed(&state, &headers).await {
        return unauthorized_json();
    }
    // Live fact (dossier §2): ANY model string accepted, echoed verbatim.
    let model = body["model"].as_str().unwrap_or_default().to_string();
    (
        StatusCode::CREATED,
        Json(json!({"session": {"id": "11111111-2222-3333-4444-555555555555", "model": model}})),
    )
        .into_response()
}

async fn stream(
    State(state): State<SharedMock>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Response {
    if !authed(&state, &headers).await {
        return unauthorized_json();
    }
    let mode = state.stream_mode.lock().await.clone();
    match mode {
        StreamMode::AccountSuspended => sse_error_response(
            StatusCode::FORBIDDEN,
            "account_suspended",
            "Your account is suspended. Reply to the email we sent you about it.",
        ),
        StreamMode::SessionUnknown => sse_error_response(
            StatusCode::NOT_FOUND,
            "session_unknown",
            "This chat does not exist. Start a new one.",
        ),
        StreamMode::ModelUnavailable => sse_error_response(
            StatusCode::FORBIDDEN,
            "model_unavailable",
            "That model isn't available in web chat.",
        ),
        StreamMode::Success => {
            // missing sessionId → 400 missing_session (dossier §6).
            if body["sessionId"].as_str().is_none() {
                return sse_error_response(
                    StatusCode::BAD_REQUEST,
                    "missing_session",
                    "sessionId required.",
                );
            }
            let model = body["model"].as_str().unwrap_or("").to_string();
            let sse = format!(
                "event: thinking\ndata: {{\"delta\":\"thinking hard \"}}\n\n\
                 event: chunk\ndata: {{\"delta\":\"MOCK:\"}}\n\n\
                 event: chunk\ndata: {{\"delta\":\" reply\"}}\n\n\
                 event: done\ndata: {{\"messageId\":\"msg-mock\",\"model\":\"{model}\",\"searchCount\":0,\"usage\":{{\"inputTokens\":7,\"outputTokens\":2}},\"billing\":\"free\"}}\n\n"
            );
            (
                StatusCode::OK,
                [("content-type", "text/event-stream; charset=utf-8")],
                sse,
            )
                .into_response()
        }
    }
}

async fn models_page() -> Response {
    let payload = include_str!("../fixtures/models-rsc-small.txt");
    (
        StatusCode::OK,
        [("content-type", "text/plain; charset=utf-8")],
        payload.to_string(),
    )
        .into_response()
}

async fn authed(state: &SharedMock, headers: &HeaderMap) -> bool {
    let cookie = headers
        .get("cookie")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default();
    let expected = state.valid_cookie.lock().await;
    cookie == *expected
}

/// SSE error frame with a non-200 status — the dossier's central wire
/// fact (§6): errors ride the SSE body, so status alone is not enough.
fn sse_error_response(status: StatusCode, code: &str, message: &str) -> Response {
    let body = format!("event: error\ndata: {{\"code\":\"{code}\",\"message\":\"{message}\"}}\n\n");
    (
        status,
        [("content-type", "text/event-stream; charset=utf-8")],
        body,
    )
        .into_response()
}

// ---- harness ----

async fn start_mock(email: &str, password: &str) -> (SharedMock, String) {
    let state = Arc::new(MockState::new(email, password));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let router = app(Arc::clone(&state));
    tokio::spawn(async move {
        axum::serve(listener, router).await.unwrap();
    });
    (state, format!("http://{addr}"))
}

/// Plugin wired to the mock; pacing is the client's real 1.5s gate — each
/// test makes a handful of calls, keeping the suite bounded.
fn wired_plugin(base: &str, store: &Arc<Store>) -> TokenHarbor {
    let client = Arc::new(Client::with_base(base.to_string(), Duration::from_secs(10)));
    TokenHarbor::new(Arc::clone(store), client, base.to_string()).unwrap()
}

fn seed_store(email: &str, password: &str) -> Arc<Store> {
    let store = Arc::new(Store::in_memory().unwrap());
    store
        .upsert_account(&AccountRow::seed(email, password))
        .unwrap();
    store
}

fn chat_request(model: &str, stream: bool) -> ChatRequest {
    ChatRequest::new(json!({
        "model": model,
        "messages": [
            {"role": "system", "content": "Be terse."},
            {"role": "user", "content": "hello"},
        ],
        "stream": stream,
    }))
    .unwrap()
}

const EMAIL: &str = "mock@example.com";
const PASSWORD: &str = "mock-pw";

// ---- tests ----

#[tokio::test]
async fn login_refresh_rotation_persisted() {
    let (state, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let auth = Auth::with_base(Arc::clone(&store), reqwest::Client::new(), base);

    // Login.
    let row = store.accounts().unwrap().pop().unwrap();
    auth.ensure_session(&row).await.unwrap();
    let after_login = store.accounts().unwrap().pop().unwrap();
    let login_refresh = after_login.refresh_token.clone();
    assert!(!login_refresh.is_empty());

    // Force expiry → refresh rotates.
    store
        .save_tokens(
            EMAIL,
            &after_login.access_token,
            &login_refresh,
            1,
            &after_login.user_json,
        )
        .unwrap();
    let expired = store.accounts().unwrap().pop().unwrap();
    assert!(!expired.access_valid(chrono::Utc::now().timestamp()));
    auth.ensure_session(&expired).await.unwrap();
    let rotated = store.accounts().unwrap().pop().unwrap();
    assert_ne!(rotated.refresh_token, login_refresh, "rotation happened");
    assert_eq!(*state.valid_cookie.lock().await, {
        use oxcost_plugin_tokenharbor::auth::cookie_for;
        let grant = oxcost_plugin_tokenharbor::auth::SessionGrant {
            access_token: rotated.access_token.clone(),
            refresh_token: rotated.refresh_token.clone(),
            expires_in: 3600,
            expires_at: rotated.access_expires_at,
            user_json: rotated.user_json.clone(),
        };
        cookie_for(&grant)
    });

    // Old token is consumed (single-use, dossier risk #1): a refresh
    // attempt with it fails → password re-login recovers.
    store
        .save_tokens(
            EMAIL,
            &rotated.access_token,
            &login_refresh,
            1,
            &rotated.user_json,
        )
        .unwrap();
    let stale = store.accounts().unwrap().pop().unwrap();
    auth.ensure_session(&stale).await.unwrap();
    let recovered = store.accounts().unwrap().pop().unwrap();
    assert!(!recovered.refresh_token.is_empty());
}

#[tokio::test]
async fn full_chat_success_stream() {
    let (_, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    let outcome = plugin
        .chat(
            chat_request("xiaomi/mimo-v2.6-flash:free", true),
            Default::default(),
        )
        .await
        .unwrap();
    let ChatOutcome::Sse(bytes) = outcome else {
        panic!("expected SSE outcome");
    };
    let text = String::from_utf8(bytes).unwrap();
    assert!(text.contains("\"content\":\"MOCK:\""));
    assert!(text.contains("\"content\":\" reply\""));
    assert!(text.contains("\"finish_reason\":\"stop\""));
    assert!(text.contains("\"prompt_tokens\":7"));
    assert!(text.ends_with("data: [DONE]\n\n"));
    // thinking must not leak into content (v1 decision).
    assert!(!text.contains("thinking hard"));
}

#[tokio::test]
async fn full_chat_success_non_stream() {
    let (_, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    let outcome = plugin
        .chat(
            chat_request("xiaomi/mimo-v2.6-flash:free", false),
            Default::default(),
        )
        .await
        .unwrap();
    let ChatOutcome::Json(body) = outcome else {
        panic!("expected JSON outcome");
    };
    assert_eq!(body["object"], "chat.completion");
    assert_eq!(body["choices"][0]["message"]["content"], "MOCK: reply");
    assert_eq!(body["usage"]["total_tokens"], 9);
}

#[tokio::test]
async fn account_suspended_maps_429_and_quarantines() {
    let (state, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    *state.stream_mode.lock().await = StreamMode::AccountSuspended;
    let err = plugin
        .chat(
            chat_request("xiaomi/mimo-v2.6-flash:free", false),
            Default::default(),
        )
        .await
        .unwrap_err();
    assert_eq!(err.status, 429);
    assert_eq!(err.err_type, "rate_limit_error");
    // Upstream sentinel text must not leak (§4).
    assert!(!err.message.contains("Reply to the email"));

    // Quarantine persisted → next request short-circuits; health flips;
    // the plugin never crashes.
    let row = &store.accounts().unwrap()[0];
    assert_eq!(row.quarantined_reason.as_deref(), Some("account_suspended"));
    let err2 = plugin
        .chat(
            chat_request("xiaomi/mimo-v2.6-flash:free", false),
            Default::default(),
        )
        .await
        .unwrap_err();
    assert_eq!(err2.status, 429, "no usable account → 429");
    assert_eq!(plugin.health(), HealthStatus::Unhealthy);
}

#[tokio::test]
async fn model_unavailable_maps_404_model_not_found() {
    let (state, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    *state.stream_mode.lock().await = StreamMode::ModelUnavailable;
    let err = plugin
        .chat(chat_request("th-rudder:free", false), Default::default())
        .await
        .unwrap_err();
    assert_eq!(err.status, 404);
    assert_eq!(err.err_type, "invalid_request_error");
    assert_eq!(err.code.as_deref(), Some("model_not_found"));
}

#[tokio::test]
async fn session_unknown_maps_502() {
    let (state, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    *state.stream_mode.lock().await = StreamMode::SessionUnknown;
    let err = plugin
        .chat(
            chat_request("xiaomi/mimo-v2.6-flash:free", false),
            Default::default(),
        )
        .await
        .unwrap_err();
    assert_eq!(err.status, 502);
    assert_eq!(err.err_type, "upstream_error");
}

#[tokio::test]
async fn refresh_catalog_fetches_and_persists() {
    let (state, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    let snapshot = plugin.refresh_catalog().await.unwrap();
    assert_eq!(snapshot.models.len(), 5);
    let persisted = store.catalog().unwrap();
    assert_eq!(persisted.models.len(), 5);
    assert_eq!(
        persisted.server_now.as_deref(),
        Some("2026-10-12T05:30:18.826Z")
    );
    assert!(
        state.valid_cookie.lock().await.is_empty(),
        "catalog fetch stays anonymous"
    );
}

#[tokio::test]
async fn models_serving_view_follows_catalog_and_tier() {
    let (_, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);
    let plugin = wired_plugin(&base, &store);

    plugin.refresh_catalog().await.unwrap();
    store
        .save_free_tier(&FreeTier {
            email: EMAIL.into(),
            model_ids: vec!["mimo-v2.6-flash".into()],
            fetched_at: 1,
        })
        .unwrap();
    let ids: Vec<String> = plugin.models().into_iter().map(|m| m.id).collect();
    assert_eq!(
        ids,
        vec!["tokenharbor/xiaomi/mimo-v2.6-flash:free"],
        "entitlement intersection narrows the serving view"
    );
}

#[tokio::test]
async fn registry_round_trip_with_mock_catalog() {
    let (_, base) = start_mock(EMAIL, PASSWORD).await;
    let store = seed_store(EMAIL, PASSWORD);

    // Production path: connect() refreshes the catalog BEFORE building the
    // registration-time manifest, so the §3 declaration matches models().
    let plugin = TokenHarbor::connect(
        store,
        Arc::new(Client::with_base(base.clone(), Duration::from_secs(10))),
        base,
    )
    .await
    .unwrap();
    let plugin = Arc::new(plugin);
    assert_eq!(
        plugin.models().len(),
        3,
        "all public free rows, no tier yet"
    );

    // Registration validates manifest (prefixed free ids) vs models().
    let mut reg = oxcost_sdk::Registry::new();
    reg.register(plugin as Arc<dyn oxcost_sdk::OxcostPlugin>)
        .expect("registers cleanly");
}
