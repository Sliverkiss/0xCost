//! Shared server state: plugin registry, live instances, host auth config.

use std::collections::BTreeMap;
use std::sync::{Arc, RwLock};

use oxcost_core::{PluginInstance, RouteDecision};

/// Host-level configuration resolved from env + CLI.
#[derive(Debug, Clone)]
pub struct HostConfig {
    /// Bearer keys accepted from clients (from `OXCOST_API_KEYS`,
    /// comma-separated). Empty = dev mode: allow all.
    pub api_keys: Vec<String>,
    /// Listen address.
    pub listen: String,
    /// Per-request forwarding timeout (contract §5: 300s).
    pub request_timeout_secs: u64,
}

impl HostConfig {
    /// Dev-mode default: no keys, loopback, 8080.
    pub fn dev_default() -> Self {
        Self {
            api_keys: Vec::new(),
            listen: "127.0.0.1:8080".into(),
            request_timeout_secs: 300,
        }
    }

    /// Is a client bearer token accepted? Dev mode (no keys) accepts all.
    pub fn accepts_key(&self, token: &str) -> bool {
        self.api_keys.is_empty() || self.api_keys.iter().any(|k| k == token)
    }
}

/// A plugin that failed registration (env auth resolution etc.).
/// Kept out of routing entirely; surfaced in /healthz (§3).
#[derive(Debug, Clone, PartialEq)]
pub struct FailedPlugin {
    /// Plugin directory / id.
    pub plugin: String,
    /// Why registration failed.
    pub reason: String,
}

/// The axum-shared application state.
pub struct AppState {
    /// Live runtime views, keyed by plugin id.
    pub instances: RwLock<BTreeMap<String, Arc<PluginInstance>>>,
    /// Host config (auth keys, listen, timeouts).
    pub config: HostConfig,
    /// Load-time registration failures surfaced in /healthz.
    pub failed: Vec<FailedPlugin>,
}

impl AppState {
    /// Assemble from load results. The registry itself stays in `main`
    /// (lifecycle transitions are driven by the instance state handles);
    /// the state keeps the derived instance map.
    pub fn new(
        instances: BTreeMap<String, Arc<PluginInstance>>,
        config: HostConfig,
        failed: Vec<FailedPlugin>,
    ) -> Self {
        Self {
            instances: RwLock::new(instances),
            config,
            failed,
        }
    }

    /// Snapshot of live instances (id order).
    pub fn instance_list(&self) -> Vec<Arc<PluginInstance>> {
        self.instances
            .read()
            .expect("instances lock poisoned")
            .values()
            .cloned()
            .collect()
    }

    /// Find the servable instance willing to serve `model`. First match
    /// wins; multi-plugin load balancing arrives in M3 — the LB seam is
    /// this method plus `forward::pick_target` (see forward.rs notes).
    pub fn find_owner(&self, model: &str) -> Option<Arc<PluginInstance>> {
        self.instance_list().into_iter().find(|inst| {
            inst.is_servable() && matches!(inst.route(model), RouteDecision::Forward { .. })
        })
    }
}
