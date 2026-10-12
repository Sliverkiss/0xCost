//! PluginRegistry: id-keyed store of validated manifests + lifecycle
//! state. Sync and pure — no I/O, no tokio (TASK_M1 §4).

use crate::lifecycle::{InvalidTransition, PluginEntry, PluginState, TransitionEvent};
use oxcost_contract::{PluginManifest, ValidationError};
use std::collections::BTreeMap;

/// Registry operation failure.
#[derive(Debug, thiserror::Error, PartialEq)]
pub enum RegistryError {
    /// Id already registered; same-id new version = replace (§6 upgrade),
    /// which callers perform via `replace`.
    #[error("plugin `{0}` is already registered")]
    DuplicateId(String),
    /// Insert ran full manifest validation and it failed.
    #[error("manifest rejected: {0}")]
    Validation(#[from] ValidationError),
    /// No plugin registered under this id.
    #[error("plugin `{0}` is not registered")]
    UnknownPlugin(String),
    /// Lifecycle transition refused.
    #[error("transition refused: {0}")]
    Transition(#[from] InvalidTransition),
}

/// Id-keyed plugin registry with lifecycle management.
#[derive(Debug, Default)]
pub struct PluginRegistry {
    plugins: BTreeMap<String, PluginEntry>,
}

impl PluginRegistry {
    /// Empty registry.
    pub fn new() -> Self {
        Self::default()
    }

    /// Register a plugin: full manifest validation runs here; the entry
    /// starts in `Registered`.
    pub fn insert(&mut self, manifest: PluginManifest) -> Result<(), RegistryError> {
        oxcost_contract::validate(&manifest)?;
        if self.plugins.contains_key(&manifest.id) {
            return Err(RegistryError::DuplicateId(manifest.id));
        }
        self.plugins.insert(
            manifest.id.clone(),
            PluginEntry {
                manifest,
                state: PluginState::Registered,
            },
        );
        Ok(())
    }

    /// Same id, new version: route in-flight requests on the old instance
    /// to completion, then swap (§6 upgrade). Only the manifest changes;
    /// the new entry restarts from `Registered`.
    pub fn replace(&mut self, manifest: PluginManifest) -> Result<(), RegistryError> {
        oxcost_contract::validate(&manifest)?;
        let existing = self
            .plugins
            .get(&manifest.id)
            .ok_or_else(|| RegistryError::UnknownPlugin(manifest.id.clone()))?;
        if existing.manifest.version == manifest.version {
            return Err(RegistryError::DuplicateId(manifest.id));
        }
        self.plugins.insert(
            manifest.id.clone(),
            PluginEntry {
                manifest,
                state: PluginState::Registered,
            },
        );
        Ok(())
    }

    /// Look up one plugin.
    pub fn get(&self, id: &str) -> Option<&PluginEntry> {
        self.plugins.get(id)
    }

    /// All registered entries (including Disabled), sorted by id.
    pub fn list(&self) -> Vec<&PluginEntry> {
        self.plugins.values().collect()
    }

    /// Entries visible to routing and `/v1/models`: not Disabled/Removed.
    /// This is the §6 listing effect of the enable/disable switch.
    pub fn list_routable(&self) -> Vec<&PluginEntry> {
        self.plugins
            .values()
            .filter(|e| e.state != PluginState::Disabled)
            .collect()
    }

    /// Deregister immediately (§6 removal semantics).
    pub fn remove(&mut self, id: &str) -> Result<PluginEntry, RegistryError> {
        self.plugins
            .remove(id)
            .ok_or_else(|| RegistryError::UnknownPlugin(id.to_string()))
    }

    /// Disable a plugin (runtime equivalent of `routing.enabled = false`).
    pub fn disable(&mut self, id: &str) -> Result<TransitionEvent, RegistryError> {
        self.transition(id, PluginState::disable)
    }

    /// Re-enable a disabled plugin; it re-enters Probing.
    pub fn enable(&mut self, id: &str) -> Result<TransitionEvent, RegistryError> {
        self.transition(id, PluginState::start_probing)
    }

    /// Apply a lifecycle transition fn to one plugin.
    fn transition(
        &mut self,
        id: &str,
        f: impl FnOnce(PluginState) -> Result<PluginState, InvalidTransition>,
    ) -> Result<TransitionEvent, RegistryError> {
        let entry = self
            .plugins
            .get_mut(id)
            .ok_or_else(|| RegistryError::UnknownPlugin(id.to_string()))?;
        let from = entry.state;
        let to = f(from)?;
        entry.state = to;
        Ok(TransitionEvent {
            plugin_id: id.to_string(),
            from,
            to,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use oxcost_contract::parse_manifest;

    fn valid_manifest() -> PluginManifest {
        parse_manifest(
            r#"
manifestVersion = "0.1"
id = "dev.oxcost.mimo2api"
name = "mimo2api"
version = "1.0.0"
apiVersion = "v1"
license = "MIT"

[entry]
mode = "external"
port = 21223

[health]
path = "/healthz"
interval_secs = 30
timeout_secs = 5

[routing]
models = ["mimo2api/mimo-v2.6-pro"]
unknown_model = "pass"

[auth]
keys = "env:MIMO2API_API_KEYS"
"#,
        )
        .expect("fixture manifest must parse")
    }

    fn registry_with_one() -> PluginRegistry {
        let mut reg = PluginRegistry::new();
        reg.insert(valid_manifest()).expect("insert");
        reg
    }

    #[test]
    fn insert_valid_starts_registered() {
        let reg = registry_with_one();
        let entry = reg.get("dev.oxcost.mimo2api").expect("present");
        assert_eq!(entry.state, PluginState::Registered);
        assert_eq!(entry.manifest.name, "mimo2api");
    }

    #[test]
    fn insert_rejects_invalid_manifest() {
        let mut bad = valid_manifest();
        bad.auth.keys = "sk-literalsecret1234567890123456789".into();
        let mut reg = PluginRegistry::new();
        assert_eq!(
            reg.insert(bad).unwrap_err(),
            RegistryError::Validation(ValidationError::LiteralSecret)
        );
        assert!(reg.list().is_empty());
    }

    #[test]
    fn insert_rejects_duplicate_id() {
        let mut reg = registry_with_one();
        assert_eq!(
            reg.insert(valid_manifest()).unwrap_err(),
            RegistryError::DuplicateId("dev.oxcost.mimo2api".into())
        );
    }

    #[test]
    fn replace_upgrades_same_id_new_version() {
        let mut reg = registry_with_one();
        let mut v2 = valid_manifest();
        v2.version = "1.1.0".into();
        reg.replace(v2).expect("upgrade");
        assert_eq!(
            reg.get("dev.oxcost.mimo2api").unwrap().manifest.version,
            "1.1.0"
        );

        // Same version again is a duplicate, not an upgrade.
        let mut same = valid_manifest();
        same.version = "1.1.0".into();
        assert_eq!(
            reg.replace(same).unwrap_err(),
            RegistryError::DuplicateId("dev.oxcost.mimo2api".into())
        );

        // Unknown id cannot be replaced.
        let mut other = valid_manifest();
        other.id = "dev.oxcost.other".into();
        other.name = "other".into();
        other.routing.models = vec!["other/m".into()];
        assert!(reg.replace(other).is_err());
    }

    #[test]
    fn remove_deregisters_immediately() {
        let mut reg = registry_with_one();
        let removed = reg.remove("dev.oxcost.mimo2api").expect("remove");
        assert_eq!(removed.state, PluginState::Registered);
        assert!(reg.get("dev.oxcost.mimo2api").is_none());
        assert!(reg.list().is_empty());
        assert_eq!(
            reg.remove("dev.oxcost.mimo2api").unwrap_err(),
            RegistryError::UnknownPlugin("dev.oxcost.mimo2api".into())
        );
    }

    #[test]
    fn disable_then_enable_reenters_probing() {
        let mut reg = registry_with_one();
        let ev = reg.disable("dev.oxcost.mimo2api").expect("disable");
        assert_eq!(ev.from, PluginState::Registered);
        assert_eq!(ev.to, PluginState::Disabled);
        assert!(
            reg.list_routable().is_empty(),
            "disabled excluded from listing"
        );

        let ev = reg.enable("dev.oxcost.mimo2api").expect("enable");
        assert_eq!(ev.from, PluginState::Disabled);
        assert_eq!(ev.to, PluginState::Probing);
        assert_eq!(reg.list_routable().len(), 1);
    }

    #[test]
    fn transition_unknown_plugin_errors() {
        let mut reg = PluginRegistry::new();
        assert_eq!(
            reg.disable("dev.oxcost.nope").unwrap_err(),
            RegistryError::UnknownPlugin("dev.oxcost.nope".into())
        );
    }

    #[test]
    fn list_is_sorted_and_routable_excludes_disabled() {
        let mut reg = registry_with_one();
        let mut second = valid_manifest();
        second.id = "dev.oxcost.aaa".into();
        second.name = "aaa".into();
        second.routing.models = vec!["aaa/m".into()];
        reg.insert(second).expect("insert second");

        let ids: Vec<&str> = reg.list().iter().map(|e| e.manifest.id.as_str()).collect();
        assert_eq!(ids, vec!["dev.oxcost.aaa", "dev.oxcost.mimo2api"]);

        reg.disable("dev.oxcost.aaa").expect("disable");
        let routable: Vec<&str> = reg
            .list_routable()
            .iter()
            .map(|e| e.manifest.id.as_str())
            .collect();
        assert_eq!(routable, vec!["dev.oxcost.mimo2api"]);
    }

    #[test]
    fn disable_while_circuit_open_is_legal() {
        let mut reg = registry_with_one();
        let id = "dev.oxcost.mimo2api".to_string();
        // CircuitOpen is a breaker state the registry cannot reach yet
        // (breaker behavior is M1.5); force it to test switch orthogonality.
        if let Some(entry) = reg.plugins.get_mut(&id) {
            entry.state = PluginState::CircuitOpen;
        }

        reg.disable(&id).expect("disable from CircuitOpen");
        assert_eq!(reg.get(&id).unwrap().state, PluginState::Disabled);

        // Enable re-enters Probing, never resumes CircuitOpen.
        reg.enable(&id).expect("enable");
        assert_eq!(reg.get(&id).unwrap().state, PluginState::Probing);
    }
}
