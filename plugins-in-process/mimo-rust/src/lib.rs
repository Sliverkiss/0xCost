//! mimo-rust: the mimo2api (Go) port as an in-process Oxcost plugin.
//!
//! Chat (+models) against the MiMo AI Studio web backend, driven through the
//! host SDK trait. Layout (PORT_DESIGN §7, single-crate form):
//!
//! - [`models`] — static catalog, normalization, fastchat routing
//! - [`thinking`] — ThinkSplitter (reasoning/content marker state machine)
//! - `sse` — upstream SSE dialect: FrameExtractor + event dispatch
//! - `openai` — OpenAI-shape shapers (chunks, usage, completion)
//! - `citations` — web_search payload parsing + annotations
//! - `pool` — account pool state machine (R1 port) + SQLite store
//! - `login` — passport login chain, header sets, cookie jar
//! - `chat` — retry-ladder pipeline
//! - `config` — `MIMO_RUST_*` environment contract
//!
//! v1 scope: chat only. tts/asr/voiceclone are UNIMPLEMENTED-reserved
//! (capability flags false); the `OxcostPlugin` implementation lands once
//! the modules above are in place.

pub mod login;
pub mod models;
pub mod pool;
pub mod sse;
pub mod store;
pub mod thinking;
pub mod turnplan;
