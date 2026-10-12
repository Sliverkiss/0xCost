//! The `OxcostPlugin` trait — the ENTIRE surface a plugin author touches
//! (contract §10 host-sdk form).

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use crate::types::{ChatOutcome, ChatRequest, HealthStatus, PluginError, RequestContext};
use crate::{ModelDescriptor, SdkManifest};

/// One in-process plugin: compiled into the host, first-class citizen
/// (contract §10).
///
/// The contract is single — four forms, one set of semantics. Everything a
/// sidecar expresses over HTTP (plugin.toml, `/v1/models`,
/// `/v1/chat/completions`, `/healthz`, §4 error mapping) an SDK plugin
/// expresses here in code. The `插件名/模型名` prefix rule and the
/// capability whitelist apply IDENTICALLY (validated in
/// [`SdkManifest::validate`]).
///
/// Capability gating: `chat` is the only capability with a required method
/// in M3. `web_search`/`audio`/... are reserved default methods
/// (unsupported by default); implementing plugins override them only for
/// capabilities their [`SdkManifest`] declares.
pub trait OxcostPlugin: Send + Sync {
    /// The code-declared manifest (id, name, version, capabilities).
    /// Same semantics as plugin.toml §3; validated once at registration.
    fn manifest(&self) -> &SdkManifest;

    /// Model descriptors this plugin serves (the `/v1/models` view for its
    /// namespace). Must stay consistent with `manifest().models` — the
    /// registry checks the ids on registration.
    fn models(&self) -> Vec<ModelDescriptor>;

    /// Handle one chat completion request (streaming or not — see
    /// [`ChatOutcome`]). Errors map onto the §4 OpenAI error body.
    fn chat(&self, req: ChatRequest, ctx: RequestContext) -> ChatFuture<'_>;

    /// Health check (the `/healthz` equivalent). The host probes this at
    /// startup and before re-enabling (§6 register→probe→serving).
    fn health(&self) -> HealthStatus;

    // ---- capability-gated reserved hooks (default: unsupported) ----
    //
    // NOT implemented in M3 (task brief: reserved hooks only). Each maps to
    // one §4 capability group; when introduced, the default body returns
    // the §4 "capability not declared" error so a mis-routed request fails
    // closed:
    //
    // fn web_search(...) -> ... { unsupported }
    // fn audio_speech(...) -> ... { unsupported }
    // fn embeddings(...) -> ... { unsupported }
}

/// Boxed async chat future returned by [`OxcostPlugin::chat`].
pub type ChatFuture<'a> =
    Pin<Box<dyn Future<Output = Result<ChatOutcome, PluginError>> + Send + 'a>>;

/// Shared plugin handle used across the host (registry, routing, server).
pub type SharedPlugin = Arc<dyn OxcostPlugin>;
