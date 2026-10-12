//! OxCost host library surface: the bin target in `main.rs` is a thin
//! assembly; everything reusable (server, forwarding, state, loader) lives
//! here so integration tests can drive the real code paths.

pub mod config;
pub mod forward;
pub mod loader;
pub mod server;
pub mod state;
