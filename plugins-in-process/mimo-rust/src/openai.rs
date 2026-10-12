//! OpenAI response shapers: streaming chunks, non-streaming completion,
//! usage mapping, citations/annotations — 1:1 port of Go
//! `internal/adapter/openai.go` + `internal/adapter/citations.go`, plus the
//! client-facing SSE frame rendering of `internal/api/stream.go`.
//!
//! Field order and null-ness are pinned to the Go JSON tags: `delta` is
//! `{}` on the finish chunk, both usage detail objects are always present
//! (zero-filled), `finish_reason` is null until termination, and the usage /
//! citations chunks carry `choices: []` so they cannot clobber
//! `finish_reason` (SDD §5.4).

use serde::Serialize;

use crate::sse::{Citation, MiMoUsage};
use crate::thinking::Piece;

/// The shared stream identity (one id for all chunks of one stream, SDD
/// §5.4).
#[derive(Debug, Clone)]
pub struct StreamIdent {
    pub id: String,
    pub model: String,
    pub created: i64,
}

/// Mints a `chatcmpl-<32-hex>` id (Go NewChatID).
pub fn new_chat_id() -> String {
    format!("chatcmpl-{}", uuid::Uuid::new_v4().simple())
}

/// Current unix seconds (Go `adapter.NowUnix`).
pub fn now_unix() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Fresh 32-hex random string (msgId / conversationId minting, Go
/// RandomHex32).
pub fn random_hex32() -> String {
    uuid::Uuid::new_v4().simple().to_string()
}

// ---------------------------------------------------------------------------
// Streaming chunks (Go Delta / ChunkChoice / Chunk)
// ---------------------------------------------------------------------------

/// The OpenAI streaming delta object. Fields are optional so the terminal
/// chunk serializes delta as `{}` (SDD §5.4).
#[derive(Debug, Clone, Default, Serialize, PartialEq)]
pub struct Delta {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub role: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub content: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reasoning_content: String,
}

/// The single element of the choices array in a streaming chunk.
/// `finish_reason` is `None` → null until termination.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ChunkChoice {
    pub index: usize,
    pub delta: Delta,
    pub finish_reason: Option<String>,
}

/// Usage detail objects — always present, zero-filled, separate types (the
/// two objects hold different counters; a shared struct would pad each with
/// the other's key, TDD API-004).
#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq, Default)]
pub struct PromptTokensDetails {
    pub cached_tokens: i64,
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq, Default)]
pub struct CompletionTokensDetails {
    pub reasoning_tokens: i64,
}

/// The OpenAI usage object. Detail objects are always carried (never null,
/// never omitted).
#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq, Default)]
pub struct Usage {
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub total_tokens: i64,
    pub prompt_tokens_details: PromptTokensDetails,
    pub completion_tokens_details: CompletionTokensDetails,
}

/// One `chat.completion.chunk` frame (Go Chunk). JSON keys are exactly
/// `{id, object, created, model, choices}` — plus `usage` / `citations`
/// only on their extension chunks.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct Chunk {
    pub id: String,
    pub object: &'static str,
    pub created: i64,
    pub model: String,
    pub choices: Vec<ChunkChoice>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub usage: Option<Usage>,
    /// Gateway extension (M19): one extra chunk with `choices: []` carries
    /// the web-search results.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub citations: Vec<CitationWire>,
}

/// The wire form of a citation (Go adapter.Citation; index is arrival
/// order, `used` mirrors upstream usedContent).
#[derive(Debug, Clone, Serialize, PartialEq, Eq, Default)]
pub struct CitationWire {
    pub index: usize,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub title: String,
    pub url: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub display_url: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub snippet: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub used: bool,
}

impl From<&Citation> for CitationWire {
    fn from(c: &Citation) -> Self {
        CitationWire {
            index: c.index,
            title: c.title.clone(),
            url: c.url.clone(),
            display_url: c.display_url.clone(),
            snippet: c.snippet.clone(),
            used: c.used,
        }
    }
}

fn chunk_base(ident: &StreamIdent) -> Chunk {
    Chunk {
        id: ident.id.clone(),
        object: "chat.completion.chunk",
        created: ident.created,
        model: ident.model.clone(),
        choices: Vec::new(),
        usage: None,
        citations: Vec::new(),
    }
}

/// The first chunk of a stream: role-only delta, finish_reason null
/// (TDD API-003).
pub fn shape_role_chunk(ident: &StreamIdent) -> Chunk {
    let mut c = chunk_base(ident);
    c.choices = vec![ChunkChoice {
        delta: Delta {
            role: "assistant".into(),
            ..Default::default()
        },
        index: 0,
        finish_reason: None,
    }];
    c
}

/// A delta chunk carrying either content or reasoning_content — never both
/// (the splitter never emits both in one piece).
pub fn shape_text_chunk(ident: &StreamIdent, piece: &Piece) -> Chunk {
    let delta = match piece.kind {
        crate::thinking::PieceKind::Reasoning => Delta {
            reasoning_content: piece.text.clone(),
            ..Default::default()
        },
        crate::thinking::PieceKind::Content => Delta {
            content: piece.text.clone(),
            ..Default::default()
        },
    };
    let mut c = chunk_base(ident);
    c.choices = vec![ChunkChoice {
        delta,
        index: 0,
        finish_reason: None,
    }];
    c
}

/// The terminal choice chunk: delta `{}`, finish_reason "stop".
pub fn shape_finish_chunk(ident: &StreamIdent) -> Chunk {
    let mut c = chunk_base(ident);
    c.choices = vec![ChunkChoice {
        delta: Delta::default(),
        index: 0,
        finish_reason: Some("stop".into()),
    }];
    c
}

/// The citations extension chunk (M19): `choices: []`, at most once, right
/// before the usage chunk.
pub fn shape_citations_chunk(ident: &StreamIdent, cits: &[Citation]) -> Chunk {
    let mut c = chunk_base(ident);
    c.citations = cits.iter().map(CitationWire::from).collect();
    c
}

/// The usage chunk with `choices: []` so it cannot clobber finish_reason
/// (SDD §5.4 bug note from mimo-web-proxy).
pub fn shape_usage_chunk(ident: &StreamIdent, usage: Usage) -> Chunk {
    let mut c = chunk_base(ident);
    c.usage = Some(usage);
    c
}

// ---------------------------------------------------------------------------
// Usage mapping (Go MapUsage / AddUsage)
// ---------------------------------------------------------------------------

/// Maps the last upstream usage event into the OpenAI usage object. Both
/// detail objects always present, zero-filled when upstream counted
/// nothing; nil → zeros, never invented (TDD API-004).
pub fn map_usage(u: &Option<MiMoUsage>) -> Usage {
    let mut out = Usage::default();
    let Some(u) = u else {
        return out;
    };
    out.prompt_tokens = u.prompt_tokens;
    out.completion_tokens = u.completion_tokens;
    out.total_tokens = u.total_tokens;
    if let Some(n) = &u.native_usage {
        if let Some(pd) = &n.prompt_tokens_details {
            out.prompt_tokens_details.cached_tokens = pd.cached_tokens;
        }
        if let Some(cd) = &n.completion_tokens_details {
            out.completion_tokens_details.reasoning_tokens = cd.reasoning_tokens;
        }
    }
    out
}

/// Accumulates usage across replayed turns (the chain-replay path sums
/// usage over its calls).
pub fn add_usage(a: Usage, b: Usage) -> Usage {
    Usage {
        prompt_tokens: a.prompt_tokens + b.prompt_tokens,
        completion_tokens: a.completion_tokens + b.completion_tokens,
        total_tokens: a.total_tokens + b.total_tokens,
        prompt_tokens_details: PromptTokensDetails {
            cached_tokens: a.prompt_tokens_details.cached_tokens
                + b.prompt_tokens_details.cached_tokens,
        },
        completion_tokens_details: CompletionTokensDetails {
            reasoning_tokens: a.completion_tokens_details.reasoning_tokens
                + b.completion_tokens_details.reasoning_tokens,
        },
    }
}

// ---------------------------------------------------------------------------
// Non-streaming completion (Go CompletionMessage / CompletionChoice /
// Completion)
// ---------------------------------------------------------------------------

/// The non-streaming response message (SDD §5.4): `refusal` always present
/// (null when nothing was refused), `annotations` when a search turn
/// produced citations, `reasoning_content` omitted when empty (TDD
/// API-001).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct CompletionMessage {
    pub role: String,
    pub content: String,
    pub refusal: Option<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub reasoning_content: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub annotations: Vec<Annotation>,
}

/// The OpenAI annotation object attached to a message (M19).
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Annotation {
    #[serde(rename = "type")]
    pub type_: String,
    pub url_citation: UrlCitation,
}

/// The url_citation body. Index fields are absent when the upstream gave no
/// position — an invented span would be worse than an absent one.
#[derive(Debug, Clone, Serialize, PartialEq, Eq, Default)]
pub struct UrlCitation {
    pub url: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub title: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub snippet: String,
}

/// Converts citations into OpenAI annotations, preserving order (Go
/// `Annotations`).
pub fn annotations(cits: &[Citation]) -> Vec<Annotation> {
    cits.iter()
        .map(|c| Annotation {
            type_: "url_citation".into(),
            url_citation: UrlCitation {
                url: c.url.clone(),
                title: c.title.clone(),
                snippet: c.snippet.clone(),
            },
        })
        .collect()
}

/// The single element of the non-streaming choices array. `logprobs` is
/// emitted as null: this gateway never returns token log-probabilities.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct CompletionChoice {
    pub index: usize,
    pub message: CompletionMessage,
    pub logprobs: Option<serde_json::Value>,
    pub finish_reason: String,
}

/// The non-streaming response body (SDD §5.4).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct Completion {
    pub id: String,
    pub object: &'static str,
    pub created: i64,
    pub model: String,
    pub choices: Vec<CompletionChoice>,
    pub usage: Usage,
    /// Gateway extension (M19) duplicating the message annotations at the
    /// top level.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub citations: Vec<CitationWire>,
}

/// Assembles the non-streaming response (Go ShapeCompletion).
pub fn shape_completion(
    ident: &StreamIdent,
    content: &str,
    reasoning: &str,
    usage: Usage,
    cits: &[Citation],
) -> Completion {
    Completion {
        id: ident.id.clone(),
        object: "chat.completion",
        created: ident.created,
        model: ident.model.clone(),
        choices: vec![CompletionChoice {
            index: 0,
            message: CompletionMessage {
                role: "assistant".into(),
                content: content.to_string(),
                refusal: None,
                reasoning_content: reasoning.to_string(),
                annotations: annotations(cits),
            },
            logprobs: None,
            finish_reason: "stop".into(),
        }],
        usage,
        citations: cits.iter().map(CitationWire::from).collect(),
    }
}

// ---------------------------------------------------------------------------
// SSE frame rendering (Go streamWriter)
// ---------------------------------------------------------------------------

/// One `data:` frame (Go chunkLocked). A chunk that cannot be encoded is
/// skipped, not fatal — never worth killing a live stream for.
pub fn render_chunk_frame(c: &Chunk) -> String {
    match serde_json::to_string(c) {
        Ok(body) => format!("data: {body}\n\n"),
        Err(_) => String::new(),
    }
}

/// The terminal sentinel frame.
pub fn render_done_frame() -> &'static str {
    "data: [DONE]\n\n"
}

/// A keepalive comment (only after commit — Go keepalive, stream.go:108).
pub const KEEPALIVE_FRAME: &str = ": keepalive\n\n";

/// An in-band error frame: the mapped error object re-emitted as an SSE
/// `data:` frame, then `[DONE]` — never a bare disconnect (ADR 0032/0033,
/// TDD 2.4). The body is the exact OpenAI error JSON the JSON path would
/// have sent.
pub fn render_error_frame(error_body: &serde_json::Value) -> String {
    let frame = serde_json::json!({ "error": error_body });
    format!("data: {frame}\n\n{}", render_done_frame())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sse::Citation;
    use crate::thinking::Piece;

    fn ident() -> StreamIdent {
        StreamIdent {
            id: "chatcmpl-test".into(),
            model: "mimo-v2.6-flash".into(),
            created: 1791792592,
        }
    }

    fn json_of<T: Serialize>(v: &T) -> serde_json::Value {
        serde_json::to_value(v).unwrap()
    }

    // 2.3: role chunk is exactly {"role":"assistant"}, finish_reason null.
    #[test]
    fn role_chunk_shape() {
        let v = json_of(&shape_role_chunk(&ident()));
        assert_eq!(v["object"], "chat.completion.chunk");
        assert_eq!(
            v["choices"][0]["delta"],
            serde_json::json!({"role": "assistant"})
        );
        assert!(v["choices"][0]["finish_reason"].is_null());
    }

    // 2.6: reasoning vs content pieces land in the right delta field.
    #[test]
    fn text_chunk_reasoning_and_content() {
        let r = json_of(&shape_text_chunk(&ident(), &Piece::reasoning("think")));
        assert_eq!(r["choices"][0]["delta"]["reasoning_content"], "think");
        assert!(r["choices"][0]["delta"].get("content").is_none());

        let c = json_of(&shape_text_chunk(&ident(), &Piece::content("answer")));
        assert_eq!(c["choices"][0]["delta"]["content"], "answer");
        assert!(c["choices"][0]["delta"].get("reasoning_content").is_none());
    }

    // 2.3: finish chunk has delta {} and finish_reason "stop".
    #[test]
    fn finish_chunk_empty_delta() {
        let v = json_of(&shape_finish_chunk(&ident()));
        assert_eq!(v["choices"][0]["delta"], serde_json::json!({}));
        assert_eq!(v["choices"][0]["finish_reason"], "stop");
    }

    // 2.9: usage chunk carries choices:[] + both detail objects zero-filled.
    #[test]
    fn usage_chunk_zero_filled_details() {
        let v = json_of(&shape_usage_chunk(&ident(), Usage::default()));
        assert_eq!(v["choices"], serde_json::json!([]));
        let u = &v["usage"];
        assert!(u.get("prompt_tokens_details").is_some());
        assert!(u.get("completion_tokens_details").is_some());
        assert_eq!(u["prompt_tokens_details"]["cached_tokens"], 0);
        assert_eq!(u["completion_tokens_details"]["reasoning_tokens"], 0);
        // Usage itself never omits the detail keys even when zero.
        let s = serde_json::to_string(&shape_usage_chunk(&ident(), Usage::default())).unwrap();
        assert!(s.contains("\"prompt_tokens_details\":"));
        assert!(s.contains("\"completion_tokens_details\":"));
    }

    // 2.2: citations extension chunk carries choices:[] + citations array.
    #[test]
    fn citations_chunk_shape() {
        let cits = vec![Citation {
            index: 0,
            title: "A".into(),
            url: "https://a".into(),
            display_url: "a".into(),
            snippet: "s".into(),
            used: true,
        }];
        let v = json_of(&shape_citations_chunk(&ident(), &cits));
        assert_eq!(v["choices"], serde_json::json!([]));
        assert_eq!(v["citations"][0]["url"], "https://a");
        assert_eq!(v["citations"][0]["used"], true);
        // Unused citations omit the flag (omitempty).
        let s = serde_json::to_string(&shape_citations_chunk(
            &ident(),
            &[Citation {
                used: false,
                ..Default::default()
            }],
        ))
        .unwrap();
        assert!(!s.contains("\"used\""));
    }

    // MapUsage: nativeUsage detail mapping; nil → zeros (TDD API-004).
    #[test]
    fn map_usage_from_mimo() {
        let empty = map_usage(&None);
        assert_eq!(empty, Usage::default());

        let u = MiMoUsage {
            prompt_tokens: 2237,
            completion_tokens: 27,
            total_tokens: 2264,
            native_usage: Some(crate::sse::NativeUsage {
                prompt_tokens_details: Some(crate::sse::NativeCached {
                    cached_tokens: 2176,
                }),
                completion_tokens_details: Some(crate::sse::NativeThink {
                    reasoning_tokens: 24,
                }),
                ..Default::default()
            }),
        };
        let m = map_usage(&Some(u));
        assert_eq!(m.prompt_tokens, 2237);
        assert_eq!(m.completion_tokens, 27);
        assert_eq!(m.total_tokens, 2264);
        assert_eq!(m.prompt_tokens_details.cached_tokens, 2176);
        assert_eq!(m.completion_tokens_details.reasoning_tokens, 24);
    }

    #[test]
    fn add_usage_sums_all_counters() {
        let a = Usage {
            prompt_tokens: 1,
            completion_tokens: 2,
            total_tokens: 3,
            prompt_tokens_details: PromptTokensDetails { cached_tokens: 4 },
            completion_tokens_details: CompletionTokensDetails {
                reasoning_tokens: 5,
            },
        };
        let b = Usage {
            prompt_tokens: 10,
            completion_tokens: 20,
            total_tokens: 30,
            prompt_tokens_details: PromptTokensDetails { cached_tokens: 40 },
            completion_tokens_details: CompletionTokensDetails {
                reasoning_tokens: 50,
            },
        };
        let s = add_usage(a, b);
        assert_eq!(s.prompt_tokens, 11);
        assert_eq!(s.completion_tokens, 22);
        assert_eq!(s.total_tokens, 33);
        assert_eq!(s.prompt_tokens_details.cached_tokens, 44);
        assert_eq!(s.completion_tokens_details.reasoning_tokens, 55);
    }

    // 2.13: non-streaming completion shape.
    #[test]
    fn completion_shape() {
        let v = json_of(&shape_completion(
            &ident(),
            "ping",
            "because",
            Usage::default(),
            &[],
        ));
        assert_eq!(v["object"], "chat.completion");
        let msg = &v["choices"][0]["message"];
        assert_eq!(msg["role"], "assistant");
        assert_eq!(msg["content"], "ping");
        assert!(msg["refusal"].is_null());
        assert_eq!(msg["reasoning_content"], "because");
        assert!(msg.get("annotations").is_none());
        assert!(v["choices"][0]["logprobs"].is_null());
        assert_eq!(v["choices"][0]["finish_reason"], "stop");
        assert!(v.get("citations").is_none());

        // Empty reasoning omits the key entirely (TDD API-001).
        let v = json_of(&shape_completion(
            &ident(),
            "ping",
            "",
            Usage::default(),
            &[],
        ));
        assert!(v["choices"][0]["message"]
            .get("reasoning_content")
            .is_none());
    }

    #[test]
    fn completion_with_citations_carries_annotations_and_top_level() {
        let cits = vec![Citation {
            index: 0,
            title: "T".into(),
            url: "https://t".into(),
            display_url: "t".into(),
            snippet: "sn".into(),
            used: false,
        }];
        let v = json_of(&shape_completion(
            &ident(),
            "a",
            "",
            Usage::default(),
            &cits,
        ));
        let ann = &v["choices"][0]["message"]["annotations"][0];
        assert_eq!(ann["type"], "url_citation");
        assert_eq!(ann["url_citation"]["url"], "https://t");
        assert_eq!(v["citations"][0]["url"], "https://t");
    }

    // 2.4: in-band error frame keeps all four keys + [DONE].
    #[test]
    fn error_frame_shape() {
        let err = serde_json::json!({
            "message": "upstream failure",
            "type": "upstream_error",
            "param": null,
            "code": "upstream_failure"
        });
        let out = render_error_frame(&err);
        assert!(out.starts_with("data: {\"error\":"));
        assert!(out.contains("\"code\":\"upstream_failure\""));
        assert!(out.ends_with("data: [DONE]\n\n"));
    }

    #[test]
    fn frames_render_go_layout() {
        let f = render_chunk_frame(&shape_role_chunk(&ident()));
        assert!(f.starts_with("data: {\"id\":"));
        assert!(f.ends_with("\n\n"));
        assert_eq!(render_done_frame(), "data: [DONE]\n\n");
        assert_eq!(KEEPALIVE_FRAME, ": keepalive\n\n");
    }

    #[test]
    fn ids_have_go_prefix_and_len() {
        assert!(new_chat_id().starts_with("chatcmpl-"));
        assert_eq!(new_chat_id().len(), 9 + 32);
        assert_eq!(random_hex32().len(), 32);
    }

    // R3 fixture equality: the shaper sequence reproduces the Go oracle's
    // own client-side output byte-for-byte (modulo the generated id).
    #[test]
    fn r3_fixture_equality() {
        let raw = std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/fixtures/r3-oracle-stream.txt"
        ))
        .unwrap();
        // Deterministic identity taken from the fixture itself.
        let ident = StreamIdent {
            id: "chatcmpl-3c1337b25c1ffb5c03c9081ebfed787b".into(),
            model: "mimo-v2.6-flash".into(),
            created: 1791792592,
        };
        let mut out = String::new();
        out.push_str(&render_chunk_frame(&shape_role_chunk(&ident)));
        for text in [
            "The user wants",
            " me",
            " to reply with exactly",
            " the word \"",
            "ping\".",
            " This",
            " is a simple request",
            " with",
            " no safety",
            " concerns.",
        ] {
            out.push_str(&render_chunk_frame(&shape_text_chunk(
                &ident,
                &Piece::reasoning(text),
            )));
        }
        out.push_str(&render_chunk_frame(&shape_text_chunk(
            &ident,
            &Piece::content("ping"),
        )));
        out.push_str(&render_chunk_frame(&shape_finish_chunk(&ident)));
        let usage = Usage {
            prompt_tokens: 2237,
            completion_tokens: 27,
            total_tokens: 2264,
            prompt_tokens_details: PromptTokensDetails {
                cached_tokens: 2176,
            },
            completion_tokens_details: CompletionTokensDetails {
                reasoning_tokens: 24,
            },
        };
        out.push_str(&render_chunk_frame(&shape_usage_chunk(&ident, usage)));
        out.push_str(render_done_frame());
        assert_eq!(out, raw);
    }
}
