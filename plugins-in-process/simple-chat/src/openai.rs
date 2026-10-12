//! OpenAI request parsing — port of Go `internal/openai/openai.go` (the
//! parsing half): ParseRequest, message sanitize/merge, thinking + search
//! switches, FlattenMessages, and the pure image-URL helpers. The live
//! http(s) image fetch lives in [`crate::image_fetch`]; response emission
//! in [`crate::oai_response`].
//!
//! Sanitization is silent by design (docs-spec-field-strip.md): the
//! upstream has no tool calling and no system role, so tool-calling
//! fields are stripped — never rejected — and system messages are merged
//! into the first user message rather than dropped.

use serde::Deserialize;

/// The only model this plugin serves (unprefixed; the host strips the
/// `simple-chat/` prefix before the request reaches the plugin).
pub const MODEL_NAME: &str = "deepseek-flash";

/// Separator between the merged system block and the original first-user
/// content: blocks joined with a blank line, then one `---` divider line
/// (docs-spec-field-strip.md).
pub const SYSTEM_MERGE_SEPARATOR: &str = "\n\n---\n\n";

/// One element of a multipart content array.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ContentPart {
    /// `text` or `image_url` (junk types are dropped at parse).
    pub kind: PartKind,
    /// The text of a `text` part.
    pub text: String,
    /// The URL of an `image_url` part (OpenAI nests it under
    /// `image_url: {url}`).
    pub image_url: String,
}

/// Content-part kinds that survive parsing.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum PartKind {
    #[default]
    Text,
    ImageUrl,
}

/// One chat message. Content is either a plain string (`content`) or a
/// multipart array (`parts`) — parsing accepts both, mirroring Go's
/// `Message.UnmarshalJSON` duality. Junk part counts and the `name` flag
/// feed the strip report; tool_calls/function_call/tool_call_id are never
/// parsed — their residue shows up as a dropped empty message in
/// `sanitize_messages`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Message {
    pub role: String,
    pub content: String,
    pub parts: Vec<ContentPart>,
}

impl Message {
    /// A system message's usable content: the plain string when present,
    /// otherwise its text parts joined with newlines. Empty result means
    /// the message carries nothing worth merging (Go `systemText`).
    pub fn system_text(&self) -> String {
        if !self.content.is_empty() {
            return self.content.clone();
        }
        self.parts
            .iter()
            .filter(|p| p.kind == PartKind::Text && !p.text.is_empty())
            .map(|p| p.text.as_str())
            .collect::<Vec<_>>()
            .join("\n")
    }
}

/// The parsed chat-completions request (Go `Request`).
#[derive(Debug, Clone, PartialEq)]
pub struct Request {
    pub model: String,
    pub messages: Vec<Message>,
    pub stream: bool,
    pub temperature: f64,
    pub top_p: f64,
    pub max_tokens: i64,
    /// Deep-thinking state sent upstream. Default (switch absent) = true.
    pub thinking_enabled: bool,
    /// Web-search state sent upstream. Default (switch absent) = false.
    pub search_enabled: bool,
    /// What sanitization removed or restructured — field names plus
    /// counters like `system_messages=2`. Debug log line; nothing else.
    pub stripped: Vec<String>,
}

/// Parsing failure (each maps to 400 `bad_request` per §6 — the caller
/// decides the wire shape).
#[derive(Debug, Clone, PartialEq, thiserror::Error)]
pub enum ParseError {
    #[error("invalid JSON body: {0}")]
    InvalidJson(String),
    #[error("unknown model {model:?}: this proxy serves {serves:?} only")]
    UnknownModel { model: String, serves: &'static str },
    #[error("invalid messages: {0}")]
    InvalidMessages(String),
    #[error("messages must not be empty")]
    EmptyMessages,
    #[error(
        "messages must not be empty (all messages were removed by sanitization: tool calls are not supported and system messages had no usable content)"
    )]
    StrippedEverything,
    #[error(r#"invalid "thinking" field: expected {{"type": "enabled"|"disabled"}}"#)]
    InvalidThinkingSwitch,
    #[error(r#"invalid "thinking.type" {ty:?}: expected "enabled" or "disabled""#)]
    InvalidThinkingType { ty: String },
    #[error(r#"invalid "search" field: expected {{"type": "enabled"|"disabled"}}"#)]
    InvalidSearchSwitch,
    #[error(r#"invalid "search.type" {ty:?}: expected "enabled" or "disabled""#)]
    InvalidSearchType { ty: String },
    #[error("content must be a string or an array of content parts")]
    InvalidContent,
}

/// The raw request mirror: kept fields plus the known-junk fields
/// (enumerated only so stripping is visible in the debug log). Anything
/// not listed is dropped by serde's unknown-key handling, exactly as Go's
/// `json.Unmarshal` ignores unknown keys.
#[derive(Debug, Default, Deserialize)]
struct RawRequest {
    #[serde(default)]
    model: String,
    #[serde(default)]
    messages: Option<serde_json::Value>,
    #[serde(default)]
    stream: bool,
    #[serde(default)]
    temperature: f64,
    #[serde(default)]
    top_p: f64,
    #[serde(default)]
    max_tokens: i64,
    #[serde(default)]
    max_completion_tokens: i64,
    #[serde(default)]
    thinking: Option<serde_json::Value>,
    #[serde(default)]
    search: Option<serde_json::Value>,

    #[serde(default)]
    tools: JunkField,
    #[serde(default)]
    tool_choice: JunkField,
    #[serde(default)]
    functions: JunkField,
    #[serde(default)]
    function_call: JunkField,
    #[serde(default)]
    parallel_tool_calls: JunkField,
    #[serde(default)]
    logprobs: JunkField,
    #[serde(default)]
    top_logprobs: JunkField,
    #[serde(default)]
    logit_bias: JunkField,
    #[serde(default)]
    user: JunkField,
    #[serde(default)]
    store: JunkField,
    #[serde(default)]
    metadata: JunkField,
    #[serde(default)]
    service_tier: JunkField,
    #[serde(default)]
    response_format: JunkField,
    #[serde(default)]
    seed: JunkField,
    #[serde(default)]
    stop: JunkField,
    #[serde(default)]
    n: JunkField,
    #[serde(default)]
    stream_options: JunkField,
    #[serde(default)]
    presence_penalty: JunkField,
    #[serde(default)]
    frequency_penalty: JunkField,
    #[serde(default)]
    prediction: JunkField,

    /// OpenAI-native search signals (contract §4 search row): any of
    /// these present turns search on for the request. Not junk — they are
    /// consumed, never reported as stripped.
    #[serde(default)]
    web_search_options: Option<serde_json::Value>,
    #[serde(default)]
    web_search_status: Option<serde_json::Value>,
}

/// One junk request-level field. Only *presence* matters — `null`, `[]`
/// and `0` count as absent (Go `rawJunkPresent` / `strippedFields`).
#[derive(Debug, Default, Deserialize, Clone, PartialEq)]
struct JunkField(Option<serde_json::Value>);

impl JunkField {
    /// Reports whether the field carried an actual value (absent, null,
    /// empty array, and zero all count as absent — a field set to any of
    /// those is invisible to the client anyway).
    fn present(&self) -> bool {
        match &self.0 {
            None | Some(serde_json::Value::Null) => false,
            Some(serde_json::Value::Array(a)) => !a.is_empty(),
            Some(serde_json::Value::Number(n)) => n.as_f64() != Some(0.0),
            _ => true,
        }
    }
}

/// Parse, sanitize, and validate a chat-completions body (Go
/// `ParseRequest`). `body` is the JSON value the host relayed.
pub fn parse_request(body: &serde_json::Value) -> Result<Request, ParseError> {
    let raw: RawRequest =
        serde_json::from_value(body.clone()).map_err(|e| ParseError::InvalidJson(e.to_string()))?;
    if raw.model != MODEL_NAME {
        return Err(ParseError::UnknownModel {
            model: raw.model,
            serves: MODEL_NAME,
        });
    }
    let mut stripped = stripped_fields(&raw);
    let msgs = raw
        .messages
        .as_ref()
        .ok_or_else(|| ParseError::InvalidMessages("missing".into()))?;
    let parsed = parse_messages(msgs)?;
    let (msgs, stripped2) = sanitize_messages(parsed, &mut stripped);
    stripped = stripped2;
    if msgs.is_empty() {
        if !stripped.is_empty() {
            return Err(ParseError::StrippedEverything);
        }
        return Err(ParseError::EmptyMessages);
    }
    let thinking = parse_thinking_switch(raw.thinking.as_ref())?;
    let search = parse_search_switch(raw.search.as_ref())?;
    // OpenAI-native search signals map onto the same flag (contract §4
    // search row). `web_search_status: "disabled"` is not one — the
    // contract's disabled/auto/enabled vocabulary maps only `enabled`
    // (and any explicit options object) to search on.
    let search = search || openai_search_signal(&raw);
    // `max_completion_tokens` is OpenAI's current name for the max_tokens
    // knob (max_tokens is the deprecated alias). The alias wins when both
    // are present and nonzero (docs-upstream-params.md).
    let max_tokens = raw.max_tokens.max(raw.max_completion_tokens);
    Ok(Request {
        model: raw.model,
        messages: msgs,
        stream: raw.stream,
        temperature: raw.temperature,
        top_p: raw.top_p,
        max_tokens,
        thinking_enabled: thinking,
        search_enabled: search,
        stripped,
    })
}

/// Parse the OpenAI-native search signals (dossier §5 F03): any
/// `web_search_options` value, or `web_search_status` set to `enabled`
/// (or any non-`disabled` truthy string the contract recognizes).
/// `:online` model suffix stripping is a host routing concern — the
/// plugin model check already rejected anything but `deepseek-flash`.
fn openai_search_signal(raw: &RawRequest) -> bool {
    if raw.web_search_options.is_some() {
        return true;
    }
    matches!(&raw.web_search_status, Some(serde_json::Value::String(s)) if s == "enabled")
}

/// The junk request-level fields present in the body (Go
/// `strippedFields`), in the Go enumeration order.
fn stripped_fields(raw: &RawRequest) -> Vec<String> {
    let present: [(&str, bool); 19] = [
        ("tools", raw.tools.present()),
        ("tool_choice", raw.tool_choice.present()),
        ("functions", raw.functions.present()),
        ("function_call", raw.function_call.present()),
        ("parallel_tool_calls", raw.parallel_tool_calls.present()),
        ("logprobs", raw.logprobs.present()),
        ("top_logprobs", raw.top_logprobs.present()),
        ("logit_bias", raw.logit_bias.present()),
        ("user", raw.user.present()),
        ("store", raw.store.present()),
        ("metadata", raw.metadata.present()),
        ("service_tier", raw.service_tier.present()),
        ("response_format", raw.response_format.present()),
        ("seed", raw.seed.present()),
        ("stop", raw.stop.present()),
        ("n", raw.n.present()),
        ("stream_options", raw.stream_options.present()),
        ("presence_penalty", raw.presence_penalty.present()),
        ("frequency_penalty", raw.frequency_penalty.present()),
    ];
    let _ = raw.prediction; // `prediction` is junk too (Go strips it; the
                            // report list above pins Go's 19 reported
                            // names — prediction presence is dropped
                            // silently, matching Go's report exactly).
    present
        .into_iter()
        .filter(|(_, p)| *p)
        .map(|(name, _)| name.to_string())
        .collect()
}

/// Decode the messages array: each message's content is either a string
/// or an array of parts (Go `Message.UnmarshalJSON`).
fn parse_messages(v: &serde_json::Value) -> Result<Vec<(Message, usize, bool)>, ParseError> {
    let arr = v
        .as_array()
        .ok_or_else(|| ParseError::InvalidMessages("must be an array".into()))?;
    let mut out = Vec::with_capacity(arr.len());
    for item in arr {
        out.push(parse_message(item)?);
    }
    Ok(out)
}

/// Decode one message: role + string-or-parts content duality, junk part
/// counting, `name` counting. Junk parts and name fields are counted at
/// parse (Go carries them on the struct) and surfaced through the
/// sanitize report by [`sanitize_messages`]'s caller-facing counters.
fn parse_message(v: &serde_json::Value) -> Result<(Message, usize, bool), ParseError> {
    #[derive(Deserialize)]
    struct RawMessage {
        role: String,
        #[serde(default)]
        content: serde_json::Value,
        #[serde(default)]
        name: serde_json::Value,
    }
    let m: RawMessage = serde_json::from_value(v.clone())
        .map_err(|e| ParseError::InvalidMessages(e.to_string()))?;
    let mut msg = Message {
        role: m.role,
        ..Default::default()
    };
    let has_name = junk_value_present(&m.name);
    let mut junk = 0usize;
    // Content: absent/null/"" → empty; string → content; array → parts.
    match &m.content {
        serde_json::Value::Null => {}
        serde_json::Value::String(s) => msg.content = s.clone(),
        serde_json::Value::Array(parts) => {
            for p in parts {
                let kind = p.get("type").and_then(serde_json::Value::as_str);
                match kind {
                    Some("image_url") => {
                        let iu = p.get("image_url");
                        // Go requires image_url != nil; a missing nested
                        // object counts as junk.
                        if iu.is_none() {
                            junk += 1;
                            continue;
                        }
                        let url = iu
                            .and_then(|iu| iu.get("url"))
                            .and_then(serde_json::Value::as_str)
                            .unwrap_or("");
                        msg.parts.push(ContentPart {
                            kind: PartKind::ImageUrl,
                            image_url: url.to_string(),
                            text: String::new(),
                        });
                    }
                    Some("text") => {
                        let text = p
                            .get("text")
                            .and_then(serde_json::Value::as_str)
                            .unwrap_or("");
                        msg.parts.push(ContentPart {
                            kind: PartKind::Text,
                            text: text.to_string(),
                            image_url: String::new(),
                        });
                    }
                    // Any other part type (audio/file/video/…) is dropped
                    // and counted.
                    _ => junk += 1,
                }
            }
        }
        _ => return Err(ParseError::InvalidContent),
    }
    Ok((msg, junk, has_name))
}

/// Did a junk *message-level* field carry an actual value? (Go
/// `rawJunkPresent`: absent, null, and empty array count as absent.)
fn junk_value_present(v: &serde_json::Value) -> bool {
    !matches!(v, serde_json::Value::Null)
        && !(v.is_array() && v.as_array().is_some_and(|a| a.is_empty()))
}

/// The message-level sanitization rules (Go `sanitizeMessages`): system
/// messages merged (not dropped) into the first user message; assistant/
/// tool messages left with no content by tool-call stripping are dropped;
/// junk content-part types and name fields were counted at parse and are
/// reported here. Returns survivors plus the appended strip report.
fn sanitize_messages(
    msgs: Vec<(Message, usize, bool)>,
    stripped: &mut Vec<String>,
) -> (Vec<Message>, Vec<String>) {
    let mut system_blocks: Vec<String> = Vec::new();
    let mut empty_tool_count = 0usize;
    let mut junk_part_count = 0usize;
    let mut name_count = 0usize;
    let mut out: Vec<Message> = Vec::with_capacity(msgs.len());
    for (m, junk, has_name) in msgs {
        if m.role == "system" {
            let text = m.system_text();
            if !text.is_empty() {
                system_blocks.push(text);
            }
            // Junk parts inside a system message still count toward the
            // report even though the message itself dissolves into the
            // merge (Go counts junkParts for every message, including
            // system ones that pass through the loop).
            junk_part_count += junk;
            name_count += usize::from(has_name);
            continue;
        }
        if (m.role == "assistant" || m.role == "tool") && m.content.is_empty() && m.parts.is_empty()
        {
            // An assistant carrier with only tool_calls, or an empty tool
            // result — nothing survives stripping, so neither does the
            // message. User messages with empty/junk-stripped content
            // still count as turn boundaries and stay.
            empty_tool_count += 1;
            continue;
        }
        junk_part_count += junk;
        name_count += usize::from(has_name);
        out.push(m);
    }
    if !system_blocks.is_empty() {
        let joined = system_blocks.join("\n\n");
        out = merge_system_blocks(out, &joined);
        stripped.push(format!("system_messages={}", system_blocks.len()));
    }
    if empty_tool_count > 0 {
        stripped.push(format!("tool_messages={empty_tool_count}"));
    }
    if junk_part_count > 0 {
        stripped.push(format!("junk_content_parts={junk_part_count}"));
    }
    if name_count > 0 {
        stripped.push(format!("message_names={name_count}"));
    }
    let out_report = std::mem::take(stripped);
    (out, out_report)
}

/// Prepend the merged system text to the first user message, separated by
/// [`SYSTEM_MERGE_SEPARATOR`] (Go `mergeSystemBlocks`). A conversation
/// without a user message gets a new leading user message carrying only
/// the system text — a system-only transcript is valid input. The
/// separator is omitted when the user message has nothing to separate
/// from (empty string content and no parts).
fn merge_system_blocks(out: Vec<Message>, system: &str) -> Vec<Message> {
    let mut out = out;
    for m in out.iter_mut() {
        if m.role != "user" {
            continue;
        }
        if !m.content.is_empty() {
            m.content = format!("{system}{SYSTEM_MERGE_SEPARATOR}{}", m.content);
        } else if !m.parts.is_empty() {
            m.content = format!("{system}{SYSTEM_MERGE_SEPARATOR}");
        } else {
            m.content = system.to_string();
        }
        return out;
    }
    let mut merged = Vec::with_capacity(out.len() + 1);
    merged.push(Message {
        role: "user".into(),
        content: system.to_string(),
        parts: Vec::new(),
    });
    merged.extend(out);
    merged
}

/// The thinking switch (Go `parseThinkingSwitch`):
///
/// ```text
/// "thinking": {"type": "enabled"}  → true
/// "thinking": {"type": "disabled"} → false
/// absent or null                   → true (deep thinking default ON)
/// ```
///
/// The shape mirrors the openai/anthropic reasoning convention. Anything
/// else is a 400.
pub fn parse_thinking_switch(raw: Option<&serde_json::Value>) -> Result<bool, ParseError> {
    parse_type_switch(
        raw,
        true,
        || ParseError::InvalidThinkingSwitch,
        |ty| ParseError::InvalidThinkingType { ty },
    )
}

/// The search switch (Go `parseSearchSwitch`) — same shape, default OFF
/// (search slows responses and is wrong for bulk text processing).
pub fn parse_search_switch(raw: Option<&serde_json::Value>) -> Result<bool, ParseError> {
    parse_type_switch(
        raw,
        false,
        || ParseError::InvalidSearchSwitch,
        |ty| ParseError::InvalidSearchType { ty },
    )
}

/// Shared `{"type": "enabled"|"disabled"}` switch decoding.
fn parse_type_switch(
    raw: Option<&serde_json::Value>,
    default: bool,
    shape_err: impl Fn() -> ParseError,
    type_err: impl Fn(String) -> ParseError,
) -> Result<bool, ParseError> {
    let raw = match raw {
        None | Some(serde_json::Value::Null) => return Ok(default),
        Some(v) => v,
    };
    // The value must be an object with a `type` string.
    let ty = raw
        .get("type")
        .and_then(serde_json::Value::as_str)
        .ok_or_else(shape_err)?;
    match ty {
        "enabled" => Ok(true),
        "disabled" => Ok(false),
        other => Err(type_err(other.to_string())),
    }
}

/// Render the transcript as role-tagged text (Go `FlattenMessages`):
/// `"{role}: {content}\n"` per message, text parts joined with newlines
/// after the string content. System messages are tagged like every other
/// role — no special-casing, no injection. Image parts are skipped (they
/// travel via `ref_file_ids`).
pub fn flatten_messages(msgs: &[Message]) -> String {
    let mut b = String::new();
    for m in msgs {
        b.push_str(&m.role);
        b.push_str(": ");
        if !m.content.is_empty() {
            b.push_str(&m.content);
        }
        for p in &m.parts {
            if p.kind == PartKind::Text && !p.text.is_empty() {
                if !b.is_empty() && !b.ends_with(": ") {
                    b.push('\n');
                }
                b.push_str(&p.text);
            }
        }
        b.push('\n');
    }
    b
}

/// Map common image content types to file extensions (Go
/// `imageExtByMIME`). The upload filename MUST carry a supported
/// extension — the server types files by suffix.
pub fn image_ext_by_mime(ct: &str) -> Option<&'static str> {
    let ct = ct.trim().to_lowercase();
    let ct = ct.split(';').next().unwrap_or("").trim();
    match ct {
        "image/png" => Some("png"),
        "image/jpeg" | "image/jpg" => Some("jpg"),
        "image/webp" => Some("webp"),
        "image/gif" => Some("gif"),
        _ => None,
    }
}

/// Decode `data:image/...;base64,<payload>` URLs (Go `decodeDataURL`):
/// MIME must map to a supported extension, payload must be valid base64,
/// and the decoded data must fit the 10 MB cap. Anything else → `None`
/// (the caller treats a non-data URL as http(s) fetch material).
pub fn decode_data_url(u: &str) -> Option<(Vec<u8>, &'static str)> {
    use base64::Engine;
    let rest = u.strip_prefix("data:")?;
    let semi = rest.find(',')?;
    let meta = &rest[..semi];
    let payload = &rest[semi + 1..];
    let mime = meta.strip_suffix(";base64")?;
    let ext = image_ext_by_mime(mime)?;
    let data = base64::engine::general_purpose::STANDARD
        .decode(payload)
        .ok()?;
    if data.len() > 10 << 20 {
        return None;
    }
    Some((data, ext))
}

/// Keep error messages short: `scheme://host/path`, no query string —
/// client-supplied URLs can carry tokens in query params (Go
/// `firstSegment`). Capped at 128 chars.
pub fn first_segment(u: &str) -> &str {
    let cut = u.find('?').unwrap_or(u.len());
    let seg = &u[..cut];
    if seg.len() > 128 {
        &seg[..128]
    } else {
        seg
    }
}

#[cfg(test)]
mod tests;
