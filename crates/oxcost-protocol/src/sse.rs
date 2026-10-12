//! SSE event shapes for streamed chat completions.
//!
//! A stream is a sequence of `data: <json chunk>` events terminated by
//! `data: [DONE]`. Only the event payloads are modeled here; framing is the
//! transport layer's concern (M1.5+).

use serde::{Deserialize, Serialize};

/// One streamed chunk: `data: {"object":"chat.completion.chunk",...}`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SseEvent {
    /// Chunk id, e.g. `chatcmpl-abc123` (same across the stream).
    pub id: String,
    /// Object type, always `chat.completion.chunk`.
    #[serde(rename = "object")]
    pub object_type: String,
    /// Unix timestamp (seconds) of creation.
    pub created: u64,
    /// Model name as returned by the upstream.
    pub model: String,
    /// Usually exactly one delta choice per chunk.
    pub choices: Vec<ChunkChoice>,
}

/// One choice inside a streamed chunk.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChunkChoice {
    pub index: u32,
    pub delta: Delta,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub finish_reason: Option<String>,
}

/// Incremental message content.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Delta {
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub role: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub content: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub tool_calls: Option<Vec<ToolCallDelta>>,
}

/// Incremental tool call fragment.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolCallDelta {
    pub index: u32,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub function: Option<ToolCallFunctionDelta>,
}

/// Incremental function fragment inside a tool call delta.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolCallFunctionDelta {
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub arguments: Option<String>,
}

/// Terminal sentinel of a stream: `data: [DONE]`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SseDone;

impl SseDone {
    /// The literal wire token for the end-of-stream event.
    pub const WIRE: &'static str = "[DONE]";
}
