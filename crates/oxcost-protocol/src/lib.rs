//! OpenAI wire types: chat completions request/response and SSE event shapes.
//! Data structs + serde only; no I/O, no host logic.

pub mod chat;
pub mod sse;

pub use chat::{
    AssistantMessage, ChatChoice, ChatCompletionRequest, ChatCompletionResponse, ChatMessage,
    FinishReason, FunctionDefinition, FunctionMessage, Role, StopSeq, SystemMessage, Tool,
    ToolCall, ToolCallFunction, ToolChoice, ToolChoiceFunction, ToolChoiceMode, ToolFunction,
    ToolMessage, Usage, UserContent, UserContentPart, UserMessage,
};
pub use sse::{SseDone, SseEvent};
