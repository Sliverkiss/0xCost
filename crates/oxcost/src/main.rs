//! OxCost host binary (M2/M3): CLI, manifest loading, env auth resolution,
//! SDK plugin registration, startup probing, and axum serving of the
//! contract §5 external surface. Assembly only — all logic lives in the
//! library modules.

use std::collections::BTreeMap;
use std::sync::Arc;

use oxcost::config::HostConfigFile;
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
///
/// TokenHarbor (the first real SDK plugin) registers when
/// `TH_ACCOUNT_1_EMAIL`/`TH_ACCOUNT_1_PASSWORD` are present: its account
/// store is `TH_STORE_PATH` (default `data/tokenharbor.db` under the
/// working dir, plugin-private per contract §3). Accounts seed from
/// `TH_ACCOUNT_N_*` env pairs; quarantined accounts are marked and skipped
/// — a quarantined or missing account never fails host startup, it shows
/// up in /healthz instead.
async fn register_sdk_plugins() -> Vec<Arc<dyn oxcost_sdk::OxcostPlugin>> {
    let mut registry = oxcost_sdk::Registry::new();
    let sample = Arc::new(oxcost_sdk::SampleEcho::new(&[]));
    if let Err(e) = registry.register(Arc::clone(&sample) as _) {
        tracing::error!("sample-echo rejected: {e}");
    }

    if std::env::var("TH_ACCOUNT_1_EMAIL").is_ok() && std::env::var("TH_ACCOUNT_1_PASSWORD").is_ok()
    {
        let path = std::path::PathBuf::from(
            std::env::var("TH_STORE_PATH").unwrap_or_else(|_| "data/tokenharbor.db".into()),
        );
        if let Some(parent) = path.parent() {
            if std::fs::create_dir_all(parent).is_err() {
                tracing::error!("tokenharbor: cannot create store dir {}", parent.display());
            }
        }
        let store = match oxcost_plugin_tokenharbor::store::Store::open(&path) {
            Ok(store) => Arc::new(store),
            Err(e) => {
                tracing::error!("tokenharbor: store open failed: {e}");
                return registry.drain();
            }
        };
        let seeded = oxcost_plugin_tokenharbor::TokenHarbor::seed_accounts_from_env(&store);
        if seeded.is_empty() {
            tracing::warn!("tokenharbor: no accounts seeded — skipping registration");
        } else {
            let client = Arc::new(oxcost_plugin_tokenharbor::client::Client::new_live());
            let auth_base = oxcost_plugin_tokenharbor::auth::AUTH_BASE.to_string();
            match oxcost_plugin_tokenharbor::TokenHarbor::connect(store, client, auth_base).await {
                Ok(plugin) => {
                    if let Err(e) = registry.register(Arc::new(plugin) as _) {
                        tracing::error!("tokenharbor rejected: {e}");
                    } else {
                        tracing::info!("tokenharbor registered (accounts: {seeded:?})");
                    }
                }
                Err(e) => tracing::error!("tokenharbor construct failed: {e}"),
            }
        }
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

    // Host config file (R1W1 A2): read once at startup; a restart picks
    // up changes (hot reload deliberately out of scope). No --config →
    // the env-only defaults below (back-compat).
    let file_config = match &cli.config_path {
        Some(path) => match HostConfigFile::load(path) {
            Ok(cfg) => Some(cfg),
            Err(e) => {
                eprintln!("oxcost: {e}");
                std::process::exit(2);
            }
        },
        None => None,
    };

    let mut config = HostConfig::dev_default();
    if let Some(cfg) = &file_config {
        if let Some(bind) = &cfg.server.bind {
            config.listen = bind.clone();
        }
        let keys = cfg.resolve_api_keys();
        if !keys.is_empty() {
            config.api_keys = keys;
        }
    }
    // Env still wins over the file for listen/api_keys: the file is the
    // deployment default, the env the operator override. When neither is
    // set the dev defaults hold.
    if let Ok(listen) = std::env::var("OXCOST_LISTEN") {
        if !listen.is_empty() {
            config.listen = listen;
        }
    }
    // `--listen` (explicit) > env OXCOST_LISTEN > config file bind >
    // dev default. cli.listen stays the dev default when unset, so
    // compare against it to detect an explicit flag.
    if cli.listen != "127.0.0.1:8080" {
        config.listen = cli.listen;
    }
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
        let sdk_plugins = register_sdk_plugins().await;
        for plugin in &sdk_plugins {
            let manifest = plugin.manifest().to_contract_manifest();
            if let Err(e) = registry.insert(manifest) {
                eprintln!(
                    "oxcost: rejected SDK plugin `{}`: {e}",
                    plugin.manifest().id
                );
            }
        }

        // Apply config-file enabled overrides (A2): applied to the registry
        // manifests after validation so the override cannot smuggle an
        // invalid manifest in, and before instance construction so the
        // §6 "off = invisible" semantics (no routing, no catalog, no
        // probing) flow through the normal paths. SDK instances need the
        // same override applied post-construction (their manifest is a
        // conversion, not a reference into the registry).
        if let Some(cfg) = &file_config {
            for entry in registry.list_mut() {
                cfg.apply_overrides(&mut entry.manifest);
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
                let mut inst = PluginInstance::from_sdk_plugin(Arc::clone(plugin));
                // The SDK instance holds its own converted manifest copy —
                // apply the config override post-construction so this form
                // honors [plugins."<id>"].enabled too.
                if let Some(cfg) = &file_config {
                    inst.set_routing_enabled(
                        cfg.effective_enabled(inst.id(), inst.routing_enabled()),
                    );
                }
                instances.insert(entry.manifest.id.clone(), Arc::new(inst));
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
