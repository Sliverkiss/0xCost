//! Upstream SSE frame parser (dossier §2/§6).
//!
//! TokenHarbor frames are `event: <type>\ndata: <json>\n\n`. Two live-verified
//! quirks matter:
//! - **Errors arrive inside the stream**: even a 400/403/404 response carries
//!   `text/event-stream` with a single `error` frame carrying a machine
//!   readable `code` — the status alone is not enough, the body must be
//!   parsed before mapping (dossier §6).
//! - The event sequence beyond `error` is hypothesis-from-reference
//!   (`thinking`/`chunk`/`done` verified shapes; `tool_use`/`citation`/
//!   `image*`/`file*` unverified) — unknown events are carried through as
//!   [`Frame::Other`] instead of being dropped, so a live capture diff is
//!   possible and nothing silently disappears.

use serde_json::Value;

/// One parsed upstream SSE frame.
#[derive(Debug, Clone, PartialEq)]
pub enum Frame {
    /// Reasoning delta (`thinking {delta}`) — reference hypothesis, shape
    /// tolerant: any JSON object passes its raw value through.
    Thinking(String),
    /// Content delta (`chunk {delta}`).
    Chunk(String),
    /// Terminal success frame (`done`): messageId, usage, billing.
    Done(DonePayload),
    /// Terminal error frame (`error {code?, message}`) — may be the ONLY
    /// frame of the response, with a non-200 HTTP status (dossier §6).
    Error(UpstreamError),
    /// Any other event (tool_use, citation, image*, file*, ...) — carried
    /// through verbatim for future diffing, never dropped silently.
    Other { event: String, data: Value },
}

/// Terminal success payload (`done` frame).
#[derive(Debug, Clone, PartialEq)]
pub struct DonePayload {
    pub message_id: Option<String>,
    pub model: Option<String>,
    pub search_count: Option<u64>,
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
}

/// Upstream error frame payload.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UpstreamError {
    /// Machine-readable code (`account_suspended`, `session_unknown`,
    /// `model_unavailable`, `missing_session`, ... — dossier §6).
    pub code: String,
    /// Human-readable upstream message.
    pub message: String,
}

/// Parse one `event:`/`data:` block pair. Returns `None` for empty blocks.
fn parse_block(event: &str, data: &str) -> Option<Frame> {
    let json: Value = serde_json::from_str(data).ok()?;
    match event {
        "thinking" => Some(Frame::Thinking(
            json.get("delta")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
        )),
        "chunk" => Some(Frame::Chunk(
            json.get("delta")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
        )),
        "done" => Some(Frame::Done(DonePayload {
            message_id: json
                .get("messageId")
                .and_then(Value::as_str)
                .map(str::to_string),
            model: json
                .get("model")
                .and_then(Value::as_str)
                .map(str::to_string),
            search_count: json.get("searchCount").and_then(Value::as_u64),
            input_tokens: json.pointer("/usage/inputTokens").and_then(Value::as_u64),
            output_tokens: json.pointer("/usage/outputTokens").and_then(Value::as_u64),
        })),
        "error" => Some(Frame::Error(UpstreamError {
            code: json
                .get("code")
                .and_then(Value::as_str)
                .unwrap_or("unknown")
                .to_string(),
            message: json
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
        })),
        other => Some(Frame::Other {
            event: other.to_string(),
            data: json,
        }),
    }
}

/// Parse a complete SSE byte sequence into frames.
///
/// Tolerant of: multi-line `data:` (joined with `\n`), `event:` omitted
/// (defaults `message`, mapped to [`Frame::Other`]), comment lines, CRLF,
/// and trailing whitespace. Unparseable `data` JSON is skipped (upstream
/// keepalives / partial writes must not kill the stream).
pub fn parse(bytes: &[u8]) -> Vec<Frame> {
    let text = String::from_utf8_lossy(bytes);
    let mut frames = Vec::new();
    let mut event: Option<String> = None;
    let mut data_lines: Vec<&str> = Vec::new();

    for line in text.lines() {
        let line = line.strip_suffix('\r').unwrap_or(line);
        if line.is_empty() {
            // Blank line = end of one frame block.
            if let Some(data) = join_data(&data_lines) {
                let ev = event.as_deref().unwrap_or("message");
                if let Some(frame) = parse_block(ev, &data) {
                    frames.push(frame);
                }
            }
            event = None;
            data_lines.clear();
        } else if let Some(rest) = line.strip_prefix("event:") {
            event = Some(rest.strip_prefix(' ').unwrap_or(rest).to_string());
        } else if let Some(rest) = line.strip_prefix("data:") {
            data_lines.push(rest.strip_prefix(' ').unwrap_or(rest));
        }
        // comment lines (":...") and anything else are ignored
    }
    // A stream that ends without a trailing blank line still has its last
    // block pending.
    if let Some(data) = join_data(&data_lines) {
        let ev = event.as_deref().unwrap_or("message");
        if let Some(frame) = parse_block(ev, &data) {
            frames.push(frame);
        }
    }
    frames
}

fn join_data(lines: &[&str]) -> Option<String> {
    if lines.is_empty() {
        None
    } else {
        Some(lines.join("\n"))
    }
}

/// First `error` frame in a parsed sequence, if any.
pub fn first_error(frames: &[Frame]) -> Option<&UpstreamError> {
    frames.iter().find_map(|f| match f {
        Frame::Error(e) => Some(e),
        _ => None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const SUCCESS: &str = include_str!("../fixtures/sse-success.txt");
    const ERR_SUSPENDED: &str = include_str!("../fixtures/sse-error-account-suspended.txt");
    const ERR_MISSING: &str = include_str!("../fixtures/sse-error-missing-session.txt");
    const ERR_UNKNOWN: &str = include_str!("../fixtures/sse-error-session-unknown.txt");
    const ERR_MODEL: &str = include_str!("../fixtures/sse-error-model-unavailable.txt");

    #[test]
    fn parses_success_sequence() {
        let frames = parse(SUCCESS.as_bytes());
        assert_eq!(frames.len(), 5, "thinking x2, chunk x2, done: {frames:?}");
        assert_eq!(
            frames[0],
            Frame::Thinking("Let me think about this. ".into())
        );
        assert_eq!(frames[2], Frame::Chunk("OK".into()));
        assert_eq!(
            frames[4],
            Frame::Done(DonePayload {
                message_id: Some("msg_01ABC".into()),
                model: Some("xiaomi/mimo-v2.6-flash:free".into()),
                search_count: Some(0),
                input_tokens: Some(12),
                output_tokens: Some(3),
            })
        );
    }

    #[test]
    fn full_text_extraction() {
        let frames = parse(SUCCESS.as_bytes());
        let text: String = frames
            .iter()
            .filter_map(|f| match f {
                Frame::Chunk(s) => Some(s.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(text, "OK — done.");
    }

    #[test]
    fn parses_error_frames_from_all_dossier_codes() {
        for (fixture, expected_code) in [
            (ERR_SUSPENDED, "account_suspended"),
            (ERR_MISSING, "missing_session"),
            (ERR_UNKNOWN, "session_unknown"),
            (ERR_MODEL, "model_unavailable"),
        ] {
            let frames = parse(fixture.as_bytes());
            assert_eq!(frames.len(), 1, "{expected_code}");
            match &frames[0] {
                Frame::Error(e) => {
                    assert_eq!(e.code, expected_code);
                    assert!(!e.message.is_empty());
                }
                other => panic!("{expected_code}: expected error frame, got {other:?}"),
            }
        }
    }

    #[test]
    fn non_200_sse_error_is_parseable_before_mapping() {
        // Dossier §6: the response body is SSE even when the HTTP status is
        // 400/403/404 — the parser must yield the code, not just see a status.
        let frames = parse(ERR_SUSPENDED.as_bytes());
        let e = first_error(&frames).expect("error frame present");
        assert_eq!(e.code, "account_suspended");
        assert!(e.message.contains("suspended"));
    }

    #[test]
    fn unknown_events_carried_not_dropped() {
        let raw = "event: citation\ndata: {\"url\":\"https://x.test\",\"title\":\"T\"}\n\n";
        let frames = parse(raw.as_bytes());
        match &frames[0] {
            Frame::Other { event, data } => {
                assert_eq!(event, "citation");
                assert_eq!(data["title"], "T");
            }
            other => panic!("expected Other frame, got {other:?}"),
        }
    }

    #[test]
    fn crlf_and_missing_trailing_blank_ok() {
        let raw =
            "event: chunk\r\ndata: {\"delta\":\"hi\"}\r\n\r\nevent: done\r\ndata: {\"usage\":{}}";
        let frames = parse(raw.as_bytes());
        assert_eq!(frames.len(), 2);
        assert_eq!(frames[0], Frame::Chunk("hi".into()));
        assert!(matches!(frames[1], Frame::Done(_)));
    }

    #[test]
    fn multiline_data_joined() {
        let raw = "event: chunk\ndata: {\"delta\":\"a\"\ndata: ,\"delta2\":1}\n\n";
        let frames = parse(raw.as_bytes());
        assert_eq!(frames[0], Frame::Chunk("a".into()));
    }

    #[test]
    fn malformed_data_skipped_not_fatal() {
        let raw = "event: chunk\ndata: {broken\n\nevent: chunk\ndata: {\"delta\":\"ok\"}\n\n";
        let frames = parse(raw.as_bytes());
        assert_eq!(frames, vec![Frame::Chunk("ok".into())]);
    }

    #[test]
    fn empty_and_comment_only_input() {
        assert!(parse(b"").is_empty());
        assert!(parse(b": keepalive\n\n").is_empty());
    }
}
