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
//! - [`sse`] — the JSON-patch SSE interpreter (Go `internal/sse`).
//! - [`openai`] — request parsing/sanitize/merge + switch decoding (Go
//!   `internal/openai`, parsing half).
//! - [`image_fetch`] — image extraction with the SSRF-guarded fetch.
//! - [`oai_response`] — OpenAI response emission (Go `response.go`).
//! - [`errmap`] — the §6 error table → contract §4 wire errors.
//! - [`chat_flow`] — the chat pipeline + retry ladder + delivery (Go
//!   `server.go`'s handler core).
//! - [`plugin`] — the `OxcostPlugin` facade (manifest, chat, health).

mod account;
mod body;
mod chat_flow;
mod client;
mod device;
mod errmap;
mod headers;
mod image_fetch;
mod lease;
mod manager;
mod oai_response;
mod openai;
mod plugin;
mod pool;
pub mod pow;
pub mod seed;
mod sse;
mod store;
mod timeutil;
mod wire;

pub use account::{Account, AccountError, BanState};
pub use body::{completion_body, login_body, normalize_mobile, CompletionRequest};
pub use chat_flow::{run_chat, ChatJob, DEFAULT_MAX_PROMPT_CHARS, MAX_ATTEMPTS};
pub use client::{Client, WatchedStream};
pub use device::{DeviceProfile, DEVICE_ID_NAMESPACE};
pub use errmap::WireError;
pub use headers::{
    base_headers, APP_BUNDLE_ID, APP_CLIENT_LOCALE, APP_CLIENT_VERSION, APP_DEVICE_MODEL,
    APP_TIMEZONE_OFF, APP_USER_AGENT,
};
pub use image_fetch::{extract_images, is_non_public, Image, ImageFetchError};
pub use lease::{AccountStatus, Lease};
pub use manager::AccountManager;
pub use oai_response::{
    completion_response, error_body, final_chunk, models_response, reasoning_stream_chunk,
    sse_done_frame, sse_frame, stream_chunk, web_search_response,
};
pub use openai::{
    flatten_messages, image_ext_by_mime, parse_request, parse_search_switch, parse_thinking_switch,
    ContentPart, Message, ParseError, PartKind, Request, MODEL_NAME, SYSTEM_MERGE_SEPARATOR,
};
pub use plugin::{
    PluginBuildError, SimpleChatConfig, SimpleChatPlugin, ENV_DB_PATH, ENV_MAX_INFLIGHT,
    ENV_MAX_PROMPT_CHARS, ENV_QUEUE_WAIT_SECS,
};
pub use pool::{Pool, PoolConfig, PoolError, DEFAULT_MAX_INFLIGHT};
pub use seed::{
    import_records, park_record_from_account, parse_account_reply, parse_index_reply,
    ImportSummary, SeedError, CONCURRENCY_WARNING,
};
pub use sse::{Delta, Interpreter, SearchResult, State, StreamError, Usage};
pub use store::{LoginRecord, ParkRecord, SqliteStore, StoreError};
pub use wire::{
    check_env, decode_envelope, parse_mute_until, Config, Envelope, EnvelopeData, SessionInfo,
    SessionPage, UpstreamError, DEFAULT_BASE_URL, PREFLIGHT_TIMEOUT, STREAM_IDLE_TIMEOUT,
};
