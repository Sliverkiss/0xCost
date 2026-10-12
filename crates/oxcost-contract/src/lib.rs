//! plugin.toml manifest model, parsing, validation, version negotiation,
//! and capability model — the host/plugin contract layer (§3–§4 of
//! docs/PLUGIN_CONTRACT.md). Pure data + rules, no I/O.

pub mod capability;
pub mod manifest;
pub mod parse;
pub mod validate;
pub mod version;

pub use capability::{Capability, CapabilitySet, Tier};
pub use manifest::{
    AuthSection, CapabilitiesSection, EntryMode, EntrySection, HealthSection, LimitsSection,
    ManifestError, PluginManifest, RoutingSection, SourceSection, UnknownModelPolicy,
};
pub use parse::parse_manifest;
pub use validate::{validate, ValidationError};
pub use version::{api_version_supported, manifest_version_supported, supports, Unsupported};

/// Supported `manifestVersion` values (contract §8: additive evolution).
pub const SUPPORTED_MANIFEST_VERSIONS: &[&str] = &["0.1"];
/// Supported `apiVersion` values.
pub const SUPPORTED_API_VERSIONS: &[&str] = &["v1"];
