//! OxCost core: plugin registry and lifecycle state machine (pure, sync,
//! no I/O). Routing/LB/circuit-breaker *behavior* arrives in M1.5/M2.

pub mod instance;
pub mod lifecycle;
pub mod registry;

pub use instance::{KeySource, PluginInstance, RouteDecision};
pub use lifecycle::{InvalidTransition, PluginEntry, PluginState, TransitionEvent};
pub use registry::{PluginRegistry, RegistryError};
