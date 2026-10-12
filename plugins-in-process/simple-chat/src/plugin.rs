//! The `OxcostPlugin` facade — the single entry point the host sees
//! (contract §10 host-sdk form). Wires the pool, the store, the chat
//! ladder ([`crate::chat_flow`]), and the §6 error mapper into the four
//! trait methods.
//!
//! Manifest: `dev.oxcost.simple-chat`, serving `simple-chat/deepseek-flash`
//! (single model, like Go). Capabilities: `chat` only — see OQ-1 in the
//! README (web search rides `chat` via the `search` switch + citations
//! passthrough; the standalone `web_search` SDK hook is reserved in M3).
//!
//! OQ-1/OQ-2 (M3 open questions), answered:
//! - #1 streaming variant: the buffer-based `ChatOutcome::Sse` preserves
//!   the Go retry/commit boundary (no client bytes before the buffer
//!   returns). A chunked variant would need a commit callback — deferred
//!   by the SDK, not worked around.
//! - #2 Retry-After: `PluginError` has no header field, so the §6
//!   Retry-After side-channel lives on [`crate::errmap::WireError`],
//!   carried internally to the facade boundary. The SDK surface relays
//!   the error verbatim; the header projection is a host-side follow-up.

use std::sync::Arc;
use std::time::Duration;

use oxcost_sdk::plugin::ChatFuture;
use oxcost_sdk::types::{ChatRequest, HealthStatus, RequestContext};
use oxcost_sdk::{ModelDescriptor, OxcostPlugin, SdkManifest};

use crate::chat_flow;
use crate::pool::{Pool, PoolConfig, PoolError};
use crate::store::SqliteStore;

/// Env: SQLite path (plugin-private account + park store).
pub const ENV_DB_PATH: &str = "SC_DB_PATH";
/// Env: prompt character cap (0 = default 2,000,000; negative disables).
pub const ENV_MAX_PROMPT_CHARS: &str = "SC_MAX_PROMPT_CHARS";
/// Env: per-account concurrent request cap (default 2).
pub const ENV_MAX_INFLIGHT: &str = "SC_MAX_INFLIGHT";
/// Env: queue wait in seconds when all accounts are busy (default 30).
pub const ENV_QUEUE_WAIT_SECS: &str = "SC_QUEUE_WAIT_SECS";

/// Construction failure (pool build or store open).
#[derive(Debug, thiserror::Error)]
pub enum PluginBuildError {
    #[error("store: {0}")]
    Store(#[from] crate::store::StoreError),
    #[error("pool: {0}")]
    Pool(#[from] PoolError),
}

/// The plugin. Holds the pool (all account state) and the prompt cap.
pub struct SimpleChatPlugin {
    manifest: SdkManifest,
    pool: Arc<Pool>,
    max_prompt_chars: usize,
}

/// Builder-facing configuration (env-resolved defaults; tests inject
/// overrides). Accounts arrive from the seed CLI's store (PC-41 import)
/// or are code-provided.
#[derive(Debug, Clone, Default)]
pub struct SimpleChatConfig {
    /// SQLite path; `None` = memory-only (tests) — production always
    /// persists (R3).
    pub db_path: Option<std::path::PathBuf>,
    /// Account credentials.
    pub accounts: Vec<crate::account::Account>,
    /// Prompt cap; 0 = [`chat_flow::DEFAULT_MAX_PROMPT_CHARS`],
    /// `usize::MAX` = disabled (the `SC_MAX_PROMPT_CHARS=-1` mapping).
    pub max_prompt_chars: usize,
    /// Per-account concurrency cap; 0 = pool default (2).
    pub max_inflight: usize,
    /// Queue wait when all accounts are busy; zero = 30s.
    pub queue_wait: Duration,
    /// Upstream base override (tests inject a local URL; empty =
    /// production `chat.deepseek.com`).
    pub base_url: String,
    /// Deterministic selection seed (tests).
    pub random_seed: u64,
}

impl SimpleChatConfig {
    /// Resolve env vars over the defaults (empty/invalid env = default).
    pub fn from_env() -> Self {
        // -1 disables the guard (usize::MAX sentinel); anything else
        // non-negative parses, 0 keeping the default.
        let max_prompt_chars = std::env::var(ENV_MAX_PROMPT_CHARS)
            .ok()
            .and_then(|v| v.trim().parse::<isize>().ok())
            .map(|v| if v < 0 { usize::MAX } else { v as usize })
            .unwrap_or(0);
        let max_inflight = std::env::var(ENV_MAX_INFLIGHT)
            .ok()
            .and_then(|v| v.trim().parse().ok())
            .filter(|&v: &usize| v > 0)
            .unwrap_or(0);
        let queue_wait = std::env::var(ENV_QUEUE_WAIT_SECS)
            .ok()
            .and_then(|v| v.trim().parse().ok())
            .filter(|&v: &u64| v > 0)
            .map(Duration::from_secs)
            .unwrap_or(Duration::ZERO);
        let db_path = std::env::var(ENV_DB_PATH)
            .ok()
            .map(std::path::PathBuf::from);
        SimpleChatConfig {
            db_path,
            max_prompt_chars,
            max_inflight,
            queue_wait,
            ..Default::default()
        }
    }
}

impl SimpleChatPlugin {
    /// Build from explicit config (accounts + overrides).
    pub fn new(cfg: SimpleChatConfig) -> Result<Self, PluginBuildError> {
        let store = match &cfg.db_path {
            Some(p) => Some(Arc::new(SqliteStore::open(p)?)),
            None => None,
        };
        let pool = Pool::new(
            cfg.accounts.clone(),
            PoolConfig {
                store,
                base_url: if cfg.base_url.is_empty() {
                    crate::wire::DEFAULT_BASE_URL.to_string()
                } else {
                    cfg.base_url
                },
                max_inflight: if cfg.max_inflight == 0 {
                    crate::pool::DEFAULT_MAX_INFLIGHT
                } else {
                    cfg.max_inflight
                },
                queue_wait: if cfg.queue_wait.is_zero() {
                    Duration::from_secs(30)
                } else {
                    cfg.queue_wait
                },
                random_seed: cfg.random_seed,
                ..Default::default()
            },
        )?;
        let max_prompt_chars = if cfg.max_prompt_chars == 0 {
            chat_flow::DEFAULT_MAX_PROMPT_CHARS
        } else if cfg.max_prompt_chars == usize::MAX {
            0 // chat_flow's 0 = "no cap"
        } else {
            cfg.max_prompt_chars
        };
        Ok(Self {
            manifest: Self::build_manifest(),
            pool,
            max_prompt_chars,
        })
    }

    fn build_manifest() -> SdkManifest {
        SdkManifest::new("dev.oxcost.simple-chat")
            .version(env!("CARGO_PKG_VERSION"))
            .models(vec!["simple-chat/deepseek-flash".into()])
    }

    /// The pool handle (health surface, tests).
    pub fn pool(&self) -> &Arc<Pool> {
        &self.pool
    }

    /// The effective prompt cap (diagnostics; 0 = disabled).
    pub fn max_prompt_chars(&self) -> usize {
        self.max_prompt_chars
    }
}

impl OxcostPlugin for SimpleChatPlugin {
    fn manifest(&self) -> &SdkManifest {
        &self.manifest
    }

    fn models(&self) -> Vec<ModelDescriptor> {
        // One entry; `owned_by` mirrors Go's /v1/models.
        self.manifest
            .models
            .iter()
            .map(|m| ModelDescriptor::owned(m, "simple-chat"))
            .collect()
    }

    fn chat(&self, req: ChatRequest, _ctx: RequestContext) -> ChatFuture<'_> {
        let pool = Arc::clone(&self.pool);
        let max_prompt = self.max_prompt_chars;
        Box::pin(async move {
            // The §4 400 family: parse/cap errors map through the same
            // table as Go's handler head.
            let mut job = match chat_flow::prepare(req.body(), max_prompt) {
                Ok(j) => j,
                Err(we) => return Err(we.error),
            };
            if let Err(we) = chat_flow::resolve_images(&mut job).await {
                return Err(we.error);
            }
            match chat_flow::run_chat(&pool, job).await {
                Ok(out) => Ok(out),
                // WireError → PluginError at the trait boundary; the
                // Retry-After side-channel stays internal (OQ #2 — the
                // host projects it when the SDK grows a header field).
                Err(we) => {
                    if let Some(ra) = we.retry_after {
                        tracing::debug!(
                            retry_after = ra,
                            code = ?we.error.code,
                            "retry-after answer"
                        );
                    }
                    Err(we.error)
                }
            }
        })
    }

    fn health(&self) -> HealthStatus {
        // Ready gate: the ring must have at least one ready account. The
        // R3 park latch surfaces through the snapshot too (a latched pool
        // answers ParkPersistence on acquire; health reflects it as
        // no-ready-accounts once states go non-ready), and an empty ring
        // is a fresh deployment — probe fails, host keeps it registered
        // but unrouted.
        let snap = self.pool.snapshot();
        if snap.iter().any(|s| s.state == "ready") {
            HealthStatus::Ok
        } else {
            HealthStatus::Unhealthy
        }
    }
}

#[cfg(test)]
mod tests;
