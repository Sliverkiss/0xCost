//! Host config file (R1W1 A2): TOML schema, `--config` loading, and
//! per-plugin `enabled` overrides.
//!
//! Semantics (spec A2):
//! - `[server]` supplies `bind` and `api_keys`. `api_keys` entries are
//!   either literal keys or `env:VAR` references resolved at startup.
//! - `[plugins."<id>"]` sections override the manifest's
//!   `routing.enabled` default — in either direction. A missing section
//!   means the manifest default applies.
//! - Config is read once at startup; changing it requires a host restart
//!   (hot reload is out of scope — YAGNI).
//!
//! The override is applied by mutating the manifest's `routing.enabled`
//! before instance construction, so the existing §6 "off = invisible"
//! semantics (excluded from routing, catalog, probing) flow through the
//! normal `is_servable()` / probe paths unchanged.

use std::collections::BTreeMap;
use std::path::Path;

use serde::Deserialize;

use oxcost_contract::PluginManifest;

/// Typed config-file failure: message carries the TOML line numbers when
/// the underlying error is a parse error.
#[derive(Debug, thiserror::Error)]
#[error("config error: {0}")]
pub struct ConfigError(pub String);

/// `[server]` section.
#[derive(Debug, Clone, PartialEq, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServerSection {
    /// Listen address, e.g. `"127.0.0.1:8123"`.
    pub bind: Option<String>,
    /// Accepted bearer keys: literal values or `env:VAR` references.
    #[serde(default)]
    pub api_keys: Vec<String>,
}

/// `[plugins."<id>"]` section.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginSection {
    /// Override for the manifest's `routing.enabled` (either direction).
    pub enabled: bool,
}

/// Parsed host config document.
#[derive(Debug, Clone, PartialEq, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HostConfigFile {
    #[serde(default)]
    pub server: ServerSection,
    #[serde(default)]
    pub plugins: BTreeMap<String, PluginSection>,
}

impl HostConfigFile {
    /// Parse TOML text. Syntax and shape errors include TOML line/column
    /// positions when available (toml's Display embeds them).
    pub fn parse(text: &str) -> Result<Self, ConfigError> {
        toml::from_str(text).map_err(|e| ConfigError(format!("{e}")))
    }

    /// Load and parse a config file from disk.
    pub fn load(path: &Path) -> Result<Self, ConfigError> {
        let text = std::fs::read_to_string(path)
            .map_err(|e| ConfigError(format!("cannot read {}: {e}", path.display())))?;
        Self::parse(&text)
    }

    /// Resolve `api_keys` entries: `env:VAR` reads the variable at startup
    /// (comma-separated values allowed, mirroring `OXCOST_API_KEYS`);
    /// anything else is a literal key. Unresolvable references yield an
    /// empty list, same as the env-var path in `main`.
    pub fn resolve_api_keys(&self) -> Vec<String> {
        let mut keys = Vec::new();
        for entry in &self.server.api_keys {
            if let Some(var) = entry.strip_prefix("env:") {
                if let Ok(value) = std::env::var(var) {
                    keys.extend(
                        value
                            .split(',')
                            .map(str::trim)
                            .filter(|s| !s.is_empty())
                            .map(str::to_string),
                    );
                }
            } else if !entry.is_empty() {
                keys.push(entry.clone());
            }
        }
        keys
    }

    /// Apply per-plugin `enabled` overrides to a manifest (in place).
    /// Unknown plugin ids in the config are ignored: manifests come and go
    ///, and a stale section must not fail startup (same tolerance as the
    /// loader's skip semantics).
    pub fn apply_overrides(&self, manifest: &mut PluginManifest) {
        if let Some(section) = self.plugins.get(&manifest.id) {
            manifest.routing.enabled = section.enabled;
        }
    }

    /// Effective `enabled` for one plugin id: config override or the
    /// manifest's value when no section exists. Exported for tests and
    /// for callers that need the decision without mutating a manifest.
    pub fn effective_enabled(&self, id: &str, manifest_enabled: bool) -> bool {
        self.plugins.get(id).map_or(manifest_enabled, |s| s.enabled)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = r#"
[server]
bind = "127.0.0.1:8123"
api_keys = ["env:OXCOST_API_KEYS"]

[plugins."dev.oxcost.tokenharbor"]
enabled = false

[plugins."dev.oxcost.mimo-rust"]
enabled = true
"#;

    #[test]
    fn parse_full_sample() {
        let cfg = HostConfigFile::parse(SAMPLE).expect("sample parses");
        assert_eq!(cfg.server.bind.as_deref(), Some("127.0.0.1:8123"));
        assert_eq!(cfg.server.api_keys, ["env:OXCOST_API_KEYS"]);
        assert_eq!(cfg.plugins.len(), 2);
        assert!(!cfg.plugins["dev.oxcost.tokenharbor"].enabled);
        assert!(cfg.plugins["dev.oxcost.mimo-rust"].enabled);
    }

    #[test]
    fn empty_document_is_valid_defaults() {
        let cfg = HostConfigFile::parse("").expect("empty is fine");
        assert_eq!(cfg, HostConfigFile::default());
        assert!(cfg.server.bind.is_none());
        assert!(cfg.plugins.is_empty());
    }

    #[test]
    fn bad_toml_reports_line_numbers() {
        // Missing value after `=`.
        let err = HostConfigFile::parse("[server]\nbind =\n").unwrap_err();
        let msg = err.0;
        assert!(msg.contains("line 2"), "line info in: {msg}");
    }

    #[test]
    fn unknown_fields_rejected() {
        let err = HostConfigFile::parse("[server]\nwat = 1\n").unwrap_err();
        assert!(err.0.contains("wat"), "field named in: {}", err.0);

        let err = HostConfigFile::parse("[plugins.x]\nenabled = true\nextra = 1\n").unwrap_err();
        assert!(err.0.contains("extra"), "field named in: {}", err.0);
    }

    #[test]
    fn plugin_section_requires_enabled() {
        // `enabled` is the only meaningful key and it is mandatory — a
        // section without it is a config mistake, not a default.
        let err = HostConfigFile::parse("[plugins.\"dev.oxcost.x\"]\n").unwrap_err();
        assert!(err.0.contains("enabled"), "message: {}", err.0);
    }

    fn manifest_with(enabled: bool) -> PluginManifest {
        let text = format!(
            r#"
manifestVersion = "0.1"
id = "dev.oxcost.cfg"
name = "cfg"
version = "1.0.0"
apiVersion = "v1"
license = "MIT"

[entry]
mode = "external"
port = 21224

[routing]
enabled = {enabled}
models = ["cfg/m"]
unknown_model = "reject"

[auth]
keys = "env:CFG_TEST_KEY"
"#
        );
        oxcost_contract::parse_manifest(&text).expect("fixture parses")
    }

    #[test]
    fn override_precedence_both_directions() {
        // config-off overrides manifest-on
        let cfg = HostConfigFile::parse("[plugins.\"dev.oxcost.cfg\"]\nenabled = false\n")
            .expect("parses");
        let mut m = manifest_with(true);
        cfg.apply_overrides(&mut m);
        assert!(!m.routing.enabled);

        // config-on overrides manifest-off
        let cfg = HostConfigFile::parse("[plugins.\"dev.oxcost.cfg\"]\nenabled = true\n")
            .expect("parses");
        let mut m = manifest_with(false);
        cfg.apply_overrides(&mut m);
        assert!(m.routing.enabled);
    }

    #[test]
    fn missing_section_keeps_manifest_default() {
        let cfg = HostConfigFile::parse("").expect("empty");
        let mut on = manifest_with(true);
        cfg.apply_overrides(&mut on);
        assert!(on.routing.enabled);
        let mut off = manifest_with(false);
        cfg.apply_overrides(&mut off);
        assert!(!off.routing.enabled);

        assert!(cfg.effective_enabled("dev.oxcost.cfg", true));
        assert!(!cfg.effective_enabled("dev.oxcost.cfg", false));
    }

    #[test]
    fn unknown_plugin_id_ignored() {
        let cfg = HostConfigFile::parse("[plugins.\"dev.oxcost.other\"]\nenabled = false\n")
            .expect("parses");
        let mut m = manifest_with(true);
        cfg.apply_overrides(&mut m);
        assert!(m.routing.enabled, "stale section must not touch the plugin");
    }

    #[test]
    fn api_keys_env_reference_and_literals() {
        // env: reference — resolved through the same comma-splitting shape
        // as the OXCOST_API_KEYS env path in main.
        let var = "OXCOST_TEST_CFG_KEYS";
        let cfg = HostConfigFile::parse(&format!("[server]\napi_keys = [\"env:{var}\"]\n"))
            .expect("parses");
        std::env::set_var(var, " k1 , k2 ,,");
        assert_eq!(cfg.resolve_api_keys(), ["k1", "k2"]);
        std::env::remove_var(var);

        // Unresolvable env var → no keys (caller decides whether that is
        // dev mode; here it just resolves to empty).
        let cfg = HostConfigFile::parse("[server]\napi_keys = [\"env:NOPE_UNSET_VAR\"]\n")
            .expect("parses");
        assert!(cfg.resolve_api_keys().is_empty());

        // Literals pass through verbatim; empty entries dropped.
        let cfg =
            HostConfigFile::parse("[server]\napi_keys = [\"sk-lit\", \"\"]\n").expect("parses");
        assert_eq!(cfg.resolve_api_keys(), ["sk-lit"]);
    }

    #[test]
    fn load_missing_file_errors_with_path() {
        let err = HostConfigFile::load(Path::new("/nonexistent/oxcost-cfg.toml"))
            .expect_err("missing file");
        assert!(err.0.contains("/nonexistent/oxcost-cfg.toml"), "{}", err.0);
    }
}
