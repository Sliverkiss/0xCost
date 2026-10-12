//! Offline integration tests: host server against a mock plugin upstream
//! (in-process axum on a loopback ephemeral port), covering contract §5
//! forwarding, auth, aggregation, and error mapping.

use std::collections::BTreeMap;
use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use axum::response::Response;
use http_body_util::BodyExt;
use oxcost::server::{app_router, probe_all};
use oxcost::state::{AppState, HostConfig};
use oxcost_contract::parse_manifest;
use oxcost_core::{KeySource, PluginInstance, PluginState};
use serde_json::{json, Value};

/// Manifest builder (valid TOML, no string surgery).
fn manifest_for(
    id: &str,
    name: &str,
    port: u16,
    models: &[&str],
    enabled: bool,
    unknown: &str,
) -> oxcost_contract::PluginManifest {
    let models_toml = models
        .iter()
        .map(|m| format!("\"{m}\""))
        .collect::<Vec<_>>()
        .join(", ");
    let text = format!(
        r#"
manifestVersion = "0.1"
id = "{id}"
name = "{name}"
version = "1.0.0"
apiVersion = "v1"
license = "MIT"

[entry]
mode = "external"
port = {port}

[health]
path = "/healthz"
interval_secs = 30
timeout_secs = 5

[routing]
enabled = {enabled}
models = [{models_toml}]
unknown_model = "{unknown}"

[capabilities]
chat = true

[auth]
keys = "env:MOCK_TEST_KEY"
"#
    );
    parse_manifest(&text).expect("mock manifest parses")
}

/// Spin up a mock plugin upstream; returns its port. The chat endpoint
/// records the Authorization header into the returned shared vec.
type AuthLog = Arc<std::sync::Mutex<Vec<Option<String>>>>;

async fn spawn_mock_plugin() -> (u16, AuthLog) {
    let received: AuthLog = Arc::new(std::sync::Mutex::new(Vec::new()));
    let received_chat = Arc::clone(&received);

    let app = axum::Router::new()
        .route(
            "/healthz",
            axum::routing::get(|| async { axum::Json(json!({"status": "ok"})) }),
        )
        .route(
            "/v1/models",
            axum::routing::get(|| async {
                axum::Json(json!({"object": "list", "data": [
                    {"id": "mock-a", "object": "model"},
                    {"id": "mock-b", "object": "model"},
                ]}))
            }),
        )
        .route(
            "/v1/chat/completions",
            axum::routing::post(
                move |headers: axum::http::HeaderMap, body: String| async move {
                    let auth = headers
                        .get(header::AUTHORIZATION)
                        .and_then(|v| v.to_str().ok())
                        .map(str::to_string);
                    received_chat.lock().unwrap().push(auth);
                    let body: Value = serde_json::from_str(&body).expect("mock got valid json");
                    let model = body["model"].as_str().unwrap_or_default().to_string();
                    axum::Json(json!({
                        "id": "chatcmpl-mock",
                        "object": "chat.completion",
                        "model": model,
                        "choices": [{
                            "index": 0,
                            "message": {"role": "assistant", "content": "mock says hi"},
                            "finish_reason": "stop"
                        }],
                    }))
                },
            ),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (port, received)
}

fn state_with(
    instances: BTreeMap<String, Arc<PluginInstance>>,
    config: HostConfig,
) -> Arc<AppState> {
    Arc::new(AppState::new(instances, config, Vec::new()))
}

fn one_instance(port: u16, key: KeySource) -> BTreeMap<String, Arc<PluginInstance>> {
    let mut instances = BTreeMap::new();
    instances.insert(
        "dev.oxcost.mock".into(),
        Arc::new(PluginInstance::new(
            manifest_for(
                "dev.oxcost.mock",
                "mock",
                port,
                &["mock/mock-a", "mock/mock-b"],
                true,
                "reject",
            ),
            key,
        )),
    );
    instances
}

async fn send(app: &axum::Router, req: Request<Body>) -> (StatusCode, Value, Option<String>) {
    let response = tower::ServiceExt::oneshot(app.clone(), req).await.unwrap();
    let status = response.status();
    let content_type = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string);
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body, content_type)
}

fn chat_request(model: &str) -> Request<Body> {
    Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": model, "messages": [{"role":"user","content":"hi"}]}).to_string(),
        ))
        .unwrap()
}

#[tokio::test]
async fn json_pass_through_and_model_rewrite() {
    let (port, _) = spawn_mock_plugin().await;
    let state = state_with(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
    );
    probe_all(&state).await;
    assert_eq!(state.instance_list()[0].state(), PluginState::Serving);

    let app = app_router(state);
    let (status, body, ct) = send(&app, chat_request("mock/mock-a")).await;
    assert_eq!(status, StatusCode::OK);
    assert!(ct.unwrap_or_default().starts_with("application/json"));
    // Model rewritten to the unprefixed upstream id; response relayed.
    assert_eq!(body["model"], "mock-a");
    assert_eq!(body["choices"][0]["message"]["content"], "mock says hi");
}

#[tokio::test]
async fn bearer_plugin_key_injected() {
    // Mock plugin on an ephemeral port; key env var set via a temp var name.
    let (port, received) = spawn_mock_plugin().await;
    let var_name = "OXCOST_TEST_MOCK_KEY_A";
    std::env::set_var(var_name, "plugin-secret-a");
    let state = state_with(
        one_instance(port, KeySource::Env(var_name.into())),
        HostConfig::dev_default(),
    );
    probe_all(&state).await;

    let app = app_router(state);
    // Host is in dev mode (no host keys) — but the plugin key must still be
    // injected on the outbound call.
    let _ = send(&app, chat_request("mock/mock-a")).await;
    let seen = received.lock().unwrap().pop().flatten();
    assert_eq!(seen.as_deref(), Some("Bearer plugin-secret-a"));
    std::env::remove_var(var_name);
}

#[tokio::test]
async fn upstream_refused_returns_502() {
    // Port 1: nothing listens there; connection refused.
    let mut instances = BTreeMap::new();
    instances.insert(
        "dev.oxcost.mock".into(),
        Arc::new(PluginInstance::new(
            manifest_for(
                "dev.oxcost.mock",
                "mock",
                1,
                &["mock/mock-a"],
                true,
                "reject",
            ),
            KeySource::None,
        )),
    );
    let state = state_with(instances, HostConfig::dev_default());
    // No probe (nothing listens): force Serving so routing reaches forwarding.
    *state.instance_list()[0].state_handle().lock().unwrap() = PluginState::Serving;

    let app = app_router(state);
    let (status, body, _) = send(&app, chat_request("mock/mock-a")).await;
    assert_eq!(status, StatusCode::BAD_GATEWAY);
    assert_eq!(body["error"]["code"], "upstream_error");
    assert_eq!(body["error"]["type"], "upstream_error");
}

#[tokio::test]
async fn unknown_model_returns_404_openai_body() {
    let (port, _) = spawn_mock_plugin().await;
    let state = state_with(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
    );
    probe_all(&state).await;

    let app = app_router(state);
    let (status, body, _) = send(&app, chat_request("mock/does-not-exist")).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(body["error"]["code"], "model_not_found");
    assert_eq!(body["error"]["type"], "invalid_request_error");
    assert!(body["error"]["message"]
        .as_str()
        .unwrap()
        .contains("does-not-exist"));
}

#[tokio::test]
async fn empty_messages_returns_400() {
    let (port, _) = spawn_mock_plugin().await;
    let state = state_with(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
    );
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": "mock/mock-a", "messages": []}).to_string(),
        ))
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(body["error"]["code"], "invalid_request");
}

#[tokio::test]
async fn malformed_body_returns_400() {
    let (port, _) = spawn_mock_plugin().await;
    let state = state_with(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
    );
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from("not json at all"))
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(body["error"]["code"], "invalid_request");
}

#[tokio::test]
async fn host_auth_401_when_keys_configured() {
    let (port, _) = spawn_mock_plugin().await;
    let mut config = HostConfig::dev_default();
    config.api_keys = vec!["host-secret".into()];
    let state = state_with(one_instance(port, KeySource::None), config);
    probe_all(&state).await;

    let app = app_router(state);

    // No key → 401.
    let (status, body, _) = send(&app, chat_request("mock/mock-a")).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    assert_eq!(body["error"]["code"], "invalid_api_key");

    // Wrong key → 401.
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::AUTHORIZATION, "Bearer wrong")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": "mock/mock-a", "messages": [{"role":"user","content":"hi"}]})
                .to_string(),
        ))
        .unwrap();
    let (status, _, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);

    // Right key → 200.
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::AUTHORIZATION, "Bearer host-secret")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": "mock/mock-a", "messages": [{"role":"user","content":"hi"}]})
                .to_string(),
        ))
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["model"], "mock-a");
}

#[tokio::test]
async fn stream_pass_through_sse_bytes_identical() {
    // Mock upstream that replies with fixed SSE bytes.
    let sse_body = b"data: {\"id\":\"x\",\"choices\":[]}\n\ndata: [DONE]\n\n";
    let app = axum::Router::new()
        .route(
            "/healthz",
            axum::routing::get(|| async { axum::Json(json!({"status": "ok"})) }),
        )
        .route(
            "/v1/chat/completions",
            axum::routing::post(move |body: String| async move {
                // Upstream decides stream-ness by the request flag.
                if body.contains("\"stream\":true") {
                    let stream = futures_util::stream::iter(vec![
                        Ok::<_, std::io::Error>(axum::body::Bytes::from_static(
                            b"data: {\"id\":\"x\",\"choices\":[]}\n\n",
                        )),
                        Ok(axum::body::Bytes::from_static(b"data: [DONE]\n\n")),
                    ]);
                    Response::builder()
                        .status(200)
                        .header(header::CONTENT_TYPE, "text/event-stream")
                        .body(Body::from_stream(stream))
                        .unwrap()
                } else {
                    Response::builder()
                        .status(200)
                        .header(header::CONTENT_TYPE, "application/json")
                        .body(Body::from("{\"model\":\"mock-a\"}"))
                        .unwrap()
                }
            }),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });

    let state = state_with(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
    );
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": "mock/mock-a", "messages": [{"role":"user","content":"hi"}], "stream": true})
                .to_string(),
        ))
        .unwrap();
    let response = tower::ServiceExt::oneshot(app, req).await.unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert!(response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .starts_with("text/event-stream"));
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    // SSE bytes relayed verbatim.
    assert_eq!(&bytes[..], &sse_body[..]);
}

#[tokio::test]
async fn models_aggregation_excludes_disabled_and_non_serving() {
    let (port, _) = spawn_mock_plugin().await;
    let mut instances = BTreeMap::new();

    // Serving plugin with two models.
    let serving = PluginInstance::new(
        manifest_for(
            "dev.oxcost.mock",
            "mock",
            port,
            &["mock/mock-a", "mock/mock-b"],
            true,
            "reject",
        ),
        KeySource::None,
    );
    *serving.state_handle().lock().unwrap() = PluginState::Serving;
    instances.insert("dev.oxcost.mock".into(), Arc::new(serving));

    // Second plugin: enabled=false → excluded even though Serving.
    let disabled = PluginInstance::new(
        manifest_for(
            "dev.oxcost.other",
            "other",
            port,
            &["other/x"],
            false,
            "reject",
        ),
        KeySource::None,
    );
    *disabled.state_handle().lock().unwrap() = PluginState::Serving;
    instances.insert("dev.oxcost.other".into(), Arc::new(disabled));

    // Third plugin: enabled but not Serving → excluded (probe-failure path).
    let probing = PluginInstance::new(
        manifest_for(
            "dev.oxcost.third",
            "third",
            port,
            &["third/y"],
            true,
            "reject",
        ),
        KeySource::None,
    );
    // stays Registered (no probe ran)
    instances.insert("dev.oxcost.third".into(), Arc::new(probing));

    let state = state_with(instances, HostConfig::dev_default());
    let app = app_router(state);

    let req = Request::builder()
        .uri("/v1/models")
        .body(Body::empty())
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    let ids: Vec<&str> = body["data"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec!["mock/mock-a", "mock/mock-b"]);
    assert_eq!(body["object"], "list");
}

#[tokio::test]
async fn healthz_reports_states_and_failures() {
    let (port, _) = spawn_mock_plugin().await;
    let state = Arc::new(AppState::new(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
        vec![oxcost::state::FailedPlugin {
            plugin: "dev.oxcost.broken".into(),
            reason: "auth reference `env:BROKEN_KEY` cannot be resolved".into(),
        }],
    ));
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .uri("/healthz")
        .body(Body::empty())
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["status"], "ok");
    assert_eq!(body["plugins"][0]["state"], "Serving");
    assert_eq!(body["failed"][0]["plugin"], "dev.oxcost.broken");
}

#[tokio::test]
async fn non_serving_plugin_not_routed() {
    // Plugin registered but probe never ran → Registered → excluded.
    let (port, _) = spawn_mock_plugin().await;
    let state = state_with(
        one_instance(port, KeySource::None),
        HostConfig::dev_default(),
    );
    // No probe_all call: state stays Registered.

    let app = app_router(state);
    let (status, body, _) = send(&app, chat_request("mock/mock-a")).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(body["error"]["code"], "model_not_found");
}

// ---- SDK-plugin path (contract §10, M3) ----

use oxcost::state::FailedPlugin;
use oxcost_core::PluginInstance as CorePluginInstance;
use oxcost_sdk::SampleEcho as SdkSampleEcho;

/// One SDK instance (the sample-echo fixture) as a state map.
fn sdk_instances() -> BTreeMap<String, Arc<CorePluginInstance>> {
    let mut instances = BTreeMap::new();
    instances.insert(
        "dev.oxcost.sample-echo".into(),
        Arc::new(CorePluginInstance::from_sdk_plugin(Arc::new(
            SdkSampleEcho::new(&[]),
        ))),
    );
    instances
}

#[tokio::test]
async fn sdk_plugin_visible_in_models_catalog() {
    let state = state_with(sdk_instances(), HostConfig::dev_default());
    probe_all(&state).await;
    assert_eq!(state.instance_list()[0].state(), PluginState::Serving);

    let app = app_router(state);
    let req = Request::builder()
        .uri("/v1/models")
        .body(Body::empty())
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    let ids: Vec<&str> = body["data"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec!["sample-echo/echo-1"]);
    assert_eq!(body["data"][0]["owned_by"], "sample-echo");
}

#[tokio::test]
async fn sdk_plugin_chat_round_trip_json() {
    let state = state_with(sdk_instances(), HostConfig::dev_default());
    probe_all(&state).await;

    let app = app_router(state);
    let (status, body, ct) = send(&app, chat_request("sample-echo/echo-1")).await;
    assert_eq!(status, StatusCode::OK);
    assert!(ct.unwrap_or_default().starts_with("application/json"));
    // Model rewritten to the unprefixed id; canned reply relayed verbatim.
    assert_eq!(body["model"], "echo-1");
    assert_eq!(body["choices"][0]["message"]["content"], "echo: hi");
    assert_eq!(body["oxcost_sample"], true);
}

#[tokio::test]
async fn sdk_plugin_chat_round_trip_stream() {
    let state = state_with(sdk_instances(), HostConfig::dev_default());
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": "sample-echo/echo-1", "messages": [{"role":"user","content":"yo"}], "stream": true})
                .to_string(),
        ))
        .unwrap();
    let response = tower::ServiceExt::oneshot(app, req).await.unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert!(response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .starts_with("text/event-stream"));
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    let text = String::from_utf8(bytes.to_vec()).unwrap();
    assert!(text.contains("\"content\":\"echo: yo\""));
    assert!(text.ends_with("data: [DONE]\n\n"));
}

#[tokio::test]
async fn sdk_plugin_health_reflected_in_healthz() {
    let state = Arc::new(AppState::new(
        sdk_instances(),
        HostConfig::dev_default(),
        vec![FailedPlugin {
            plugin: "dev.oxcost.broken-sdk".into(),
            reason: "synthetic failure for the table".into(),
        }],
    ));
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .uri("/healthz")
        .body(Body::empty())
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["status"], "ok");
    // SDK plugin probed via the in-process health() call → Serving.
    let sdk_entry = body["plugins"]
        .as_array()
        .unwrap()
        .iter()
        .find(|p| p["id"] == "dev.oxcost.sample-echo")
        .expect("sdk plugin in healthz");
    assert_eq!(sdk_entry["state"], "Serving");
    assert_eq!(sdk_entry["routable"], true);
}

#[tokio::test]
async fn sdk_plugin_unknown_model_404_and_auth_401() {
    // Unknown model → 404 model_not_found, same namespace rules as sidecars.
    let state = state_with(sdk_instances(), HostConfig::dev_default());
    probe_all(&state).await;
    let app = app_router(state);
    let (status, body, _) = send(&app, chat_request("sample-echo/ghost")).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(body["error"]["code"], "model_not_found");
    assert_eq!(body["error"]["type"], "invalid_request_error");

    // A model belonging to no plugin at all → 404 as well.
    let state = state_with(sdk_instances(), HostConfig::dev_default());
    probe_all(&state).await;
    let app = app_router(state);
    let (status, _, _) = send(&app, chat_request("elsewhere/m")).await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // Host auth enforced on the SDK path too.
    let mut config = HostConfig::dev_default();
    config.api_keys = vec!["host-secret".into()];
    let state = state_with(sdk_instances(), config);
    probe_all(&state).await;
    let app = app_router(state);
    let (status, body, _) = send(&app, chat_request("sample-echo/echo-1")).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    assert_eq!(body["error"]["code"], "invalid_api_key");
    assert_eq!(body["error"]["type"], "invalid_request_error");

    // With the right key the SDK path serves.
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::AUTHORIZATION, "Bearer host-secret")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            json!({"model": "sample-echo/echo-1", "messages": [{"role":"user","content":"k"}]})
                .to_string(),
        ))
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["model"], "echo-1");
}

#[tokio::test]
async fn sdk_plugin_error_maps_onto_openai_body() {
    // The failing variant exercises the §4 error mapping over the SDK path.
    let mut instances = BTreeMap::new();
    instances.insert(
        "dev.oxcost.sample-echo".into(),
        Arc::new(CorePluginInstance::from_sdk_plugin(Arc::new(
            SdkSampleEcho::failing(),
        ))),
    );
    let state = state_with(instances, HostConfig::dev_default());
    probe_all(&state).await;

    let app = app_router(state);
    let (status, body, _) = send(&app, chat_request("sample-echo/echo-1")).await;
    assert_eq!(status, StatusCode::BAD_GATEWAY);
    assert_eq!(body["error"]["type"], "upstream_error");
    assert_eq!(body["error"]["message"], "sample-echo: canned failure");
}

#[tokio::test]
async fn sidecar_and_sdk_catalogs_merge_in_one_namespace() {
    // One sidecar (mock upstream) + one SDK plugin, merged catalog.
    let (port, _) = spawn_mock_plugin().await;
    let mut instances = one_instance(port, KeySource::None);
    instances.extend(sdk_instances());
    let state = state_with(instances, HostConfig::dev_default());
    probe_all(&state).await;

    let app = app_router(state);
    let req = Request::builder()
        .uri("/v1/models")
        .body(Body::empty())
        .unwrap();
    let (status, body, _) = send(&app, req).await;
    assert_eq!(status, StatusCode::OK);
    let mut ids: Vec<&str> = body["data"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m["id"].as_str().unwrap())
        .collect();
    ids.sort_unstable();
    assert_eq!(
        ids,
        vec!["mock/mock-a", "mock/mock-b", "sample-echo/echo-1"]
    );

    // Both forms route: sidecar…
    let (status, body, _) = send(&app, chat_request("mock/mock-a")).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["choices"][0]["message"]["content"], "mock says hi");
    // …and SDK.
    let (status, body, _) = send(&app, chat_request("sample-echo/echo-1")).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["choices"][0]["message"]["content"], "echo: hi");
}
