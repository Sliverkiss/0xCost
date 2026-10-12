//! axum server assembly (contract §5 external surface):
//! - `POST /v1/chat/completions` — routing + forwarding
//! - `GET /v1/models`            — aggregated catalog
//! - `GET /healthz`              — host health + per-plugin state table
//!
//! On startup each plugin is health-probed (Registered→Probing→Serving);
//! a plugin not in Serving is excluded from routes and models (§6).

use std::sync::Arc;

use axum::extract::rejection::JsonRejection;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde_json::Value;

use crate::forward::{aggregate_models, forward_chat};
use crate::state::AppState;

/// Build the application router.
pub fn app_router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/v1/chat/completions", post(chat_completions))
        .route("/v1/models", get(models))
        .route("/healthz", get(healthz))
        .with_state(state)
}

/// `POST /v1/chat/completions`.
async fn chat_completions(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Result<Json<Value>, JsonRejection>,
) -> Response {
    let body = match body {
        Ok(Json(v)) => v,
        // Bad JSON is a 400-class invalid_request (contract §4).
        Err(rejection) => {
            return crate::forward::HostError::InvalidRequest(format!(
                "malformed request body: {rejection}"
            ))
            .into_response();
        }
    };
    match forward_chat(state, headers, body).await {
        Ok(response) => response,
        Err(e) => e.into_response(),
    }
}

/// `GET /v1/models`.
async fn models(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    let body = aggregate_models(&state);
    (StatusCode::OK, Json(body))
}

/// `GET /healthz` — host status + per-plugin state table + load failures.
async fn healthz(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    let plugins: Vec<Value> = state
        .instance_list()
        .into_iter()
        .map(|inst| {
            serde_json::json!({
                "id": inst.id(),
                "name": inst.name(),
                "state": inst.state().name(),
                "routable": inst.is_servable(),
                "models": inst.advertised_models(),
            })
        })
        .collect();
    let failures: Vec<Value> = state
        .failed
        .iter()
        .map(|f| {
            serde_json::json!({
                "plugin": f.plugin,
                "reason": f.reason,
            })
        })
        .collect();
    let body = serde_json::json!({
        "status": "ok",
        "auth": if state.config.api_keys.is_empty() { "dev (no keys)" } else { "key required" },
        "plugins": plugins,
        "failed": failures,
    });
    (StatusCode::OK, Json(body))
}

/// Health-probe every instance at startup: Registered → Probing → (Serving |
/// still Probing on failure). A probe failure leaves the plugin registered
/// but not Serving — excluded from routing until a later probe succeeds
/// (background re-probing loop arrives with the lifecycle loop milestone).
///
/// Probe form follows the transport handle: SDK plugins answer the
/// in-process `health()` trait call; sidecars get an HTTP GET (§6).
pub async fn probe_all(state: &AppState) {
    for inst in state.instance_list() {
        advance_state(&inst, |s| s.start_probing());
        let healthy = match inst.handle() {
            oxcost_core::PluginHandle::Sdk(plugin) => {
                plugin.health() == oxcost_sdk::HealthStatus::Ok
            }
            oxcost_core::PluginHandle::Sidecar { .. } => {
                let url = inst
                    .manifest()
                    .health
                    .as_ref()
                    .map(|h| inst.url_for(&h.path));
                match url {
                    Some(url) => probe(&url).await,
                    // remote mode has no /healthz; the contract's lightweight
                    // chat probe arrives with remote-mode support.
                    None => false,
                }
            }
        };
        if healthy {
            advance_state(&inst, |s| s.probe_succeeded());
            tracing::info!(plugin = inst.id(), "probe ok — Serving");
        } else {
            tracing::warn!(plugin = inst.id(), "probe failed — not Serving yet");
        }
    }
}

/// Apply a lifecycle transition to the instance's shared state handle.
/// M3 seam: transitions will additionally run through
/// `PluginRegistry::transition` for centralized validation.
fn advance_state(
    inst: &Arc<oxcost_core::PluginInstance>,
    f: impl FnOnce(
        oxcost_core::PluginState,
    ) -> Result<oxcost_core::PluginState, oxcost_core::InvalidTransition>,
) {
    let handle = inst.state_handle();
    let mut guard = handle.lock().expect("state mutex poisoned");
    if let Ok(next) = f(*guard) {
        *guard = next;
    }
}

/// One GET probe: 2xx = healthy.
async fn probe(url: &str) -> bool {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .expect("probe client builds");
    match client.get(url).send().await {
        Ok(resp) => resp.status().is_success(),
        Err(_) => false,
    }
}

/// Serve until shutdown. Binds `config.listen`.
pub async fn serve(state: Arc<AppState>) -> std::io::Result<()> {
    let app = app_router(state.clone());
    let listener = tokio::net::TcpListener::bind(&state.config.listen).await?;
    tracing::info!("listening on {}", state.config.listen);
    axum::serve(listener, app).await
}

/// Content-type helper for tests.
#[cfg(test)]
pub(crate) fn is_json(headers: &HeaderMap) -> bool {
    headers
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.starts_with("application/json"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::header;

    #[test]
    fn is_json_checks_content_type() {
        let mut h = HeaderMap::new();
        assert!(!is_json(&h));
        h.insert(
            header::CONTENT_TYPE,
            header::HeaderValue::from_static("application/json"),
        );
        assert!(is_json(&h));
        h.insert(
            header::CONTENT_TYPE,
            header::HeaderValue::from_static("text/event-stream"),
        );
        assert!(!is_json(&h));
    }
}
