//! OxCost core: plugin registry, lifecycle state machine, runtime plugin
//! instances and the transport-form unification handle (pure state, no
//! I/O in this crate; HTTP forwarding lives in the `oxcost` binary).

pub mod handle;
pub mod instance;
pub mod lifecycle;
pub mod registry;

pub use handle::PluginHandle;
pub use instance::{KeySource, PluginInstance, RouteDecision};
pub use lifecycle::{InvalidTransition, PluginEntry, PluginState, TransitionEvent};
pub use registry::{PluginRegistry, RegistryError};
