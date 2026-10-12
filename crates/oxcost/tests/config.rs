//! A2 host-config integration tests: precedence matrix for
//! `[plugins."<id>"].enabled` vs manifest `routing.enabled`, each case
//! asserting the §6 three-way exclusion (routing, catalog, probing).

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use http_body_util::BodyExt;
use oxcost::config::HostConfigFile;
use oxcost::server::{app_router, probe_all};
use oxcost::state::{AppState, HostConfig};
use oxcost_contract::parse_manifest;
use oxcost_core::{PluginInstance, PluginState};
use serde_json::Value;

/// External-mode manifest fixture with a live mock upstream port.
fn manifest_for(id: &str, name: &str, port: u16, enabled: bool) -> oxcost_contract::PluginManifest {
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
models = ["{name}/m"]
unknown_model = "reject"

[auth]
keys = "env:CFG_MATRIX_KEY"
"#
    );
    parse_manifest(&text).expect("fixture manifest parses")
}

/// Mock plugin upstream: healthy /healthz + canned chat reply.
async fn spawn_mock_plugin() -> u16 {
    let app = axum::Router::new()
        .route(
            "/healthz",
            axum::routing::get(|| async { axum::Json(serde_json::json!({"status": "ok"})) }),
        )
        .route(
            "/v1/chat/completions",
            axum::routing::post(move |body: String| async move {
                let v: Value = serde_json::from_str(&body).expect("mock got json");
                axum::Json(serde_json::json!({
                    "model": v["model"],
                    "choices": [{"index": 0,
                        "message": {"role": "assistant", "content": "cfg-matrix reply"},
                        "finish_reason": "stop"}],
                }))
            }),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    port
}

/// Build a state with one instance whose enabled flag came from the
/// config/manifest precedence under test, run the startup probe, and
/// return the live router.
async fn assemble(port: u16, manifest_enabled: bool, config_text: &str) -> axum::Router {
    let mut instances = BTreeMap::new();
    let mut manifest = manifest_for("dev.oxcost.cfgm", "cfgm", port, manifest_enabled);
    if !config_text.is_empty() {
        let cfg = HostConfigFile::parse(config_text).expect("config parses");
        cfg.apply_overrides(&mut manifest);
    }
    instances.insert(
        "dev.oxcost.cfgm".into(),
        Arc::new(PluginInstance::new(manifest, oxcost_core::KeySource::None)),
    );
    let state = Arc::new(AppState::new(
        instances,
        HostConfig::dev_default(),
        Vec::new(),
    ));
    probe_all(&state).await;
    app_router(state)
}

async fn get(app: &axum::Router, uri: &str) -> (StatusCode, Value) {
    let req = Request::builder().uri(uri).body(Body::empty()).unwrap();
    let resp = tower::ServiceExt::oneshot(app.clone(), req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    (
        status,
        serde_json::from_slice(&bytes).unwrap_or(Value::Null),
    )
}

async fn chat(app: &axum::Router, model: &str) -> StatusCode {
    let req = Request::builder()
        .method("POST")
        .uri("/v1/chat/completions")
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            serde_json::json!({"model": model, "messages": [{"role":"user","content":"hi"}]})
                .to_string(),
        ))
        .unwrap();
    let resp = tower::ServiceExt::oneshot(app.clone(), req).await.unwrap();
    resp.status()
}

/// The three-way exclusion for a disabled plugin, as one helper so every
/// matrix case asserts all three surfaces.
async fn assert_invisible(app: &axum::Router) {
    // 1. Routing: model not found (404).
    assert_eq!(chat(app, "cfgm/m").await, StatusCode::NOT_FOUND);
    // 2. Catalog: no advertised models.
    let (status, body) = get(app, "/v1/models").await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        body["data"].as_array().unwrap().is_empty(),
        "disabled plugin must not appear in /v1/models"
    );
}

async fn assert_visible(app: &axum::Router) {
    assert_eq!(chat(app, "cfgm/m").await, StatusCode::OK);
    let (status, body) = get(app, "/v1/models").await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["data"][0]["id"], "cfgm/m");
}

// ---- Precedence matrix ----

#[tokio::test]
async fn config_off_overrides_manifest_on() {
    let port = spawn_mock_plugin().await;
    let app = assemble(
        port,
        true,
        r#"[plugins."dev.oxcost.cfgm"]
enabled = false
"#,
    )
    .await;
    assert_invisible(&app).await;
}

#[tokio::test]
async fn config_on_overrides_manifest_off() {
    let port = spawn_mock_plugin().await;
    let app = assemble(
        port,
        false,
        r#"[plugins."dev.oxcost.cfgm"]
enabled = true
"#,
    )
    .await;
    assert_visible(&app).await;
}

#[tokio::test]
async fn no_section_manifest_default_applies() {
    // Manifest on + no section → visible.
    let port = spawn_mock_plugin().await;
    let app = assemble(port, true, "").await;
    assert_visible(&app).await;

    // Manifest off + no section → invisible.
    let port = spawn_mock_plugin().await;
    let app = assemble(port, false, "").await;
    assert_invisible(&app).await;
}

#[tokio::test]
async fn section_for_other_plugin_ignored() {
    let port = spawn_mock_plugin().await;
    let app = assemble(
        port,
        true,
        r#"[plugins."dev.oxcost.somebody-else"]
enabled = false
"#,
    )
    .await;
    assert_visible(&app).await;
}

// ---- Probing exclusion (the third leg) ----

/// The probe-frozen leg: with an enabled=false override, probe_all must
/// leave the instance Registered (never probed) even though a healthy
/// upstream exists.
#[tokio::test]
async fn disabled_plugin_is_not_probed() {
    let port = spawn_mock_plugin().await;
    let mut instances = BTreeMap::new();
    let mut manifest = manifest_for("dev.oxcost.cfgm", "cfgm", port, true);
    HostConfigFile::parse("[plugins.\"dev.oxcost.cfgm\"]\nenabled = false\n")
        .expect("parses")
        .apply_overrides(&mut manifest);
    let inst = Arc::new(PluginInstance::new(manifest, oxcost_core::KeySource::None));
    instances.insert("dev.oxcost.cfgm".into(), inst);
    let state = Arc::new(AppState::new(
        instances,
        HostConfig::dev_default(),
        Vec::new(),
    ));

    probe_all(&state).await;

    let inst = state.instance_list()[0].clone();
    assert_eq!(
        inst.state(),
        PluginState::Registered,
        "disabled plugin must never enter Probing"
    );
    assert!(!inst.is_servable());
}

#[tokio::test]
async fn enabled_by_config_is_probed_to_serving() {
    let port = spawn_mock_plugin().await;
    let mut instances = BTreeMap::new();
    let mut manifest = manifest_for("dev.oxcost.cfgm", "cfgm", port, false);
    HostConfigFile::parse("[plugins.\"dev.oxcost.cfgm\"]\nenabled = true\n")
        .expect("parses")
        .apply_overrides(&mut manifest);
    let inst = Arc::new(PluginInstance::new(manifest, oxcost_core::KeySource::None));
    instances.insert("dev.oxcost.cfgm".into(), inst);
    let state = Arc::new(AppState::new(
        instances,
        HostConfig::dev_default(),
        Vec::new(),
    ));

    probe_all(&state).await;

    let inst = state.instance_list()[0].clone();
    assert_eq!(inst.state(), PluginState::Serving);
    assert!(inst.is_servable());
}

// ---- Bad TOML surfaces with line numbers ----

#[test]
fn bad_config_error_carries_line() {
    let dir = std::env::temp_dir().join("oxcost-cfg-matrix");
    std::fs::create_dir_all(&dir).unwrap();
    let path = PathBuf::from(&dir).join("bad.toml");
    std::fs::write(&path, "[server]\nbind = 127.0.0.1:8123\n").unwrap();
    let err = HostConfigFile::load(&path).unwrap_err();
    assert!(err.0.contains("line 2"), "line info in: {}", err.0);
    std::fs::remove_file(&path).ok();
}
