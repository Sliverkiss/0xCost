//! OxCost host binary (M2): CLI, manifest loading, env auth resolution,
//! startup probing, and axum serving of the contract §5 external surface.
//! Assembly only — all logic lives in the library modules.

use std::collections::BTreeMap;
use std::sync::Arc;

use oxcost::loader::{load_plugins, print_registration_table, Cli, LoadOutcome};
use oxcost::server;
use oxcost::state::{AppState, FailedPlugin, HostConfig};
use oxcost_core::{KeySource, PluginInstance, PluginRegistry};

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
        print_registration_table(&registry);

        // Build runtime instances for successfully registered plugins,
        // resolving env: auth references (failures stay out of routing).
        let mut instances = BTreeMap::new();
        let mut failed = Vec::new();
        for entry in registry.list() {
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
