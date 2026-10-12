//! Upstream SSE dialect: frame extraction, event dispatch, in-band error
//! classification, web_search citation parsing. 1:1 port of Go
//! `internal/mimo/sse.go` (parser + classifier + web_search).

use crate::thinking::{Piece, ThinkSplitter};
use serde::Deserialize;

/// One parsed SSE event frame from the upstream stream (SDD §2.5).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Event {
    /// `id:` line, when present.
    pub id: String,
    /// `event:` name (dialogId, message, usage, error, ...).
    pub name: String,
    /// `data:` payload (JSON for most events).
    pub data: String,
}

/// In-band error classification (SDD §2.5). "服务器繁忙" is transient and
/// retryable; "模型名称错误" is deterministic and never retried; anything else
/// is a generic stream failure.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum InBandError {
    /// Transient upstream throttle: retry with backoff.
    Busy(String),
    /// The channel refuses this model id: never retry.
    ModelRejected(String),
    /// Any other in-band `error` event payload.
    Stream(String),
    /// sensitive_query / sensitive_title: content filter, verbatim reason.
    ContentFilter { reason: String, message: String },
}

impl InBandError {
    /// The upstream's own message text (ADR 0033: verbatim passthrough).
    pub fn upstream_message(&self) -> String {
        match self {
            InBandError::Busy(m) | InBandError::ModelRejected(m) | InBandError::Stream(m) => {
                m.clone()
            }
            InBandError::ContentFilter { reason, message } => {
                let mut s = format!("content filtered by upstream ({reason})");
                let m = message.trim();
                if !m.is_empty() {
                    s.push_str(": ");
                    s.push_str(m);
                }
                s
            }
        }
    }
}

/// classifyInBandError: pattern-based (upstream sends human-readable Chinese).
pub fn classify_in_band_error(msg: &str) -> InBandError {
    if msg.contains("繁忙") || msg.contains("稍后再试") {
        InBandError::Busy(msg.to_string())
    } else if msg.contains("模型名称") || msg.contains("模型不存在") {
        InBandError::ModelRejected(msg.to_string())
    } else {
        InBandError::Stream(msg.to_string())
    }
}

/// `data` JSON of message/dialogId/finish events.
#[derive(Deserialize, Default)]
struct MsgPayload {
    #[serde(rename = "type", default)]
    #[allow(dead_code)]
    type_: String,
    #[serde(default)]
    content: String,
}

/// Upstream `usage` event payload (SDD §2.5). Emitted multiple times; last
/// wins.
#[derive(Deserialize, Default, Clone, Debug, PartialEq, Eq)]
pub struct MiMoUsage {
    #[serde(rename = "promptTokens", default)]
    pub prompt_tokens: i64,
    #[serde(rename = "completionTokens", default)]
    pub completion_tokens: i64,
    #[serde(rename = "totalTokens", default)]
    pub total_tokens: i64,
    #[serde(rename = "nativeUsage", default)]
    pub native_usage: Option<NativeUsage>,
}

/// Upstream nativeUsage (snake_case fields).
#[derive(Deserialize, Default, Clone, Debug, PartialEq, Eq)]
pub struct NativeUsage {
    #[serde(default)]
    pub prompt_tokens: i64,
    #[serde(default)]
    pub completion_tokens: i64,
    #[serde(default)]
    pub total_tokens: i64,
    #[serde(default)]
    pub prompt_tokens_details: Option<NativeCached>,
    #[serde(default)]
    pub completion_tokens_details: Option<NativeThink>,
}

#[derive(Deserialize, Default, Clone, Debug, PartialEq, Eq)]
pub struct NativeCached {
    #[serde(default)]
    pub cached_tokens: i64,
}

#[derive(Deserialize, Default, Clone, Debug, PartialEq, Eq)]
pub struct NativeThink {
    #[serde(default)]
    pub reasoning_tokens: i64,
}

/// Turn outcome assembled by [`Streamer::finish`].
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct TurnResult {
    pub dialog_id: String,
    pub reasoning: String,
    pub content: String,
    pub usage: Option<MiMoUsage>,
    pub citations: Vec<Citation>,
}

/// One web-search citation (gateway extension, M19).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Citation {
    /// Arrival order across all web_search events of the turn.
    pub index: usize,
    pub title: String,
    pub url: String,
    pub display_url: String,
    pub snippet: String,
    pub used: bool,
}

/// Streamer drives one upstream chat stream end-to-end: SSE event dispatch,
/// thinking-marker splitting, usage last-wins, dialogId chaining, citation
/// accumulation (SDD §2.4/§2.5).
#[derive(Default)]
pub struct Streamer {
    splitter: ThinkSplitter,
    dialog_id: String,
    reasoning: String,
    content: String,
    usage: Option<MiMoUsage>,
    citations: Vec<Citation>,
    /// Every piece, in emission order (the streaming passthrough source).
    pieces: Vec<Piece>,
    /// web_search payloads that failed to parse (surfaced by callers at
    /// debug level; never fails the turn).
    pub citations_parse_errors: Vec<String>,
}

/// handleEvent outcome: done on finish [DONE], error kills the turn.
#[derive(Debug, PartialEq, Eq)]
pub enum HandleOutcome {
    Continue,
    Done,
    Error(InBandError),
}

impl Streamer {
    pub fn new() -> Self {
        Self::default()
    }

    /// Processes one parsed event (TDD SSE-005: error/sensitive events fail
    /// the turn; malformed message deltas are skipped, never fatal).
    pub fn handle_event(&mut self, ev: &Event) -> HandleOutcome {
        match ev.name.as_str() {
            "dialogId" => {
                if let Ok(p) = serde_json::from_str::<MsgPayload>(&ev.data) {
                    if !p.content.is_empty() {
                        self.dialog_id = p.content.clone();
                    }
                }
                HandleOutcome::Continue
            }
            "message" => match serde_json::from_str::<MsgPayload>(&ev.data) {
                Ok(p) => {
                    for piece in self.splitter.push(&p.content) {
                        self.record_piece(piece);
                    }
                    HandleOutcome::Continue
                }
                // Malformed delta: skip, never kill the stream.
                Err(_) => HandleOutcome::Continue,
            },
            "usage" => {
                if let Ok(u) = serde_json::from_str::<MiMoUsage>(&ev.data) {
                    self.usage = Some(u); // last one wins (TDD SSE-012)
                }
                HandleOutcome::Continue
            }
            "finish" => {
                let p = serde_json::from_str::<MsgPayload>(&ev.data).unwrap_or_default();
                if p.content == "[DONE]" {
                    HandleOutcome::Done
                } else {
                    HandleOutcome::Continue
                }
            }
            "error" => {
                let p = serde_json::from_str::<MsgPayload>(&ev.data).unwrap_or_default();
                HandleOutcome::Error(classify_in_band_error(&p.content))
            }
            "sensitive_query" | "sensitive_title" => {
                let p = serde_json::from_str::<MsgPayload>(&ev.data).unwrap_or_default();
                HandleOutcome::Error(InBandError::ContentFilter {
                    reason: ev.name.clone(),
                    message: p.content,
                })
            }
            "web_search" => {
                match parse_web_search(&ev.data) {
                    Ok(cits) if !cits.is_empty() => {
                        for (i, mut c) in cits.into_iter().enumerate() {
                            c.index = self.citations.len() + i;
                            self.citations.push(c);
                        }
                    }
                    Ok(_) => {}
                    Err(e) => self.citations_parse_errors.push(e),
                }
                HandleOutcome::Continue
            }
            // doc / tip_ratio / tip_truncate: search/parse progress — nothing
            // client-visible to carry.
            _ => HandleOutcome::Continue,
        }
    }

    fn record_piece(&mut self, piece: Piece) {
        match piece.kind {
            crate::thinking::PieceKind::Reasoning => self.reasoning.push_str(&piece.text),
            crate::thinking::PieceKind::Content => self.content.push_str(&piece.text),
        }
        self.pieces.push(piece);
    }

    /// Drains the splitter (unterminated think block counts as reasoning,
    /// TDD SSE-009) and returns the assembled turn. Flushed pieces also land
    /// in `pieces` so streaming consumers get the complete text.
    pub fn finish(&mut self) -> TurnResult {
        for piece in self.splitter.flush() {
            self.record_piece(piece);
        }
        TurnResult {
            dialog_id: self.dialog_id.clone(),
            reasoning: self.reasoning.clone(),
            content: self.content.clone(),
            usage: self.usage.clone(),
            citations: self.citations.clone(),
        }
    }

    /// Pieces emitted so far, in order (streaming passthrough source).
    pub fn pieces(&self) -> &[Piece] {
        &self.pieces
    }
}

/// parseEvents: reads an SSE byte stream, feeding every complete frame to
/// [`Streamer::handle_event`]. Returns `Ok(true)` when a finish [DONE] frame
/// ended the turn cleanly; `Err` on in-band errors or a stream that ended
/// before finish (EOF-before-[DONE]).
///
/// Wire format (SDD §2.5): frames separated by a blank line; field lines
/// `id:`, `event:`, `data:` — colon WITHOUT space in the MiMo dialect, but
/// `event: x` and CRLF endings are tolerated (TDD SSE-002/003). Comment
/// lines starting with ':' are ignored (keepalive).
pub fn parse_events(input: &str, streamer: &mut Streamer) -> Result<bool, InBandError> {
    let mut id = String::new();
    let mut event = String::new();
    let mut data = String::new();
    let flush = |streamer: &mut Streamer,
                 id: &mut String,
                 event: &mut String,
                 data: &mut String|
     -> Option<HandleOutcome> {
        if event.is_empty() && data.is_empty() {
            return None;
        }
        let ev = Event {
            id: std::mem::take(id),
            name: std::mem::take(event),
            data: std::mem::take(data),
        };
        Some(streamer.handle_event(&ev))
    };
    for line in input.split_inclusive('\n') {
        let clean = line.trim_end_matches(['\r', '\n']);
        if clean.is_empty() {
            if let Some(outcome) = flush(streamer, &mut id, &mut event, &mut data) {
                match fold(outcome) {
                    // Continue: keep reading (the Go loop only returns on
                    // done or error).
                    Ok(false) => {}
                    other => return other,
                }
            }
        } else if !clean.starts_with(':') {
            // Field line: strip one optional space after the colon.
            if let Some(i) = clean.find(':') {
                let (name, rest) = clean.split_at(i);
                let val = &rest[1..];
                let val = val.strip_prefix(' ').unwrap_or(val);
                match name {
                    "id" => {
                        id.clear();
                        id.push_str(val);
                    }
                    "event" => {
                        event.clear();
                        event.push_str(val);
                    }
                    "data" => {
                        if !data.is_empty() {
                            data.push('\n');
                        }
                        data.push_str(val);
                    }
                    _ => {}
                }
            }
        }
    }
    // Final flush: a Done frame ended the turn cleanly; a trailing error
    // frame still fails the turn; anything else means the stream ended
    // without finish.
    match flush(streamer, &mut id, &mut event, &mut data) {
        Some(outcome) => fold_final(outcome),
        None => Err(InBandError::Stream("unexpected EOF".to_string())),
    }
}

/// fold, but for the post-loop final flush: Continue also means EOF (the
/// stream ended without a finish [DONE] frame).
fn fold_final(outcome: HandleOutcome) -> Result<bool, InBandError> {
    match outcome {
        HandleOutcome::Done => Ok(true),
        HandleOutcome::Error(e) => Err(e),
        HandleOutcome::Continue => Err(InBandError::Stream("unexpected EOF".to_string())),
    }
}

fn fold(outcome: HandleOutcome) -> Result<bool, InBandError> {
    match outcome {
        HandleOutcome::Continue => Ok(false),
        HandleOutcome::Done => Ok(true),
        HandleOutcome::Error(e) => Err(e),
    }
}

/// parseWebSearch: a payload that is not JSON at all is reported (never
/// silently skipped); individual odd fields degrade one citation's richness,
/// not the result set.
pub fn parse_web_search(data: &str) -> Result<Vec<Citation>, String> {
    let trimmed = data.trim();
    if trimmed.is_empty() || trimmed == "null" {
        return Ok(Vec::new());
    }
    let items = decode_web_search_items(trimmed)?;
    let mut out = Vec::with_capacity(items.len());
    for it in items {
        let url = raw_string(it.get("url"));
        if url.is_empty() {
            continue; // a citation without a source is noise
        }
        out.push(Citation {
            index: 0,
            title: raw_string(it.get("name")),
            url,
            display_url: raw_string(it.get("displayUrl")),
            snippet: raw_string(it.get("snippet")),
            used: raw_bool(it.get("usedContent")),
        });
    }
    Ok(out)
}

/// Accepts both upstream shapes: a bare array and an object wrapping the
/// array in `data`.
fn decode_web_search_items(
    trimmed: &str,
) -> Result<Vec<serde_json::Map<String, serde_json::Value>>, String> {
    let v: serde_json::Value =
        serde_json::from_str(trimmed).map_err(|e| format!("web_search payload unreadable: {e}"))?;
    match v {
        serde_json::Value::Array(items) => Ok(items
            .iter()
            .filter_map(|i| i.as_object().cloned())
            .collect()),
        serde_json::Value::Object(obj) => match obj.get("data") {
            Some(serde_json::Value::Array(items)) => Ok(items
                .iter()
                .filter_map(|i| i.as_object().cloned())
                .collect()),
            _ => Ok(Vec::new()),
        },
        _ => Ok(Vec::new()),
    }
}

/// rawString: a null/number/object/array in a string slot yields "" (one
/// unexpected type cannot sink the citation); numbers stringify.
fn raw_string(raw: Option<&serde_json::Value>) -> String {
    match raw {
        Some(serde_json::Value::String(s)) => s.clone(),
        Some(serde_json::Value::Number(n)) => n.to_string(),
        _ => String::new(),
    }
}

/// rawBool: the boolean forms seen in the wild — true/false, "True"/"False"
/// (case-insensitive), 1/0.
fn raw_bool(raw: Option<&serde_json::Value>) -> bool {
    match raw {
        Some(serde_json::Value::Bool(b)) => *b,
        Some(serde_json::Value::Number(n)) => n.as_i64() == Some(1),
        Some(serde_json::Value::String(s)) => {
            let l = s.to_ascii_lowercase();
            l == "true" || l == "1"
        }
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(input: &str) -> (Result<bool, InBandError>, TurnResult) {
        let mut s = Streamer::new();
        let r = parse_events(input, &mut s);
        let t = s.finish();
        (r, t)
    }

    const SIMPLE: &str = "event:dialogId\ndata:{\"content\":\"abc123\"}\n\n\
        event:message\ndata:{\"content\":\"hello\"}\n\n\
        event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";

    #[test]
    fn basic_turn_assembles() {
        let (r, t) = run(SIMPLE);
        assert_eq!(r, Ok(true));
        assert_eq!(t.dialog_id, "abc123");
        assert_eq!(t.content, "hello");
        assert_eq!(t.reasoning, "");
        assert!(t.usage.is_none());
    }

    #[test]
    fn colon_without_space_and_crlf_tolerated() {
        // TDD SSE-002/003: `event: x` (space after colon) + CRLF endings.
        let input = "event: dialogId\r\ndata: {\"content\":\"d1\"}\r\n\r\n\
            event: message\r\ndata: {\"content\":\"x\"}\r\n\r\n\
            event: finish\r\ndata: {\"content\":\"[DONE]\"}\r\n\r\n";
        let (r, t) = run(input);
        assert_eq!(r, Ok(true));
        assert_eq!(t.dialog_id, "d1");
        assert_eq!(t.content, "x");
    }

    #[test]
    fn comments_ignored() {
        let input = ": keepalive\n\nevent:message\ndata:{\"content\":\"a\"}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, t) = run(input);
        assert_eq!(r, Ok(true));
        assert_eq!(t.content, "a");
    }

    #[test]
    fn multiple_data_lines_joined_with_newline() {
        // Data lines join with '\n' → JSON parse of the joined payload
        // decides: a split JSON object fails to parse and the delta is
        // skipped (never fatal); a validly split one reassembles.
        let input = "event:message\ndata:{\"content\":\ndata:\"x\"}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, t) = run(input);
        assert_eq!(r, Ok(true));
        assert_eq!(t.content, "x");
    }

    #[test]
    fn eof_before_done_is_error() {
        // TDD SSE-013: stream ends without finish [DONE].
        let (r, _) = run("event:message\ndata:{\"content\":\"partial\"}\n\n");
        assert!(matches!(r, Err(InBandError::Stream(_))));
    }

    #[test]
    fn in_band_error_classified_busy() {
        let (r, _) = run("event:error\ndata:{\"content\":\"服务器繁忙，请稍后再试\"}\n\n");
        assert_eq!(
            r,
            Err(InBandError::Busy("服务器繁忙，请稍后再试".to_string()))
        );
    }

    #[test]
    fn in_band_error_classified_model_rejected() {
        let (r, _) = run("event:error\ndata:{\"content\":\"模型名称错误\"}\n\n");
        assert_eq!(
            r,
            Err(InBandError::ModelRejected("模型名称错误".to_string()))
        );
    }

    #[test]
    fn in_band_error_generic() {
        let (r, _) = run("event:error\ndata:{\"content\":\"别的错\"}\n\n");
        assert_eq!(r, Err(InBandError::Stream("别的错".to_string())));
    }

    #[test]
    fn sensitive_query_is_content_filter_verbatim() {
        let r = run("event:sensitive_query\ndata:{\"content\":\"bad topic\"}\n\n").0;
        assert_eq!(
            r,
            Err(InBandError::ContentFilter {
                reason: "sensitive_query".to_string(),
                message: "bad topic".to_string()
            })
        );
        // Verbatim upstream wording in the message (ADR 0033).
        assert_eq!(
            r.unwrap_err().upstream_message(),
            "content filtered by upstream (sensitive_query): bad topic"
        );
    }

    #[test]
    fn usage_last_wins() {
        // TDD SSE-012.
        let input = "event:usage\ndata:{\"promptTokens\":1,\"completionTokens\":2,\"totalTokens\":3}\n\n\
            event:usage\ndata:{\"promptTokens\":10,\"completionTokens\":20,\"totalTokens\":30,\"nativeUsage\":{\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, t) = run(input);
        assert_eq!(r, Ok(true));
        let u = t.usage.unwrap();
        assert_eq!(u.prompt_tokens, 10);
        assert_eq!(u.completion_tokens, 20);
        assert_eq!(u.total_tokens, 30);
        assert_eq!(
            u.native_usage
                .unwrap()
                .prompt_tokens_details
                .unwrap()
                .cached_tokens,
            5
        );
    }

    #[test]
    fn malformed_message_delta_skipped() {
        let input = "event:message\ndata:not json\n\n\
            event:message\ndata:{\"content\":\"ok\"}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, t) = run(input);
        assert_eq!(r, Ok(true));
        assert_eq!(t.content, "ok");
    }

    #[test]
    fn web_search_events_accumulate_with_indices() {
        let input = "event:web_search\ndata:[{\"name\":\"a\",\"url\":\"https://a\",\"usedContent\":true}]\n\n\
            event:web_search\ndata:{\"data\":[{\"name\":\"b\",\"url\":\"https://b\",\"usedContent\":\"False\"}]}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, t) = run(input);
        assert_eq!(r, Ok(true));
        assert_eq!(t.citations.len(), 2);
        assert_eq!(t.citations[0].index, 0);
        assert_eq!(t.citations[0].url, "https://a");
        assert!(t.citations[0].used);
        assert_eq!(t.citations[1].index, 1);
        assert!(!t.citations[1].used);
    }

    #[test]
    fn web_search_malformed_payload_recorded_not_fatal() {
        let input = "event:web_search\ndata:not-json\n\n\
            event:message\ndata:{\"content\":\"answer\"}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let mut s = Streamer::new();
        let r = parse_events(input, &mut s);
        assert_eq!(r, Ok(true));
        assert_eq!(s.citations_parse_errors.len(), 1);
        assert_eq!(s.finish().content, "answer");
    }

    #[test]
    fn web_search_item_without_url_skipped() {
        assert!(parse_web_search("[{\"name\":\"nourl\"}]")
            .unwrap()
            .is_empty());
    }

    #[test]
    fn web_search_flexible_types() {
        // usedContent as string "True", metadata as object, name as number →
        // citation survives with degraded fields.
        let cits = parse_web_search(
            "[{\"name\":123,\"url\":\"https://x\",\"usedContent\":\"True\",\"metadata\":{}}]",
        )
        .unwrap();
        assert_eq!(cits.len(), 1);
        assert_eq!(cits[0].title, "123"); // number stringified
        assert!(cits[0].used);
    }

    #[test]
    fn web_search_null_payload_ok() {
        assert!(parse_web_search("null").unwrap().is_empty());
        assert!(parse_web_search("  ").unwrap().is_empty());
    }

    #[test]
    fn finish_non_done_continues() {
        // A finish event with other content does not terminate the turn.
        let input = "event:finish\ndata:{\"content\":\"other\"}\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, _) = run(input);
        assert_eq!(r, Ok(true));
    }

    #[test]
    fn unknown_event_names_ignored() {
        let input = "event:doc\ndata:progress\n\nevent:tip_ratio\ndata:0.5\n\n\
            event:finish\ndata:{\"content\":\"[DONE]\"}\n\n";
        let (r, _) = run(input);
        assert_eq!(r, Ok(true));
    }
}
