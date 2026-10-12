//! `MimoPlugin` — the mimo2api port as an in-process `OxcostPlugin`
//! (contract §10): chat (+models) against the MiMo AI Studio web
//! backend, driving [`ChatPipeline`].
//!
//! Manifest: `dev.oxcost.mimo-rust`, serving the three live-proven
//! models under the `mimo-rust/` prefix (the host strips the prefix
//! before the body reaches `chat()`, contract §5 model routing). v1
//! capabilities: chat only — tts/asr/voiceclone/image input are
//! UNIMPLEMENTED-reserved; the SDK's reserved hooks stay default so a
//! mis-routed request fails closed.

use oxcost_contract::capability::{Capability, CapabilitySet};
use oxcost_contract::manifest::UnknownModelPolicy;
use oxcost_sdk::plugin::ChatFuture;
use oxcost_sdk::types::{ChatOutcome, ChatRequest, HealthStatus, PluginError, RequestContext};
use oxcost_sdk::{ModelDescriptor, OxcostPlugin, SdkManifest};

use crate::chat::{ChatAnswer, ChatPipeline};
use crate::config::Config;
use crate::errmap::MappedError;
use crate::models::SERVED_MODELS;
use crate::pool::Pool;
use crate::seed;
use crate::store::{self, Store};

/// The in-process mimo2api plugin.
pub struct MimoPlugin {
    manifest: SdkManifest,
    pipeline: ChatPipeline,
    /// Whether the store holds any account row at construction — an
    /// empty store is a deployment configuration error the host should
    /// see in health; a full-but-disabled pool still answers 429/503
    /// cleanly (cooldowns may expire).
    has_accounts: bool,
}

impl MimoPlugin {
    /// Construct from a resolved [`Config`]: open the store, seed from
    /// env, build the pool, wire the login client.
    pub fn new(cfg: Config) -> Result<Self, String> {
        let store = Store::open(&cfg.db_path).map_err(|e| format!("open store: {e}"))?;
        // Seed accounts from MIMO_ACCOUNT_n_* (the Go env seeding names —
        // the deployment contract already uses them; the MIMO_RUST_*
        // namespace is for gateway knobs, not credentials).
        let seeded = store::seed_from_env(&store, &|k| std::env::var(k).ok())
            .map_err(|e| format!("seed accounts: {e}"))?;
        // Session-bearing rows (R1-W2a live seeding): MIMO_RUST_SESSION_n_*
        // carries the Upstash row's cookie/ph/pass_token/user_id/device
        // onto the seeded accounts. No vars → no-op.
        seed::seed_session_from_env(&store, &|k| std::env::var(k).ok())
            .map_err(|e| format!("seed sessions: {e}"))?;
        let rows = store
            .load_accounts()
            .map_err(|e| format!("load accounts: {e}"))?;
        let records = store::to_pool_records(&rows);
        let login = crate::login::Client::new(cfg.login).map_err(|e| format!("client: {e}"))?;
        let pool = Pool::new(&records, cfg.pool, None, None);
        Ok(Self {
            manifest: Self::build_manifest(),
            pipeline: ChatPipeline {
                pool,
                store,
                login,
                thinking_default_on: cfg.thinking_default_on,
                default_model: cfg.default_model,
            },
            has_accounts: !seeded.is_empty() || !rows.is_empty(),
        })
    }

    fn build_manifest() -> SdkManifest {
        let models: Vec<String> = SERVED_MODELS
            .iter()
            .map(|m| format!("mimo-rust/{m}"))
            .collect();
        SdkManifest::new("dev.oxcost.mimo-rust")
            .version(env!("CARGO_PKG_VERSION"))
            .models(models)
            .capabilities(CapabilitySet::from_caps([Capability::Chat]))
            // Undeclared ids: the floor catalog resolves several
            // non-advertised ids (superseded models etc.) — passing them
            // through is the Go behavior (normalize against the floor).
            .unknown_model(UnknownModelPolicy::Pass)
            .enabled(true)
    }

    /// The chat pipeline (tests drive it directly).
    pub fn pipeline(&self) -> &ChatPipeline {
        &self.pipeline
    }
}

/// MappedError → PluginError (the host relays it verbatim onto the §4
/// body). Retry-After: `PluginError` has no header field (M3 open
/// question #2 — see the deliverable); where Go attaches a Retry-After
/// the mapped message/code already carries the actionable hint.
impl From<&MappedError> for PluginError {
    fn from(e: &MappedError) -> Self {
        PluginError {
            status: e.status,
            err_type: e.body.error.typ.clone(),
            message: e.body.error.message.clone(),
            param: e.body.error.param.clone(),
            code: e.body.error.code.clone(),
        }
    }
}

impl OxcostPlugin for MimoPlugin {
    fn manifest(&self) -> &SdkManifest {
        &self.manifest
    }

    fn models(&self) -> Vec<ModelDescriptor> {
        // `mimo-rust/<id>` prefixed, owned by the plugin display name —
        // consistent with manifest().models (the registry checks).
        self.manifest
            .models
            .iter()
            .map(|m| ModelDescriptor::owned(m, "mimo-rust"))
            .collect()
    }

    fn chat(&self, req: ChatRequest, _ctx: RequestContext) -> ChatFuture<'_> {
        Box::pin(async move {
            // The body arrives prefix-stripped (contract §5); the
            // pipeline resolves "auto"/aliases and echoes what it
            // resolved or what the client named.
            let body = req.body().clone();
            match self.pipeline.handle(&body).await {
                Ok(ChatAnswer::Completion(v)) => Ok(ChatOutcome::Json(v)),
                Ok(ChatAnswer::Sse(bytes)) => Ok(ChatOutcome::Sse(bytes)),
                Err(e) => Err(PluginError::from(&e)),
            }
        })
    }

    fn health(&self) -> HealthStatus {
        // Ok while the pool can serve or recover; an empty store means
        // the deployment forgot to seed accounts — that is Unhealthy so
        // the host surfaces it at probe time (contract §6).
        if !self.has_accounts {
            HealthStatus::Unhealthy
        } else {
            HealthStatus::Ok
        }
    }
}

#[cfg(test)]
#[path = "plugin_tests.rs"]
mod tests;
