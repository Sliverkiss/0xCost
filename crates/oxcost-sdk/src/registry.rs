//! Explicit plugin registry for SDK plugins.
//!
//! Registration is a plain function call from host `main` — no
//! inventory/linkme collection magic (task brief: explicit is debuggable).
//! On `register`, the manifest is validated (§3 rules), the `models()`
//! return is cross-checked against the declared model list, and a duplicate
//! id is rejected.

use std::collections::BTreeMap;

use crate::manifest::SdkManifestError;
use crate::plugin::SharedPlugin;

/// Registration failure.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum RegistrationError {
    /// Same id already registered (§6: same-id new version = replace via
    /// `Registry::replace`).
    #[error("SDK plugin `{0}` is already registered")]
    DuplicateId(String),
    /// Manifest failed contract validation (§3 rules).
    #[error(transparent)]
    InvalidManifest(#[from] SdkManifestError),
    /// `models()` disagreed with `manifest().models` (declaration drift).
    #[error("models() returned `{0}`, which is not in manifest.models")]
    ModelMismatch(String),
}

/// Id-keyed registry of compiled-in plugins.
#[derive(Default)]
pub struct Registry {
    plugins: BTreeMap<String, SharedPlugin>,
}

impl std::fmt::Debug for Registry {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Registry")
            .field("ids", &self.plugins.keys().collect::<Vec<_>>())
            .finish()
    }
}

impl Registry {
    /// Empty registry.
    pub fn new() -> Self {
        Self::default()
    }

    /// Validate + register a plugin (§3 rules run here; the manifest is
    /// checked once, not per request).
    pub fn register(&mut self, plugin: SharedPlugin) -> Result<(), RegistrationError> {
        let manifest = plugin.manifest();
        manifest.validate()?;
        let declared: std::collections::BTreeSet<&str> =
            manifest.models.iter().map(String::as_str).collect();
        for model in plugin.models() {
            if !declared.contains(model.id.as_str()) {
                return Err(RegistrationError::ModelMismatch(model.id));
            }
        }
        if self.plugins.contains_key(&manifest.id) {
            return Err(RegistrationError::DuplicateId(manifest.id.clone()));
        }
        self.plugins.insert(manifest.id.clone(), plugin);
        Ok(())
    }

    /// Same-id new version: swap the implementation (§6 upgrade semantics).
    /// Rejects a same-version duplicate and unknown ids.
    pub fn replace(&mut self, plugin: SharedPlugin) -> Result<(), RegistrationError> {
        let manifest = plugin.manifest();
        let Some(existing) = self.plugins.get(&manifest.id) else {
            return Err(RegistrationError::InvalidManifest(
                SdkManifestError::Invalid(format!(
                    "cannot replace unregistered plugin `{}`",
                    manifest.id
                )),
            ));
        };
        if existing.manifest().version == manifest.version {
            return Err(RegistrationError::DuplicateId(manifest.id.clone()));
        }
        // Full validation for the new version, then swap. (Not `register`:
        // the id is intentionally already present.)
        manifest.validate()?;
        let declared: std::collections::BTreeSet<&str> =
            manifest.models.iter().map(String::as_str).collect();
        for model in plugin.models() {
            if !declared.contains(model.id.as_str()) {
                return Err(RegistrationError::ModelMismatch(model.id));
            }
        }
        self.plugins.insert(manifest.id.clone(), plugin);
        Ok(())
    }

    /// Look up one plugin by id.
    pub fn get(&self, id: &str) -> Option<&SharedPlugin> {
        self.plugins.get(id)
    }

    /// All registered plugins, sorted by id.
    pub fn list(&self) -> Vec<SharedPlugin> {
        self.plugins.values().cloned().collect()
    }

    /// Number of registered plugins.
    pub fn len(&self) -> usize {
        self.plugins.len()
    }

    /// Is the registry empty?
    pub fn is_empty(&self) -> bool {
        self.plugins.is_empty()
    }

    /// Take all plugins out (host main drains the registry into
    /// `PluginHandle` instances at startup).
    pub fn drain(&mut self) -> Vec<SharedPlugin> {
        std::mem::take(&mut self.plugins).into_values().collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::SdkManifest;
    use crate::plugin::OxcostPlugin;
    use crate::types::{ChatOutcome, ChatRequest, HealthStatus, PluginError, RequestContext};
    use crate::ModelDescriptor;
    use std::sync::Arc;

    /// Minimal test plugin.
    struct Stub {
        manifest: SdkManifest,
    }

    impl Stub {
        fn new(id: &str, models: &[&str]) -> Self {
            Self {
                manifest: SdkManifest::new(id)
                    .models(models.iter().map(|m| m.to_string()).collect()),
            }
        }
    }

    impl OxcostPlugin for Stub {
        fn manifest(&self) -> &SdkManifest {
            &self.manifest
        }

        fn models(&self) -> Vec<ModelDescriptor> {
            self.manifest
                .models
                .iter()
                .map(|m| ModelDescriptor::new(m.clone()))
                .collect()
        }

        fn chat(&self, _req: ChatRequest, _ctx: RequestContext) -> crate::plugin::ChatFuture<'_> {
            Box::pin(async { Err::<ChatOutcome, PluginError>(PluginError::upstream("stub")) })
        }

        fn health(&self) -> HealthStatus {
            HealthStatus::Ok
        }
    }

    /// Simulates `models()` drifting from the manifest.
    struct Drift(Stub);

    impl OxcostPlugin for Drift {
        fn manifest(&self) -> &SdkManifest {
            self.0.manifest()
        }
        fn models(&self) -> Vec<ModelDescriptor> {
            vec![ModelDescriptor::new("sample-echo/ghost")]
        }
        fn chat(&self, req: ChatRequest, ctx: RequestContext) -> crate::plugin::ChatFuture<'_> {
            self.0.chat(req, ctx)
        }
        fn health(&self) -> HealthStatus {
            self.0.health()
        }
    }

    #[test]
    fn register_validates_and_stores() {
        let mut reg = Registry::new();
        reg.register(Arc::new(Stub::new(
            "dev.oxcost.sample-echo",
            &["sample-echo/echo-1"],
        )))
        .expect("valid plugin registers");
        assert_eq!(reg.len(), 1);
        assert!(reg.get("dev.oxcost.sample-echo").is_some());
        assert_eq!(reg.list().len(), 1);
    }

    #[test]
    fn register_rejects_bad_manifest() {
        let mut reg = Registry::new();
        let err = reg
            .register(Arc::new(Stub::new("dev.oxcost.sample-echo", &[])))
            .unwrap_err();
        assert!(matches!(
            err,
            RegistrationError::InvalidManifest(SdkManifestError::Invalid(_))
        ));
        assert!(reg.is_empty());
    }

    #[test]
    fn register_rejects_model_mismatch() {
        let mut reg = Registry::new();
        let drifted: SharedPlugin = Arc::new(Drift(Stub::new(
            "dev.oxcost.sample-echo",
            &["sample-echo/echo-1"],
        )));
        assert_eq!(
            reg.register(drifted).unwrap_err(),
            RegistrationError::ModelMismatch("sample-echo/ghost".into())
        );
    }

    #[test]
    fn duplicate_id_rejected() {
        let mut reg = Registry::new();
        let p: SharedPlugin =
            Arc::new(Stub::new("dev.oxcost.sample-echo", &["sample-echo/echo-1"]));
        reg.register(Arc::clone(&p)).expect("first");
        assert_eq!(
            reg.register(p).unwrap_err(),
            RegistrationError::DuplicateId("dev.oxcost.sample-echo".into())
        );
    }

    #[test]
    fn replace_swaps_version_rejects_same_version() {
        let mut reg = Registry::new();
        reg.register(Arc::new(Stub::new(
            "dev.oxcost.sample-echo",
            &["sample-echo/echo-1"],
        )))
        .unwrap();

        // Same version → duplicate.
        let same: SharedPlugin =
            Arc::new(Stub::new("dev.oxcost.sample-echo", &["sample-echo/echo-1"]));
        assert_eq!(
            reg.replace(same).unwrap_err(),
            RegistrationError::DuplicateId("dev.oxcost.sample-echo".into())
        );

        // New version → swap.
        let v2: SharedPlugin = Arc::new(Stub {
            manifest: SdkManifest::new("dev.oxcost.sample-echo")
                .version("0.2.0")
                .models(vec!["sample-echo/echo-1".into()]),
        });
        reg.replace(v2).expect("upgrade");
        assert_eq!(
            reg.get("dev.oxcost.sample-echo")
                .unwrap()
                .manifest()
                .version,
            "0.2.0"
        );

        // Unknown id → error.
        let other: SharedPlugin = Arc::new(Stub::new("dev.oxcost.other", &["other/m"]));
        assert!(reg.replace(other).is_err());
    }

    #[test]
    fn drain_empties_registry() {
        let mut reg = Registry::new();
        reg.register(Arc::new(Stub::new(
            "dev.oxcost.sample-echo",
            &["sample-echo/echo-1"],
        )))
        .unwrap();
        let drained = reg.drain();
        assert_eq!(drained.len(), 1);
        assert!(reg.is_empty());
    }
}
