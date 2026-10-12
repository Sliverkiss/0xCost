//! PluginInstance: the host's runtime view of one registered plugin —
//! transport handle (sidecar HTTP vs in-process SDK), routing fields,
//! capability set, and a state handle. Pure data; the actual dispatch
//! (HTTP forwarding or SDK trait call) lives in the `oxcost` binary. Kept
//! free of I/O so routing decisions are unit-testable.

use crate::handle::PluginHandle;
use crate::lifecycle::PluginState;
use oxcost_contract::{CapabilitySet, EntryMode, PluginManifest, UnknownModelPolicy};
use std::sync::{Arc, Mutex};

/// Where a plugin's key material comes from at request time.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum KeySource {
    /// Read the value of this environment variable when forwarding.
    Env(String),
    /// host-spawn mode: the host generated the key at spawn time.
    HostGenerated,
    /// No key; the plugin must be relying on loopback-only trust (§4).
    None,
}

/// Routing decision for a requested model id (contract §5).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RouteDecision {
    /// The model is served by this plugin; send `upstream_model` upstream
    /// (the `plugin-name/` prefix stripped).
    Forward {
        /// Model id to send upstream.
        upstream_model: String,
    },
    /// The model is not declared and the policy says refuse.
    Reject,
}

/// One live plugin inside the host.
#[derive(Debug)]
pub struct PluginInstance {
    manifest: PluginManifest,
    /// Transport handle: how requests reach this plugin's chat surface.
    handle: PluginHandle,
    capabilities: CapabilitySet,
    /// Shared lifecycle state; the host advances it via the registry/state
    /// machine, the instance reads it when routing.
    state: Arc<Mutex<PluginState>>,
}

impl PluginInstance {
    /// Build a sidecar instance from a validated manifest and its resolved
    /// key source. Endpoint URLs are derived from `[entry]`:
    /// - external → `http://127.0.0.1:<port>`
    /// - remote   → the declared base url (trailing slashes normalized away)
    /// - host-spawn → the port the host assigned at spawn
    pub fn new(manifest: PluginManifest, key_source: KeySource) -> Self {
        let endpoint = match manifest.entry.mode {
            EntryMode::External => {
                let port = manifest
                    .entry
                    .port
                    .expect("validated external manifests carry a port");
                format!("http://127.0.0.1:{port}")
            }
            EntryMode::Remote => {
                let mut url = manifest
                    .entry
                    .url
                    .clone()
                    .expect("validated remote manifests carry a url");
                while url.ends_with('/') {
                    url.pop();
                }
                url
            }
            EntryMode::HostSpawn => String::new(), // assigned when the host spawns the process
        };
        let capabilities = CapabilitySet::from_section(&manifest.capabilities);
        Self {
            manifest,
            handle: PluginHandle::Sidecar {
                endpoint,
                key_source,
            },
            capabilities,
            state: Arc::new(Mutex::new(PluginState::Registered)),
        }
    }

    /// Build an in-process instance (contract §10): the manifest is the
    /// SDK manifest converted to the shared contract shape, the handle is
    /// the trait object itself. No endpoint, no key source — dispatch is a
    /// direct call.
    pub fn from_sdk_plugin(plugin: std::sync::Arc<dyn oxcost_sdk::OxcostPlugin>) -> Self {
        let manifest = plugin.manifest().to_contract_manifest();
        let capabilities = CapabilitySet::from_section(&manifest.capabilities);
        Self {
            manifest,
            handle: PluginHandle::Sdk(plugin),
            capabilities,
            state: Arc::new(Mutex::new(PluginState::Registered)),
        }
    }

    /// Plugin id.
    pub fn id(&self) -> &str {
        &self.manifest.id
    }

    /// Display name (model prefix).
    pub fn name(&self) -> &str {
        &self.manifest.name
    }

    /// Transport handle (sidecar HTTP vs in-process SDK dispatch).
    pub fn handle(&self) -> &PluginHandle {
        &self.handle
    }

    /// Base endpoint URL (no trailing slash). Empty for SDK plugins —
    /// they have no endpoint; use [`Self::handle`] for dispatch.
    pub fn endpoint(&self) -> &str {
        match &self.handle {
            PluginHandle::Sidecar { endpoint, .. } => endpoint,
            PluginHandle::Sdk(_) => "",
        }
    }

    /// Full URL for one route path, e.g. `/v1/chat/completions`.
    /// Sidecar plugins only; panics on SDK plugins (no endpoint exists —
    /// call sites dispatch on the handle before reaching for URLs).
    pub fn url_for(&self, path: &str) -> String {
        match &self.handle {
            PluginHandle::Sidecar { endpoint, .. } => format!("{endpoint}{path}"),
            PluginHandle::Sdk(_) => panic!("SDK plugins have no endpoint URL"),
        }
    }

    /// Resolved key source (sidecar plugins only).
    pub fn key_source(&self) -> Option<&KeySource> {
        match &self.handle {
            PluginHandle::Sidecar { key_source, .. } => Some(key_source),
            PluginHandle::Sdk(_) => None,
        }
    }

    /// Declared capabilities.
    pub fn capabilities(&self) -> &CapabilitySet {
        &self.capabilities
    }

    /// The manifest backing this instance.
    pub fn manifest(&self) -> &PluginManifest {
        &self.manifest
    }

    /// Shared state handle (kept in sync with the registry entry).
    pub fn state_handle(&self) -> Arc<Mutex<PluginState>> {
        Arc::clone(&self.state)
    }

    /// Current lifecycle state snapshot.
    pub fn state(&self) -> PluginState {
        *self.state.lock().expect("state mutex poisoned")
    }

    /// `routing.enabled` (§6 manual switch).
    pub fn routing_enabled(&self) -> bool {
        self.manifest.routing.enabled
    }

    /// Should this instance serve traffic right now? (§6: routable states
    /// only, and the manual switch respected.)
    pub fn is_servable(&self) -> bool {
        self.routing_enabled() && self.state().is_routable()
    }

    /// Decide whether this instance serves `requested_model` (contract §5
    /// model routing + the plugin's `unknown_model` policy).
    pub fn route(&self, requested_model: &str) -> RouteDecision {
        // Exact match on the full prefixed id first.
        if self
            .manifest
            .routing
            .models
            .iter()
            .any(|m| m == requested_model)
        {
            let upstream_model = requested_model
                .split_once('/')
                .map(|(_, rest)| rest.to_string())
                .unwrap_or_else(|| requested_model.to_string());
            return RouteDecision::Forward { upstream_model };
        }
        let prefix = format!("{}/", self.manifest.name);
        match &self.manifest.routing.unknown_model {
            UnknownModelPolicy::Reject => RouteDecision::Reject,
            UnknownModelPolicy::Pass => {
                // Pass through with the plugin prefix stripped when present;
                // a bare (unprefixed) name goes upstream verbatim.
                let upstream_model = requested_model
                    .strip_prefix(&prefix)
                    .map(str::to_string)
                    .unwrap_or_else(|| requested_model.to_string());
                RouteDecision::Forward { upstream_model }
            }
            UnknownModelPolicy::Fallback(target) => {
                // §3: `fallback=<model>` — target is a full prefixed id of
                // this plugin (validation enforces the prefix).
                let upstream_model = target
                    .strip_prefix(&prefix)
                    .map(str::to_string)
                    .unwrap_or_else(|| target.to_string());
                RouteDecision::Forward { upstream_model }
            }
        }
    }

    /// Model ids this instance advertises in `/v1/models` (declared list).
    pub fn advertised_models(&self) -> &[String] {
        &self.manifest.routing.models
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use oxcost_contract::parse_manifest;

    fn external_manifest() -> PluginManifest {
        parse_manifest(
            r#"
manifestVersion = "0.1"
id = "dev.oxcost.mimo2api"
name = "mimo2api"
version = "1.0.0"
apiVersion = "v1"
license = "AGPL-3.0"

[entry]
mode = "external"
port = 21223

[health]
path = "/healthz"
interval_secs = 30
timeout_secs = 5

[routing]
models = ["mimo2api/mimo-v2.6-pro", "mimo2api/mimo-v2.6-flash"]
unknown_model = "reject"

[capabilities]
chat = true

[auth]
keys = "env:MIMO2API_API_KEYS"
"#,
        )
        .expect("fixture must parse")
    }

    fn instance() -> PluginInstance {
        PluginInstance::new(
            external_manifest(),
            KeySource::Env("MIMO2API_API_KEYS".into()),
        )
    }

    #[test]
    fn external_endpoint_derived_from_port() {
        let inst = instance();
        assert_eq!(inst.endpoint(), "http://127.0.0.1:21223");
        assert_eq!(
            inst.url_for("/v1/chat/completions"),
            "http://127.0.0.1:21223/v1/chat/completions"
        );
    }

    #[test]
    fn remote_endpoint_trims_trailing_slash() {
        let mut m = external_manifest();
        m.id = "dev.oxcost.relay".into();
        m.name = "relay".into();
        m.entry = oxcost_contract::EntrySection {
            mode: EntryMode::Remote,
            cmd: None,
            port: None,
            url: Some("https://relay.example.com/v1/".into()),
        };
        m.health = None;
        m.routing.models = vec!["relay/gpt-x".into()];
        let inst = PluginInstance::new(m, KeySource::None);
        assert_eq!(inst.endpoint(), "https://relay.example.com/v1");
    }

    #[test]
    fn declared_model_routes_with_prefix_stripped() {
        let inst = instance();
        assert_eq!(
            inst.route("mimo2api/mimo-v2.6-flash"),
            RouteDecision::Forward {
                upstream_model: "mimo-v2.6-flash".into()
            }
        );
    }

    #[test]
    fn unknown_model_reject_policy() {
        let inst = instance();
        assert_eq!(inst.route("mimo2api/nope"), RouteDecision::Reject);
        assert_eq!(inst.route("other/whatever"), RouteDecision::Reject);
    }

    #[test]
    fn pass_policy_strips_prefix_and_forwards_bare() {
        let mut m = external_manifest();
        m.routing.unknown_model = UnknownModelPolicy::Pass;
        let inst = PluginInstance::new(m, KeySource::None);
        assert_eq!(
            inst.route("mimo2api/some-undeclared"),
            RouteDecision::Forward {
                upstream_model: "some-undeclared".into()
            }
        );
        assert_eq!(
            inst.route("bare-name"),
            RouteDecision::Forward {
                upstream_model: "bare-name".into()
            }
        );
    }

    #[test]
    fn state_and_servability() {
        let inst = instance();
        assert_eq!(inst.state(), PluginState::Registered);
        assert!(!inst.is_servable());

        *inst.state_handle().lock().unwrap() = PluginState::Serving;
        assert!(inst.is_servable());

        // Manual switch overrides state.
        let mut m = external_manifest();
        m.routing.enabled = false;
        let inst = PluginInstance::new(m, KeySource::None);
        *inst.state_handle().lock().unwrap() = PluginState::Serving;
        assert!(
            !inst.is_servable(),
            "routing.enabled=false must exclude from serving"
        );
    }

    #[test]
    fn capabilities_and_models_reflected() {
        let inst = instance();
        assert!(inst
            .capabilities()
            .contains(oxcost_contract::Capability::Chat));
        assert!(!inst
            .capabilities()
            .contains(oxcost_contract::Capability::WebSearch));
        assert_eq!(inst.advertised_models().len(), 2);
        assert_eq!(inst.id(), "dev.oxcost.mimo2api");
        assert_eq!(inst.name(), "mimo2api");
    }

    // ---- SDK-plugin instances (contract §10) ----

    mod sdk {
        use super::*;
        use oxcost_sdk::{
            ChatOutcome, ChatRequest, HealthStatus, ModelDescriptor, RequestContext, SampleEcho,
            SdkManifest,
        };

        /// Unhealthy stub plugin for state tests.
        struct UnhealthyEcho(SampleEcho);

        impl oxcost_sdk::OxcostPlugin for UnhealthyEcho {
            fn manifest(&self) -> &SdkManifest {
                self.0.manifest()
            }
            fn models(&self) -> Vec<ModelDescriptor> {
                self.0.models()
            }
            fn chat(&self, req: ChatRequest, ctx: RequestContext) -> oxcost_sdk::ChatFuture<'_> {
                self.0.chat(req, ctx)
            }
            fn health(&self) -> HealthStatus {
                HealthStatus::Unhealthy
            }
        }

        #[test]
        fn sdk_instance_routes_like_sidecar() {
            let inst = PluginInstance::from_sdk_plugin(Arc::new(SampleEcho::new(&[])));
            assert_eq!(inst.id(), "dev.oxcost.sample-echo");
            assert_eq!(inst.name(), "sample-echo");
            assert!(matches!(inst.handle(), PluginHandle::Sdk(_)));
            // Same namespace semantics as the sidecar form.
            assert_eq!(
                inst.route("sample-echo/echo-1"),
                RouteDecision::Forward {
                    upstream_model: "echo-1".into()
                }
            );
            assert_eq!(inst.route("sample-echo/ghost"), RouteDecision::Reject);
            assert!(inst
                .capabilities()
                .contains(oxcost_contract::Capability::Chat));
            assert_eq!(inst.advertised_models(), ["sample-echo/echo-1"]);
            // No endpoint/key for the in-process form.
            assert_eq!(inst.endpoint(), "");
            assert!(inst.key_source().is_none());
        }

        #[test]
        fn sdk_instance_lifecycle_works_identically() {
            let inst = PluginInstance::from_sdk_plugin(Arc::new(SampleEcho::new(&[])));
            assert_eq!(inst.state(), PluginState::Registered);
            *inst.state_handle().lock().unwrap() = PluginState::Serving;
            assert!(inst.is_servable());

            let un = PluginInstance::from_sdk_plugin(Arc::new(UnhealthyEcho(SampleEcho::new(&[]))));
            *un.state_handle().lock().unwrap() = PluginState::Serving;
            assert!(
                un.is_servable(),
                "health() is read at probe time, not by routing"
            );
        }

        #[test]
        fn sdk_handle_chat_dispatch_reaches_plugin() {
            let inst = PluginInstance::from_sdk_plugin(Arc::new(SampleEcho::new(&[])));
            let PluginHandle::Sdk(plugin) = inst.handle() else {
                panic!("expected SDK handle")
            };
            let req = ChatRequest::new(serde_json::json!({
                "model": "echo-1",
                "messages": [{"role": "user", "content": "ping"}]
            }))
            .unwrap();
            let outcome = tokio::runtime::Runtime::new()
                .unwrap()
                .block_on(async { plugin.chat(req, RequestContext::default()).await });
            match outcome {
                Ok(ChatOutcome::Json(v)) => {
                    assert_eq!(v["choices"][0]["message"]["content"], "echo: ping");
                }
                other => panic!("unexpected outcome {other:?}"),
            }
        }
    }
}
