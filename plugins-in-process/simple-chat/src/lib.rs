//! `simple-chat` — in-process OxCost plugin (contract §10 host-sdk form):
//! DeepSeek web-chat `deepseek-flash` with default-on reasoning and opt-in
//! web search, ported from the Go `simple-chat` gateway.
//!
//! Architecture (all plugin-internal, no admin surface — PORT_DESIGN F13):
//!
//! - [`account`] — the Go `upstream.Account` shape + park-state semantics
//!   (R3: fail-closed latch, unknown-deadline quarantine, monotonic parks).
//! - [`timeutil`] — RFC3339 UTC parsing/formatting (the store's date format).
//! - [`device`] — deterministic device identity minting (Go `region.go`).
//! - [`store`] — plugin-private SQLite backing (R3 latch, generation
//!   fencing, expired-park stripping at load).
//! - [`seed`] — PC-41 Upstash import parsing (read-only; the `sc-seed`
//!   maintenance CLI wraps it).
//! - [`pow`] — HashV1 proof-of-work (Keccak-f rounds 1..23).
//!
//! Upstream wire client, SSE interpreter, account pool, and the
//! `OxcostPlugin` facade land in subsequent modules.

mod account;
mod device;
pub mod pow;
pub mod seed;
mod store;
mod timeutil;

pub use account::{Account, AccountError, BanState};
pub use device::{DeviceProfile, DEVICE_ID_NAMESPACE};
pub use seed::{
    import_records, park_record_from_account, parse_account_reply, parse_index_reply,
    ImportSummary, SeedError, CONCURRENCY_WARNING,
};
pub use store::{LoginRecord, ParkRecord, SqliteStore, StoreError};
