//! Manifest parsing: TOML text → `PluginManifest`.

use crate::manifest::{ManifestError, PluginManifest};

/// Parse a `plugin.toml` document. Syntax/shape errors are structured;
/// semantic rules (id format, env: auth, mode fields, ...) run later in
/// `validate`.
pub fn parse_manifest(text: &str) -> Result<PluginManifest, ManifestError> {
    text.parse::<toml::Table>()
        .map_err(|e| ManifestError(format!("TOML syntax: {e}")))?
        .try_into()
        .map_err(|e: toml::de::Error| ManifestError(format!("schema: {e}")))
}
