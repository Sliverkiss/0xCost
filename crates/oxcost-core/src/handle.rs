//! `PluginHandle`: the unification seam between the two transport forms.
//!
//! The host's routing/auth/aggregation/error-mapping layers are
//! form-agnostic: everything above this enum works on `PluginInstance`
//! fields (capabilities, model list, lifecycle state); everything below it
//! is one of these two dispatch arms:
//!
//! - [`PluginHandle::Sidecar`] — the M2 path: HTTP call to a plugin
//!   endpoint (external/remote/host-spawn forms; §4 sidecar protocol).
//! - [`PluginHandle::Sdk`] — the M3 path: direct in-process call on the
//!   [`OxcostPlugin`] trait (host-sdk form; §10).
//!
//! A third arm for dynamic loading (dylib/WASM, contract §8 roadmap) would
//! slot in here without touching the layers above.

use oxcost_sdk::SharedPlugin;

/// How the host reaches one plugin's chat surface.
pub enum PluginHandle {
    /// Out-of-process plugin: HTTP against `endpoint` with `key_source`
    /// auth (contract §4 sidecar protocol).
    Sidecar {
        /// Base URL, e.g. `http://127.0.0.1:21223` (no trailing slash).
        endpoint: String,
        /// Resolved auth key source for the outbound Authorization header.
        key_source: crate::KeySource,
    },
    /// In-process plugin (contract §10): direct trait call, no HTTP hop.
    Sdk(SharedPlugin),
}

impl std::fmt::Debug for PluginHandle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PluginHandle::Sidecar { endpoint, .. } => f
                .debug_struct("Sidecar")
                .field("endpoint", endpoint)
                .finish_non_exhaustive(),
            // The trait object is not Debug; the id is already shown by the
            // owning PluginInstance.
            PluginHandle::Sdk(_) => f.write_str("Sdk(..)"),
        }
    }
}
