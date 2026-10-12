//! Version negotiation (contract §8): the host prints its support matrix
//! at startup and rejects manifests whose declared versions it cannot
//! serve.

use crate::manifest::PluginManifest;
use crate::{SUPPORTED_API_VERSIONS, SUPPORTED_MANIFEST_VERSIONS};

/// Reason a manifest failed version negotiation.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum Unsupported {
    /// `manifestVersion` not in the host's supported set.
    #[error("unsupported manifestVersion `{0}`; supported: {SUPPORTED_MANIFEST_VERSIONS:?}")]
    ManifestVersion(String),
    /// `apiVersion` not in the host's supported set.
    #[error("unsupported apiVersion `{0}`; supported: {SUPPORTED_API_VERSIONS:?}")]
    ApiVersion(String),
}

/// Is this manifest format version supported?
pub fn manifest_version_supported(v: &str) -> bool {
    SUPPORTED_MANIFEST_VERSIONS.contains(&v)
}

/// Is this runtime contract version supported?
pub fn api_version_supported(v: &str) -> bool {
    SUPPORTED_API_VERSIONS.contains(&v)
}

/// Version negotiation API (TASK_M1 §3). `Ok(())` = the host can serve
/// this plugin; the error names the offending version.
pub fn supports(manifest: &PluginManifest) -> Result<(), Unsupported> {
    if !manifest_version_supported(&manifest.manifest_version) {
        return Err(Unsupported::ManifestVersion(
            manifest.manifest_version.clone(),
        ));
    }
    if !api_version_supported(&manifest.api_version) {
        return Err(Unsupported::ApiVersion(manifest.api_version.clone()));
    }
    Ok(())
}
