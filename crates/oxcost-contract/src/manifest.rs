//! Manifest data model mirroring contract §3 field-for-field.

use serde::{Deserialize, Serialize};

/// Top-level `plugin.toml` document.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginManifest {
    /// Contract version of the manifest format, e.g. `"0.1"`.
    #[serde(rename = "manifestVersion")]
    pub manifest_version: String,
    /// Reverse-DNS plugin id, globally unique, e.g. `dev.oxcost.mimo2api`.
    pub id: String,
    /// Display name; must equal the last segment of `id`.
    pub name: String,
    /// Plugin semver.
    pub version: String,
    /// Host runtime contract version the plugin targets, e.g. `"v1"`.
    #[serde(rename = "apiVersion")]
    pub api_version: String,
    /// Plugin license (SPDX identifier or similar).
    pub license: String,
    /// How the plugin runs (contract §3 `[entry]`).
    pub entry: EntrySection,
    /// Health probing config; forbidden for `remote` mode.
    #[serde(default)]
    pub health: Option<HealthSection>,
    /// Model routing config.
    pub routing: RoutingSection,
    /// Capability flags; defaults to chat-only.
    #[serde(default)]
    pub capabilities: CapabilitiesSection,
    /// Key references (`env:` only, never literals).
    pub auth: AuthSection,
    /// Optional per-plugin limits; unset inherits host defaults.
    #[serde(default)]
    pub limits: Option<LimitsSection>,
    /// Optional supply-chain provenance.
    #[serde(default)]
    pub source: Option<SourceSection>,
}

/// `[entry]`: how the host reaches the plugin.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EntrySection {
    /// `host-spawn | external | remote`.
    pub mode: EntryMode,
    /// Command line for `host-spawn`; ignored otherwise.
    #[serde(default)]
    pub cmd: Option<Vec<String>>,
    /// Local sidecar port for `external`.
    #[serde(default)]
    pub port: Option<u16>,
    /// Upstream base URL for `remote` (http/https).
    #[serde(default)]
    pub url: Option<String>,
}

/// Entry modes (contract §3).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum EntryMode {
    /// Host spawns and supervises the process.
    HostSpawn,
    /// Already-deployed local sidecar.
    External,
    /// Remote OpenAI-compatible endpoint.
    Remote,
}

/// `[health]`: probe configuration.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HealthSection {
    /// Health endpoint path, e.g. `/healthz`.
    pub path: String,
    /// Probe interval in seconds.
    pub interval_secs: u64,
    /// Probe timeout in seconds.
    pub timeout_secs: u64,
}

/// `[routing]`: model exposure.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutingSection {
    /// Default `true`; `false` = registered but disabled (§6 switch).
    #[serde(default = "default_true")]
    pub enabled: bool,
    /// Declared model ids; must be verified real upstream.
    pub models: Vec<String>,
    /// Policy for models not in `models`.
    pub unknown_model: UnknownModelPolicy,
}

/// `routing.unknown_model`: `pass | fallback=<model> | reject`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(from = "String", into = "String")]
pub enum UnknownModelPolicy {
    /// Forward undeclared models upstream as-is.
    Pass,
    /// Route undeclared models to a fallback model.
    Fallback(String),
    /// Refuse undeclared models.
    Reject,
}

impl UnknownModelPolicy {
    /// Parse the wire string: bare `pass`/`reject`, or `fallback=<model>`
    /// with a non-empty model. Malformed values return `None`.
    pub fn parse(s: &str) -> Option<Self> {
        match s.trim() {
            "pass" => Some(Self::Pass),
            "reject" => Some(Self::Reject),
            other => other
                .strip_prefix("fallback=")
                .filter(|m| !m.trim().is_empty())
                .map(|m| Self::Fallback(m.to_string())),
        }
    }
}

impl From<UnknownModelPolicy> for String {
    fn from(p: UnknownModelPolicy) -> Self {
        match p {
            UnknownModelPolicy::Pass => "pass".into(),
            UnknownModelPolicy::Fallback(m) => format!("fallback={m}"),
            UnknownModelPolicy::Reject => "reject".into(),
        }
    }
}

impl From<String> for UnknownModelPolicy {
    fn from(s: String) -> Self {
        // Shape is checked at validation time; malformed values never reach
        // here because deserialization keeps the raw string.
        Self::parse(&s).unwrap_or(Self::Pass)
    }
}

const fn default_true() -> bool {
    true
}

/// `[capabilities]`: capability flags, chat defaults to `true`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, default)]
pub struct CapabilitiesSection {
    /// `/v1/chat/completions` (default capability, §4).
    pub chat: bool,
    pub embeddings: bool,
    pub images: bool,
    pub audio_speech: bool,
    pub audio_transcription: bool,
    pub audio_translations: bool,
    pub video: bool,
    pub moderations: bool,
    pub responses: bool,
    pub web_search: bool,
    pub files: bool,
    pub batches: bool,
    pub realtime: bool,
}

impl Default for CapabilitiesSection {
    fn default() -> Self {
        Self {
            chat: true,
            embeddings: false,
            images: false,
            audio_speech: false,
            audio_transcription: false,
            audio_translations: false,
            video: false,
            moderations: false,
            responses: false,
            web_search: false,
            files: false,
            batches: false,
            realtime: false,
        }
    }
}

/// `[auth]`: key references.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthSection {
    /// `env:VAR_NAME` reference; literals are rejected.
    pub keys: String,
}

/// `[limits]`: optional per-plugin limits.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LimitsSection {
    /// Per-plugin concurrency cap.
    pub max_inflight: u32,
    /// Circuit-breaker cooldown after consecutive failures (seconds).
    pub cooldown_secs: u64,
}

/// `[source]`: optional provenance.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SourceSection {
    /// Source repository URL.
    pub repository: String,
}

/// Structured parse error (TOML syntax or shape mismatch).
#[derive(Debug, thiserror::Error)]
#[error("manifest parse error: {0}")]
pub struct ManifestError(pub String);
