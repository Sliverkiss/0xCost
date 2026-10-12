//! OxCost in-process plugin SDK (contract §10 `host-sdk` form): Rust
//! plugins compiled into the host binary as first-class citizens.
//!
//! A plugin author implements exactly one trait — [`OxcostPlugin`] — and
//! hands an `Arc` of it to [`Registry::register`] from host `main`. The
//! contract is single across all four plugin forms: the `插件名/模型名`
//! prefix rule, capability whitelist, `unknown_model` policy, and §4 error
//! vocabulary apply identically; [`SdkManifest`] reuses the contract
//! crate's §3 validation rather than redefining any rule.
//!
//! ```no_run
//! use std::sync::Arc;
//! use oxcost_sdk::{OxcostPlugin, Registry, SampleEcho};
//!
//! let mut registry = Registry::new();
//! registry.register(Arc::new(SampleEcho::new(&[]))).expect("valid plugin");
//! ```

pub mod descriptor;
pub mod manifest;
pub mod plugin;
pub mod registry;
pub mod sample_echo;
pub mod types;

pub use descriptor::ModelDescriptor;
pub use manifest::{SdkManifest, SdkManifestError};
pub use plugin::{ChatFuture, OxcostPlugin, SharedPlugin};
pub use registry::{RegistrationError, Registry};
pub use sample_echo::SampleEcho;
pub use types::{ChatOutcome, ChatRequest, HealthStatus, PluginError, RequestContext};
