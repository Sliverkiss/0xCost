//! `tokenharbor`: the FIRST real OxcostPlugin (contract §10 host-sdk form)
//! — tokenharbor.ai web chat behind the OpenAI chat-completions protocol,
//! compiled into the host process.
//!
//! Module map:
//! - [`auth`] — Supabase login / single-use refresh rotation / cookie
//! - [`client`] — paced, browser-shaped HTTP for tokenharbor.ai
//! - [`models`] — `/models` RSC parsing + free-tier policy (no hardcoded ids)
//! - [`chat`] — consent gate → temp session → SSE → OpenAI mapping + §4 errors
//! - [`sse`] — upstream frame parser
//! - [`store`] — plugin-private SQLite (accounts, catalog, free-tier)
//!
//! v1 scope: chat only (no tools per maintainer; web_search reserved,
//! default-unsupported). One account suffices; quarantined accounts are
//! marked, skipped, surfaced in health — never crash the plugin.

pub mod auth;
pub mod chat;
pub mod client;
pub mod models;
pub mod sse;
pub mod store;

use std::sync::Arc;

use oxcost_contract::UnknownModelPolicy;
use oxcost_sdk::{ChatRequest, HealthStatus, ModelDescriptor, OxcostPlugin, SdkManifest};

use crate::auth::Auth;
use crate::chat::ChatFlow;
use crate::client::Client;
use crate::store::Store;

/// Reverse-DNS plugin id (§3).
pub const PLUGIN_ID: &str = "dev.oxcost.tokenharbor";
/// Catalog refresh TTL (task brief: cache with TTL; 5 min matches the
/// free-window churn rate without hammering /models).
const CATALOG_TTL_SECS: i64 = 300;

/// The TokenHarbor plugin.
///
/// The manifest is a REGISTRATION-TIME declaration (SDK contract: validated
/// once at registration, host bakes it into its catalog at startup), so it
/// is built from whatever catalog the store holds at construction. The
/// production constructor ([`TokenHarbor::connect`]) refreshes the catalog
/// first; runtime refreshes ([`TokenHarbor::refresh_catalog`]) update the
/// store only — churn is absorbed at request time via
/// `unknown_model = Pass` + honest upstream `model_unavailable` errors.
pub struct TokenHarbor {
    manifest: SdkManifest,
    flow: Arc<ChatFlow>,
    /// Fetched-at of the catalog snapshot currently in the store.
    catalog_fetched_at: std::sync::atomic::AtomicI64,
}

impl TokenHarbor {
    /// Runtime constructor: an already-opened store + client + auth base
    /// (testable — the mock suite passes a local base URL).
    ///
    /// The manifest baseline is built from the CURRENT cached catalog so
    /// the §3 declaration is real (目录真实 — declared ids exist upstream);
    /// an empty cache falls back to a placeholder id and health stays the
    /// source of truth.
    pub fn new(
        store: Arc<Store>,
        client: Arc<Client>,
        auth_base: String,
    ) -> Result<Self, oxcost_sdk::PluginError> {
        let snapshot = store.catalog().map_err(internal)?;
        let http = auth_http_client();
        let auth = Auth::with_base(Arc::clone(&store), http, auth_base);
        let manifest = build_manifest(&snapshot);
        Ok(Self {
            manifest,
            flow: Arc::new(ChatFlow {
                store,
                client,
                auth: Arc::new(auth),
            }),
            catalog_fetched_at: std::sync::atomic::AtomicI64::new(snapshot.fetched_at),
        })
    }

    /// Production constructor: refresh the catalog FIRST (one anonymous
    /// `/models` fetch, stale-while-error on failure), then build the
    /// registration-time manifest from the freshest snapshot the store
    /// holds. Call from async context on the host startup path.
    pub async fn connect(
        store: Arc<Store>,
        client: Arc<Client>,
        auth_base: String,
    ) -> Result<Self, oxcost_sdk::PluginError> {
        let mut plugin = Self::new(store, client, auth_base)?;
        // Best-effort freshness: a failed fetch keeps the cached snapshot
        // (possibly placeholder manifest — health carries the real state).
        if let Err(e) = plugin.refresh_catalog().await {
            tracing::warn!(error = %e, "startup catalog refresh failed — using cache");
        }
        // Rebuild the declaration from whatever the store holds now.
        let snapshot = plugin.flow.store.catalog().map_err(internal)?;
        let manifest = build_manifest(&snapshot);
        plugin.manifest = manifest;
        Ok(plugin)
    }

    /// Seed accounts from `TH_ACCOUNT_N_EMAIL`/`TH_ACCOUNT_N_PASSWORD` env
    /// pairs (TEST accounts only — contract §3: credentials via env, never
    /// committed). Idempotent: re-seeding never wipes live tokens. Returns
    /// the emails actually seeded.
    pub fn seed_accounts_from_env(store: &Store) -> Vec<String> {
        let mut seeded = Vec::new();
        for n in 1..=10 {
            let email = std::env::var(format!("TH_ACCOUNT_{n}_EMAIL")).ok();
            let password = std::env::var(format!("TH_ACCOUNT_{n}_PASSWORD")).ok();
            if let (Some(email), Some(password)) = (email, password) {
                if email.is_empty() || password.is_empty() {
                    continue;
                }
                let row = store::AccountRow::seed(&email, &password);
                match store.upsert_account(&row) {
                    Ok(()) => seeded.push(email),
                    Err(e) => tracing::warn!(%email, error = %e, "account seed failed"),
                }
            }
        }
        seeded
    }

    /// Refresh the catalog snapshot (live `/models` RSC; anonymous works —
    /// dossier VERIFICATION.md §7) and persist it. On failure the caller
    /// keeps the cached snapshot (stale-while-error, §5 catalog rule).
    pub async fn refresh_catalog(
        &self,
    ) -> Result<store::CatalogSnapshot, crate::client::TransportError> {
        let resp = self.flow.client.get("/models", "").await?;
        if resp.status != 200 {
            return Err(crate::client::TransportError::Status {
                status: resp.status,
                detail: "models RSC fetch failed".into(),
            });
        }
        let text = resp.text();
        let snapshot = store::CatalogSnapshot {
            models: models::parse_rsc_snapshot(&text),
            fetched_at: chrono::Utc::now().timestamp(),
            server_now: models::parse_server_now(&text),
        };
        if let Err(e) = self.flow.store.save_catalog(&snapshot) {
            tracing::warn!(error = %e, "catalog persist failed (cache-only loss)");
        }
        self.catalog_fetched_at
            .store(snapshot.fetched_at, std::sync::atomic::Ordering::Relaxed);
        Ok(snapshot)
    }

    /// Servable models right now: public free rows (window-valid)
    /// intersected with the non-quarantined accounts' entitlements
    /// (union across accounts — a model ANY account can serve, we serve).
    /// With no entitlements fetched yet, the public free table stands in
    /// (advertise-and-let-upstream-decide beats hiding everything).
    fn servable_models(&self) -> Vec<store::ModelRow> {
        let Ok(snapshot) = self.flow.store.catalog() else {
            return Vec::new();
        };
        let accounts = self.flow.store.accounts().unwrap_or_default();
        let usable: Vec<&store::AccountRow> = accounts
            .iter()
            .filter(|a| a.quarantined_reason.is_none())
            .collect();

        let mut union: Option<std::collections::BTreeSet<String>> = None;
        for account in &usable {
            let tier = self.flow.store.free_tier(&account.email).ok().flatten();
            let Some(tier) = tier else {
                // No tier fetched yet → fall back to the public free table.
                return models::free_models_now(
                    &snapshot.models,
                    snapshot.server_now.as_deref(),
                    chrono::Utc::now(),
                );
            };
            let ids: std::collections::BTreeSet<String> = models::servable_free_models(
                &snapshot.models,
                snapshot.server_now.as_deref(),
                chrono::Utc::now(),
                &tier,
            )
            .into_iter()
            .map(|r| r.full_id)
            .collect();
            union = Some(match union {
                Some(u) => u.union(&ids).cloned().collect(),
                None => ids,
            });
        }
        if usable.is_empty() {
            return Vec::new();
        }
        // Preserve catalog row order for stable output.
        snapshot
            .models
            .into_iter()
            .filter(|row| union.as_ref().is_some_and(|u| u.contains(&row.full_id)))
            .collect()
    }

    /// Catalog age in seconds (i64::MAX when never fetched).
    pub fn catalog_age_secs(&self) -> i64 {
        let fetched = self
            .catalog_fetched_at
            .load(std::sync::atomic::Ordering::Relaxed);
        if fetched == 0 {
            i64::MAX
        } else {
            chrono::Utc::now().timestamp() - fetched
        }
    }

    /// Whether the catalog TTL has elapsed (host-side refresh hook).
    pub fn catalog_stale(&self) -> bool {
        self.catalog_age_secs() > CATALOG_TTL_SECS
    }
}

/// Build the §3 manifest from a catalog snapshot: `tokenharbor/`-prefixed
/// free model ids, chat-only, `unknown_model = pass` (the free table
/// churns weekly — an undeclared id is passed upstream and fails there
/// honestly rather than 404ing at the host).
fn build_manifest(snapshot: &store::CatalogSnapshot) -> SdkManifest {
    let mut ids: Vec<String> = models::free_models_now(
        &snapshot.models,
        snapshot.server_now.as_deref(),
        chrono::Utc::now(),
    )
    .into_iter()
    .map(|row| format!("tokenharbor/{}", row.full_id))
    .collect();
    if ids.is_empty() {
        // §3 requires a non-empty model list; a placeholder keeps the
        // plugin registrable when the catalog is empty (health carries the
        // real state, and refresh_catalog repairs the declaration).
        ids.push("tokenharbor/unresolved-catalog".into());
    }
    SdkManifest {
        unknown_model: UnknownModelPolicy::Pass,
        ..SdkManifest::new(PLUGIN_ID)
            .version(env!("CARGO_PKG_VERSION"))
            .models(ids)
    }
}

impl OxcostPlugin for TokenHarbor {
    fn manifest(&self) -> &SdkManifest {
        &self.manifest
    }

    fn models(&self) -> Vec<ModelDescriptor> {
        self.servable_models()
            .into_iter()
            .map(|row| {
                ModelDescriptor::owned(format!("tokenharbor/{}", row.full_id), "tokenharbor")
            })
            .collect()
    }

    fn chat(
        &self,
        req: ChatRequest,
        ctx: oxcost_sdk::RequestContext,
    ) -> oxcost_sdk::ChatFuture<'_> {
        let flow = Arc::clone(&self.flow);
        Box::pin(async move { flow.chat(&req, &ctx).await })
    }

    fn health(&self) -> HealthStatus {
        // Healthy iff at least one account is usable (non-quarantined).
        // Quarantined-only → Unhealthy: registered, visible in /healthz,
        // but the host will not route to us until an account recovers.
        match self.flow.store.accounts() {
            Ok(accounts) if accounts.iter().any(|a| a.quarantined_reason.is_none()) => {
                HealthStatus::Ok
            }
            _ => HealthStatus::Unhealthy,
        }
    }
}

/// Separate HTTP client for the auth host (Supabase is a real API — no
/// pacing needed, short timeouts suffice).
fn auth_http_client() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(std::time::Duration::from_secs(15))
        .read_timeout(std::time::Duration::from_secs(20))
        .build()
        .expect("auth client builds")
}

fn internal(e: store::StoreError) -> oxcost_sdk::PluginError {
    tracing::error!(error = %e, "plugin store failure");
    oxcost_sdk::PluginError::upstream("plugin internal storage failure")
}

#[cfg(test)]
mod tests {
    use super::*;
    use oxcost_sdk::ChatOutcome;

    const RSC: &str = include_str!("../fixtures/models-rsc-small.txt");

    fn offline_plugin() -> (Arc<Store>, TokenHarbor) {
        let store = Arc::new(Store::in_memory().unwrap());
        let plugin = TokenHarbor::new(
            Arc::clone(&store),
            Arc::new(Client::with_base(
                "http://127.0.0.1:1".into(),
                std::time::Duration::from_secs(1),
            )),
            "http://127.0.0.1:1".into(),
        )
        .unwrap();
        (store, plugin)
    }

    #[test]
    fn manifest_from_snapshot_is_chat_only_with_prefixed_free_ids() {
        let snapshot = store::CatalogSnapshot {
            models: models::parse_rsc_snapshot(RSC),
            fetched_at: 1,
            server_now: models::parse_server_now(RSC),
        };
        let manifest = build_manifest(&snapshot);
        assert!(manifest.validate().is_ok());
        assert_eq!(manifest.id, PLUGIN_ID);
        assert_eq!(manifest.name, "tokenharbor");
        assert_eq!(
            manifest.models,
            vec![
                "tokenharbor/anthropic/claude-haiku-5.5:free",
                "tokenharbor/vercel-ai-gateway/deepseek/deepseek-v4.1-flash:free",
                "tokenharbor/xiaomi/mimo-v2.6-flash:free",
            ]
        );
    }

    #[test]
    fn manifest_with_empty_catalog_uses_placeholder() {
        let manifest = build_manifest(&store::CatalogSnapshot::default());
        assert_eq!(manifest.models, vec!["tokenharbor/unresolved-catalog"]);
        assert!(manifest.validate().is_ok());
    }

    #[tokio::test]
    async fn health_quarantined_accounts_surface_unhealthy() {
        let (store, plugin) = offline_plugin();
        assert_eq!(
            plugin.health(),
            HealthStatus::Unhealthy,
            "no accounts at all"
        );

        store
            .upsert_account(&store::AccountRow::seed("q@example.com", "pw"))
            .unwrap();
        store
            .set_quarantine("q@example.com", Some("account_suspended"), 1)
            .unwrap();
        assert_eq!(plugin.health(), HealthStatus::Unhealthy);

        // A usable account flips it back to Ok.
        store
            .upsert_account(&store::AccountRow::seed("ok@example.com", "pw"))
            .unwrap();
        assert_eq!(plugin.health(), HealthStatus::Ok);
    }

    #[test]
    fn models_intersection_with_entitlements() {
        let (store, plugin) = offline_plugin();
        store
            .save_catalog(&store::CatalogSnapshot {
                models: models::parse_rsc_snapshot(RSC),
                fetched_at: 1,
                server_now: models::parse_server_now(RSC),
            })
            .unwrap();
        store
            .upsert_account(&store::AccountRow::seed("a@example.com", "pw"))
            .unwrap();
        store
            .save_free_tier(&store::FreeTier {
                email: "a@example.com".into(),
                model_ids: vec!["deepseek-v4.1-flash".into(), "mimo-v2.6-flash".into()],
                fetched_at: 1,
            })
            .unwrap();

        let ids: Vec<String> = plugin.models().into_iter().map(|m| m.id).collect();
        assert_eq!(
            ids,
            vec![
                "tokenharbor/vercel-ai-gateway/deepseek/deepseek-v4.1-flash:free",
                "tokenharbor/xiaomi/mimo-v2.6-flash:free",
            ]
        );
    }

    #[test]
    fn models_fall_back_to_public_table_without_tiers() {
        let (store, plugin) = offline_plugin();
        store
            .save_catalog(&store::CatalogSnapshot {
                models: models::parse_rsc_snapshot(RSC),
                fetched_at: 1,
                server_now: models::parse_server_now(RSC),
            })
            .unwrap();
        store
            .upsert_account(&store::AccountRow::seed("a@example.com", "pw"))
            .unwrap();
        // No free-tier row → public free table (3 rows on snapshot day).
        assert_eq!(plugin.models().len(), 3);
    }

    #[test]
    fn models_empty_with_no_usable_accounts() {
        let (store, plugin) = offline_plugin();
        store
            .save_catalog(&store::CatalogSnapshot {
                models: models::parse_rsc_snapshot(RSC),
                fetched_at: 1,
                server_now: models::parse_server_now(RSC),
            })
            .unwrap();
        store
            .upsert_account(&store::AccountRow::seed("q@example.com", "pw"))
            .unwrap();
        store
            .set_quarantine("q@example.com", Some("account_suspended"), 1)
            .unwrap();
        assert!(plugin.models().is_empty());
    }

    #[tokio::test]
    async fn chat_with_no_accounts_is_rate_limit_not_crash() {
        let (_, plugin) = offline_plugin();
        let req = ChatRequest::new(serde_json::json!({
            "model": "xiaomi/mimo-v2.6-flash:free",
            "messages": [{"role": "user", "content": "hi"}],
        }))
        .unwrap();
        let err = plugin
            .chat(req, oxcost_sdk::RequestContext::default())
            .await
            .unwrap_err();
        assert_eq!(err.status, 429);
        assert_eq!(err.err_type, "rate_limit_error");
    }

    #[tokio::test]
    async fn registry_accepts_the_plugin() {
        let (_, plugin) = offline_plugin();
        let mut reg = oxcost_sdk::Registry::new();
        reg.register(Arc::new(plugin)).expect("manifest validates");
        assert_eq!(reg.len(), 1);
        // ChatOutcome import used by other tests via same module.
        let _ = ChatOutcome::json(serde_json::json!({}));
    }
}
