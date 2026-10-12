//! Manifest validation (contract §3 rules). Every rule is a typed error
//! variant; the host rejects registration on any failure.

use crate::capability::CapabilitySet;
use crate::manifest::{EntryMode, PluginManifest, UnknownModelPolicy};
use crate::SUPPORTED_API_VERSIONS;
use crate::SUPPORTED_MANIFEST_VERSIONS;

/// Typed validation failure, one per contract §3 rule.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum ValidationError {
    /// Rule 1: id must match `^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)+$`.
    #[error("invalid plugin id `{0}`: must be reverse-DNS, lowercase, ≥2 segments")]
    InvalidId(String),
    /// Rule 2: manifestVersion outside the supported set.
    #[error("unsupported manifestVersion `{0}`; supported: {SUPPORTED_MANIFEST_VERSIONS:?}")]
    UnsupportedManifestVersion(String),
    /// Rule 2: apiVersion outside the supported set.
    #[error("unsupported apiVersion `{0}`; supported: {SUPPORTED_API_VERSIONS:?}")]
    UnsupportedApiVersion(String),
    /// Rule 3: models list empty.
    #[error("routing.models must be non-empty")]
    EmptyModels,
    /// Rule 3: duplicate model entries.
    #[error("duplicate model `{0}` in routing.models")]
    DuplicateModel(String),
    /// Rule 3: model prefix must equal the last id segment (= `name`).
    #[error("model `{model}` prefix `{prefix}` does not match plugin name `{name}`")]
    ModelPrefixMismatch {
        model: String,
        prefix: String,
        name: String,
    },
    /// Rule 3 corollary: display name must equal the last id segment.
    #[error("name `{name}` must equal last segment of id `{id}`")]
    NameIdMismatch { name: String, id: String },
    /// Rule 4: auth.keys must be an `env:` reference.
    #[error("auth.keys must start with `env:` (got `{0}`)")]
    AuthNotEnvRef(String),
    /// Rule 4: value looks like a literal secret.
    #[error("auth.keys looks like a literal secret, env: reference required")]
    LiteralSecret,
    /// Rule 5: mode requires a field that is absent.
    #[error("entry.mode `{mode}` requires `{field}`")]
    MissingEntryField {
        mode: &'static str,
        field: &'static str,
    },
    /// Rule 5: remote url must be http/https.
    #[error("entry.url must be http(s), got `{0}`")]
    InvalidEntryUrl(String),
    /// Rule 5: remote forbids the [health] table.
    #[error("entry.mode `remote` forbids [health] (probing is a lightweight chat call)")]
    HealthForbiddenForRemote,
    /// Rule 6: capability declared without its dependency.
    #[error("capability `{cap}` requires its dependency `{dep}`")]
    MissingCapabilityDependency {
        cap: &'static str,
        dep: &'static str,
    },
    /// Rule 7: unknown_model is not `pass`/`reject`/`fallback=<model>`.
    #[error("routing.unknown_model must be `pass`, `reject`, or `fallback=<model>`")]
    InvalidUnknownModel,
}

/// Run every §3 rule; `Ok(())` means the manifest may register.
pub fn validate(manifest: &PluginManifest) -> Result<(), ValidationError> {
    validate_id(manifest)?;
    validate_versions(manifest)?;
    validate_models(manifest)?;
    validate_auth(manifest)?;
    validate_entry(manifest)?;
    validate_capabilities(manifest)?;
    validate_unknown_model(manifest)
}

/// Rule 1: `^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)+$`.
///
/// Hand-rolled equivalent: lowercase letters/digits/hyphens/dots only,
/// first char a letter, every dot starts a new non-empty segment whose
/// first char is a letter or digit, at least two segments, no trailing dot.
fn id_is_valid(id: &str) -> bool {
    if id.is_empty() || !id.starts_with(|c: char| c.is_ascii_lowercase()) || id.ends_with('.') {
        return false;
    }
    if !id
        .chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || c == '.')
    {
        return false;
    }
    let segments: Vec<&str> = id.split('.').collect();
    if segments.len() < 2 {
        return false;
    }
    segments.iter().all(|seg| {
        let mut chars = seg.chars();
        let first = chars.next().unwrap_or_default();
        (first.is_ascii_lowercase() || first.is_ascii_digit())
            && chars.all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
    })
}

fn validate_id(manifest: &PluginManifest) -> Result<(), ValidationError> {
    let id = &manifest.id;
    if !id_is_valid(id) {
        return Err(ValidationError::InvalidId(id.clone()));
    }

    // Rule 3 corollary: name == last id segment (plugin prefix rule).
    let last = id.rsplit('.').next().unwrap_or_default();
    if manifest.name != last {
        return Err(ValidationError::NameIdMismatch {
            name: manifest.name.clone(),
            id: id.clone(),
        });
    }
    Ok(())
}

fn validate_versions(manifest: &PluginManifest) -> Result<(), ValidationError> {
    if !SUPPORTED_MANIFEST_VERSIONS.contains(&manifest.manifest_version.as_str()) {
        return Err(ValidationError::UnsupportedManifestVersion(
            manifest.manifest_version.clone(),
        ));
    }
    if !SUPPORTED_API_VERSIONS.contains(&manifest.api_version.as_str()) {
        return Err(ValidationError::UnsupportedApiVersion(
            manifest.api_version.clone(),
        ));
    }
    Ok(())
}

fn validate_models(manifest: &PluginManifest) -> Result<(), ValidationError> {
    if manifest.routing.models.is_empty() {
        return Err(ValidationError::EmptyModels);
    }
    let mut seen = std::collections::BTreeSet::new();
    for model in &manifest.routing.models {
        if !seen.insert(model) {
            return Err(ValidationError::DuplicateModel(model.clone()));
        }
        // Plugin prefix = first path segment of the model id.
        let prefix = model.split('/').next().unwrap_or_default();
        if prefix != manifest.name {
            return Err(ValidationError::ModelPrefixMismatch {
                model: model.clone(),
                prefix: prefix.to_string(),
                name: manifest.name.clone(),
            });
        }
    }
    Ok(())
}

/// Rule 4 heuristics: `sk-` anywhere, or a ≥32-char dense token.
fn looks_like_secret(value: &str) -> bool {
    if value.contains("sk-") {
        return true;
    }
    value.len() >= 32
        && value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
}

fn validate_auth(manifest: &PluginManifest) -> Result<(), ValidationError> {
    let keys = &manifest.auth.keys;
    if looks_like_secret(keys) {
        return Err(ValidationError::LiteralSecret);
    }
    if !keys.starts_with("env:") {
        return Err(ValidationError::AuthNotEnvRef(keys.clone()));
    }
    // Even behind `env:`, embedded secret material is rejected.
    if looks_like_secret(&keys[4..]) {
        return Err(ValidationError::LiteralSecret);
    }
    Ok(())
}

fn validate_entry(manifest: &PluginManifest) -> Result<(), ValidationError> {
    use EntryMode::*;
    let entry = &manifest.entry;
    match entry.mode {
        HostSpawn => {
            if entry.cmd.as_deref().unwrap_or_default().is_empty() {
                return Err(ValidationError::MissingEntryField {
                    mode: "host-spawn",
                    field: "cmd",
                });
            }
        }
        External => {
            if entry.port.is_none() {
                return Err(ValidationError::MissingEntryField {
                    mode: "external",
                    field: "port",
                });
            }
        }
        Remote => {
            let Some(url) = entry.url.as_deref() else {
                return Err(ValidationError::MissingEntryField {
                    mode: "remote",
                    field: "url",
                });
            };
            if !(url.starts_with("http://") || url.starts_with("https://")) {
                return Err(ValidationError::InvalidEntryUrl(url.to_string()));
            }
            if manifest.health.is_some() {
                return Err(ValidationError::HealthForbiddenForRemote);
            }
        }
    }
    Ok(())
}

fn validate_capabilities(manifest: &PluginManifest) -> Result<(), ValidationError> {
    let set = CapabilitySet::from_section(&manifest.capabilities);
    if let Some((cap, dep)) = set.unmet_dependencies().first() {
        return Err(ValidationError::MissingCapabilityDependency {
            cap: cap.as_str(),
            dep: dep.as_str(),
        });
    }
    Ok(())
}

fn validate_unknown_model(manifest: &PluginManifest) -> Result<(), ValidationError> {
    match &manifest.routing.unknown_model {
        // A bare `fallback=` (empty target) is a malformed policy.
        UnknownModelPolicy::Fallback(m) if m.trim().is_empty() => {
            Err(ValidationError::InvalidUnknownModel)
        }
        UnknownModelPolicy::Pass | UnknownModelPolicy::Reject | UnknownModelPolicy::Fallback(_) => {
            Ok(())
        }
    }
}
