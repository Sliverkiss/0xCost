//! Chat completion request/response message types (OpenAI shapes).
//!
//! Structs keep the canonical JSON field names via `#[serde(rename_all =
//! "snake_case")]` / explicit renames. Optional fields round-trip as absent
//! keys (not `null`) to match OpenAI wire behavior.

use serde::{Deserialize, Serialize};

/// Chat role. Assistant/tool/function roles kept distinct per OpenAI spec.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Role {
    System,
    User,
    Assistant,
    Tool,
    Function,
}

/// Stop reason for a completion choice.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FinishReason {
    Stop,
    Length,
    ToolCalls,
    ContentFilter,
    FunctionCall,
}

/// One message in a chat completion request.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "role", rename_all = "snake_case")]
pub enum ChatMessage {
    System(SystemMessage),
    User(UserMessage),
    Assistant(AssistantMessage),
    Tool(ToolMessage),
    Function(FunctionMessage),
}

/// `role: system` message.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SystemMessage {
    pub content: String,
}

/// `role: user` message; content may be a plain string or multimodal parts.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct UserMessage {
    pub content: UserContent,
}

/// Multimodal user content (text / image_url parts).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum UserContent {
    Text(String),
    Parts(Vec<UserContentPart>),
}

/// One part of multimodal user content.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum UserContentPart {
    Text { text: String },
    ImageUrl { image_url: ImageUrl },
}

/// Image reference inside a multimodal part.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ImageUrl {
    pub url: String,
}

/// `role: assistant` message; tool_calls present only in tool-use turns.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AssistantMessage {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub tool_calls: Option<Vec<ToolCall>>,
}

/// `role: tool` message: output of one tool invocation.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolMessage {
    pub tool_call_id: String,
    pub content: String,
}

/// `role: function` (legacy) message.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct FunctionMessage {
    pub name: String,
    pub content: String,
}

/// A tool invocation requested by the assistant.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolCall {
    /// Tool call id, e.g. `call_abc123`.
    pub id: String,
    /// Always `function` in the current spec.
    #[serde(rename = "type")]
    pub call_type: String,
    pub function: ToolCallFunction,
}

/// Function name + JSON-encoded arguments inside a tool call.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolCallFunction {
    pub name: String,
    /// JSON-encoded arguments string, per OpenAI wire format.
    pub arguments: String,
}

/// Tool exposed to the model.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Tool {
    #[serde(rename = "type")]
    pub tool_type: String,
    pub function: FunctionDefinition,
}

/// Function signature for a tool.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct FunctionDefinition {
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub description: Option<String>,
    /// JSON Schema of the parameters; arbitrary JSON object.
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub parameters: Option<serde_json::Value>,
}

/// Tool selection strategy.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum ToolChoice {
    Mode(ToolChoiceMode),
    Function(ToolChoiceFunction),
}

/// String modes for `tool_choice`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolChoiceMode {
    None,
    Auto,
    Required,
}

/// Forced specific function selection.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolChoiceFunction {
    #[serde(rename = "type")]
    pub choice_type: String,
    pub function: ToolFunction,
}

/// Function name inside a forced tool choice.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ToolFunction {
    pub name: String,
}

/// `POST /v1/chat/completions` request body.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChatCompletionRequest {
    pub model: String,
    pub messages: Vec<ChatMessage>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub temperature: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub top_p: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub n: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub stream: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub stop: Option<StopSeq>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub max_tokens: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub tools: Option<Vec<Tool>>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub tool_choice: Option<ToolChoice>,
}

/// `stop` is either a single string or a list of up to 4 strings.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum StopSeq {
    One(String),
    Many(Vec<String>),
}

/// `POST /v1/chat/completions` response body.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChatCompletionResponse {
    /// Object id, e.g. `chatcmpl-abc123`.
    pub id: String,
    /// Object type, always `chat.completion`.
    #[serde(rename = "object")]
    pub object_type: String,
    /// Unix timestamp (seconds) of creation.
    pub created: u64,
    /// Model name as returned by the upstream.
    pub model: String,
    pub choices: Vec<ChatChoice>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub usage: Option<Usage>,
}

/// One choice in a completion response.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ChatChoice {
    pub index: u32,
    pub message: AssistantMessage,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub finish_reason: Option<FinishReason>,
}

/// Token accounting.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct Usage {
    pub prompt_tokens: u64,
    pub completion_tokens: u64,
    pub total_tokens: u64,
}
