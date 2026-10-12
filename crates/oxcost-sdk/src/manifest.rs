//! `SdkManifest`: the code-declared manifest for SDK plugins.
//!
//! Same semantics as plugin.toml (contract §3) — id format, name == last id
//! segment, model prefix rule, capability whitelist + dependencies — but
//! authored in Rust, not TOML. Validation reuses the contract crate's rules
//! verbatim (single contract, four forms): the struct converts into a
//! synthetic `PluginManifest` and runs the same `validate()` a sidecar
//! manifest goes through.

use oxcost_contract::{
    AuthSection, CapabilitiesSection, Capability, CapabilitySet, EntryMode, EntrySection,
    HealthSection, PluginManifest, RoutingSection, UnknownModelPolicy,
};

/// Errors from building/validating an `SdkManifest`.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum SdkManifestError {
    /// The underlying contract validation refused the manifest.
    #[error("SDK manifest rejected: {0}")]
    Invalid(String),
}

/// Code-declared manifest for an SDK plugin (contract §3 semantics).
///
/// Field-for-field the same routing-relevant information a plugin.toml
/// carries — id, name, version, capabilities, model list, `unknown_model`
/// policy, `enabled` switch. The entry/health/auth dimensions of a sidecar
/// manifest make no sense in-process: entry mode is fixed (`host-sdk`),
/// there is no separate process to health-probe over HTTP and no env key
/// indirection needed.
///
/// No serde derives: manifests are declared in code, never parsed from
/// disk (that is what the sidecar/TOML form is for).
#[derive(Debug, Clone, PartialEq)]
pub struct SdkManifest {
    /// Reverse-DNS plugin id, e.g. `dev.oxcost.sample-echo`.
    pub id: String,
    /// Display name; must equal the last segment of `id` (prefix rule).
    pub name: String,
    /// Plugin semver, e.g. "0.1.0".
    pub version: String,
    /// Declared model ids, `name/model` prefixed (§3 prefix rule).
    pub models: Vec<String>,
    /// Declared capability set.
    pub capabilities: CapabilitySet,
    /// Policy for undeclared models (§3; default reject).
    pub unknown_model: UnknownModelPolicy,
    /// `routing.enabled` equivalent (§6 switch; default true).
    pub enabled: bool,
}

impl SdkManifest {
    /// Start a manifest for `id`; `name` defaults to the id's last segment
    /// (the §3 prefix rule), chat-only capabilities, reject policy.
    pub fn new(id: impl Into<String>) -> Self {
        let id = id.into();
        let name = id.rsplit('.').next().unwrap_or(&id).to_string();
        Self {
            id,
            name,
            version: "0.1.0".into(),
            models: Vec::new(),
            capabilities: CapabilitySet::from_caps([Capability::Chat]),
            unknown_model: UnknownModelPolicy::Reject,
            enabled: true,
        }
    }

    /// Set the semver.
    pub fn version(mut self, version: impl Into<String>) -> Self {
        self.version = version.into();
        self
    }

    /// Set the declared model list (full `name/model` prefixed ids).
    pub fn models(mut self, models: Vec<String>) -> Self {
        self.models = models;
        self
    }

    /// Declare capabilities (replaces the chat-only default).
    pub fn capabilities(mut self, caps: CapabilitySet) -> Self {
        self.capabilities = caps;
        self
    }

    /// Set the `unknown_model` policy.
    pub fn unknown_model(mut self, policy: UnknownModelPolicy) -> Self {
        self.unknown_model = policy;
        self
    }

    /// Set the enabled switch (§6).
    pub fn enabled(mut self, enabled: bool) -> Self {
        self.enabled = enabled;
        self
    }

    /// Validate against the contract §3 rules.
    ///
    /// Converts to the shared `PluginManifest` shape and runs the same
    /// validator sidecar manifests go through — single contract, one rule
    /// set. `auth.keys` is synthesized as a non-secret `env:` placeholder
    /// because in-process plugins hold no key material.
    pub fn validate(&self) -> Result<(), SdkManifestError> {
        if self.models.is_empty() {
            return Err(SdkManifestError::Invalid(
                "routing.models must be non-empty".into(),
            ));
        }
        let manifest = self.to_contract_manifest();
        oxcost_contract::validate(&manifest).map_err(|e| SdkManifestError::Invalid(e.to_string()))
    }

    /// Convert into the shared contract manifest shape so the host's
    /// registry/lifecycle/instance machinery treats both forms uniformly.
    pub fn to_contract_manifest(&self) -> PluginManifest {
        PluginManifest {
            manifest_version: "0.1".into(),
            id: self.id.clone(),
            name: self.name.clone(),
            version: self.version.clone(),
            api_version: "v1".into(),
            license: "AGPL-3.0".into(),
            entry: EntrySection {
                // Nearest wire equivalent; never used for I/O — SDK plugins
                // are dispatched in-process.
                mode: EntryMode::External,
                cmd: None,
                // Validated shape requires a port; unused in-process.
                port: Some(0),
                url: None,
            },
            health: Some(HealthSection {
                path: "/healthz".into(),
                interval_secs: 30,
                timeout_secs: 5,
            }),
            routing: RoutingSection {
                enabled: self.enabled,
                models: self.models.clone(),
                unknown_model: self.unknown_model.clone(),
            },
            capabilities: capabilities_to_section(&self.capabilities),
            auth: AuthSection {
                // Placeholder; SDK plugins hold no keys (§4 auth is a
                // sidecar/remote concern).
                keys: "env:OXCOST_SDK_NO_KEY".into(),
            },
            limits: None,
            source: None,
        }
    }
}

/// Map a [`CapabilitySet`] onto the manifest `[capabilities]` flags shape.
fn capabilities_to_section(set: &CapabilitySet) -> CapabilitiesSection {
    let mut s = CapabilitiesSection::default();
    for cap in set.iter() {
        match cap {
            Capability::Chat => s.chat = true,
            Capability::Embeddings => s.embeddings = true,
            Capability::Images => s.images = true,
            Capability::AudioSpeech => s.audio_speech = true,
            Capability::AudioTranscription => s.audio_transcription = true,
            Capability::AudioTranslations => s.audio_translations = true,
            Capability::Video => s.video = true,
            Capability::Moderations => s.moderations = true,
            Capability::Responses => s.responses = true,
            Capability::WebSearch => s.web_search = true,
            Capability::Files => s.files = true,
            Capability::Batches => s.batches = true,
            Capability::Realtime => s.realtime = true,
        }
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use oxcost_contract::Capability;

    fn echo_manifest() -> SdkManifest {
        SdkManifest::new("dev.oxcost.sample-echo")
            .version("0.1.0")
            .models(vec![
                "sample-echo/echo-1".into(),
                "sample-echo/echo-2".into(),
            ])
    }

    #[test]
    fn name_defaults_to_last_id_segment() {
        let m = SdkManifest::new("dev.oxcost.sample-echo");
        assert_eq!(m.name, "sample-echo");
    }

    #[test]
    fn valid_manifest_passes_contract_validation() {
        assert_eq!(echo_manifest().validate(), Ok(()));
    }

    #[test]
    fn empty_models_rejected() {
        let m = SdkManifest::new("dev.oxcost.sample-echo");
        assert!(m.validate().is_err());
    }

    #[test]
    fn prefix_rule_enforced() {
        let m =
            SdkManifest::new("dev.oxcost.sample-echo").models(vec!["wrong-prefix/echo-1".into()]);
        match m.validate() {
            Err(SdkManifestError::Invalid(msg)) => {
                assert!(msg.contains("does not match plugin name"), "{msg}");
            }
            other => panic!("expected prefix rejection, got {other:?}"),
        }
    }

    #[test]
    fn bad_id_rejected() {
        let mut m = echo_manifest();
        m.id = "not-reverse-dns".into();
        assert!(m.validate().is_err());
    }

    #[test]
    fn name_id_mismatch_rejected() {
        let mut m = echo_manifest();
        m.name = "other".into();
        assert!(m.validate().is_err());
    }

    #[test]
    fn capability_dependency_enforced() {
        let m = echo_manifest().capabilities(CapabilitySet::from_caps([Capability::Batches]));
        assert!(m.validate().is_err(), "batches without files must fail");
    }

    #[test]
    fn conversion_carries_routing_fields() {
        let m = echo_manifest().enabled(false);
        let c = m.to_contract_manifest();
        assert_eq!(c.id, "dev.oxcost.sample-echo");
        assert_eq!(c.name, "sample-echo");
        assert!(!c.routing.enabled);
        assert_eq!(c.routing.models.len(), 2);
        assert!(CapabilitySet::from_section(&c.capabilities).contains(Capability::Chat));
    }
}
