//! OxCost host binary (M2/M3): CLI, manifest loading, env auth resolution,
//! SDK plugin registration, startup probing, and axum serving of the
//! contract §5 external surface. Assembly only — all logic lives in the
//! library modules.

use std::collections::BTreeMap;
use std::sync::Arc;

use oxcost::loader::{load_plugins, print_registration_table, Cli, LoadOutcome};
use oxcost::server;
use oxcost::state::{AppState, FailedPlugin, HostConfig};
use oxcost_core::{KeySource, PluginInstance, PluginRegistry};

/// Register the built-in SDK plugins (contract §10). Explicit, called from
/// main — no inventory/linkme collection magic.
///
/// The sample-echo fixture is **test scaffolding, not a feature** (contract
/// §11 synthetic-fixture policy): it is registered unconditionally so the
/// SDK path is exercisable offline and visible in the registration table.
/// The three real SDK plugins (TokenHarbor, mimo-rust, simple-chat) register
/// here as they land.
fn register_sdk_plugins() -> Vec<Arc<dyn oxcost_sdk::OxcostPlugin>> {
    let mut registry = oxcost_sdk::Registry::new();
    let sample = Arc::new(oxcost_sdk::SampleEcho::new(&[]));
    if let Err(e) = registry.register(Arc::clone(&sample) as _) {
        tracing::error!("sample-echo rejected: {e}");
    }
    registry.drain()
}

fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let args: Vec<String> = std::env::args().collect();
    let cli = match Cli::parse(&args) {
        Ok(cli) => cli,
        Err(msg) => {
            eprintln!("oxcost: {msg}");
            std::process::exit(2);
        }
    };

    let mut config = HostConfig::dev_default();
    config.listen = cli.listen;
    if let Ok(keys) = std::env::var("OXCOST_API_KEYS") {
        config.api_keys = keys
            .split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
            .collect();
    }
    if config.api_keys.is_empty() {
        tracing::warn!("OXCOST_API_KEYS unset — dev mode: loopback bind, all requests allowed");
    }

    let runtime = tokio::runtime::Runtime::new().expect("tokio runtime");
    runtime.block_on(async move {
        let mut registry = PluginRegistry::new();
        let outcomes = load_plugins(&mut registry, &cli.plugins_dir);
        for outcome in &outcomes {
            if let LoadOutcome::Rejected { plugin, reason } = outcome {
                eprintln!("oxcost: rejected plugin `{plugin}`: {reason}");
            }
        }

        // SDK plugins (§10): validated by the SDK registry, then folded
        // into the host registry as contract-shape manifests so the
        // lifecycle/registration table treats both forms uniformly.
        let sdk_plugins = register_sdk_plugins();
        for plugin in &sdk_plugins {
            let manifest = plugin.manifest().to_contract_manifest();
            if let Err(e) = registry.insert(manifest) {
                eprintln!(
                    "oxcost: rejected SDK plugin `{}`: {e}",
                    plugin.manifest().id
                );
            }
        }
        print_registration_table(&registry);

        // Build runtime instances for successfully registered plugins:
        // sidecars resolve env: auth references, SDK plugins dispatch
        // in-process. Failures stay out of routing and surface in /healthz.
        let mut instances = BTreeMap::new();
        let mut failed = Vec::new();
        for entry in registry.list() {
            let sdk_match = sdk_plugins
                .iter()
                .find(|p| p.manifest().id == entry.manifest.id);
            if let Some(plugin) = sdk_match {
                let id = entry.manifest.id.clone();
                instances.insert(
                    id,
                    Arc::new(PluginInstance::from_sdk_plugin(Arc::clone(plugin))),
                );
                continue;
            }
            match oxcost_contract::resolve_auth(&entry.manifest, |name: &str| {
                std::env::var(name).ok()
            }) {
                Ok(var) => {
                    let id = entry.manifest.id.clone();
                    let inst = Arc::new(PluginInstance::new(
                        entry.manifest.clone(),
                        KeySource::Env(var),
                    ));
                    instances.insert(id, inst);
                }
                Err(e) => failed.push(FailedPlugin {
                    plugin: entry.manifest.id.clone(),
                    reason: e.to_string(),
                }),
            }
        }

        let state = Arc::new(AppState::new(instances, config, failed));

        // Check mode: load + validate + print, then exit (no probe, no serve).
        if cli.check {
            return;
        }

        // Startup health probe: Registered → Probing → Serving (§6).
        server::probe_all(&state).await;

        if let Err(e) = server::serve(state).await {
            eprintln!("oxcost: server error: {e}");
            std::process::exit(1);
        }
    });
}
