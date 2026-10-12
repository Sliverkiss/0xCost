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
//! - [`wire`] — response envelope, error taxonomy + classifiers.
//! - [`headers`] — the android-app impersonation header table.
//! - [`body`] — login/completion request body serializers (kotlinx order).
//! - [`client`] — the live reqwest client + R2 watched stream.
//!
//! SSE interpreter, account pool, and the `OxcostPlugin` facade land in
//! subsequent modules.

mod account;
mod body;
mod client;
mod device;
mod headers;
pub mod pow;
pub mod seed;
mod store;
mod timeutil;
mod wire;

pub use account::{Account, AccountError, BanState};
pub use body::{completion_body, login_body, normalize_mobile, CompletionRequest};
pub use client::{Client, WatchedStream};
pub use device::{DeviceProfile, DEVICE_ID_NAMESPACE};
pub use headers::{
    base_headers, APP_BUNDLE_ID, APP_CLIENT_LOCALE, APP_CLIENT_VERSION, APP_DEVICE_MODEL,
    APP_TIMEZONE_OFF, APP_USER_AGENT,
};
pub use seed::{
    import_records, park_record_from_account, parse_account_reply, parse_index_reply,
    ImportSummary, SeedError, CONCURRENCY_WARNING,
};
pub use store::{LoginRecord, ParkRecord, SqliteStore, StoreError};
pub use wire::{
    check_env, decode_envelope, parse_mute_until, Config, Envelope, EnvelopeData, SessionInfo,
    SessionPage, UpstreamError, DEFAULT_BASE_URL, PREFLIGHT_TIMEOUT, STREAM_IDLE_TIMEOUT,
};
