//! SSE interpreter — port of Go `internal/sse/sse.go`: the upstream
//! JSON-patch SSE stream decoded into text deltas, reasoning deltas,
//! usage, search results, and terminal state.
//!
//! Stream shape (recon §3.5): `data:` lines carry `{p, o, v}` patch ops.
//! Omitted `p`/`o` repeat the previous op. The first data payload carries
//! the full response object under `v.response`. The stream ends with
//! `event: close`; `data: [DONE]` is tolerated but not expected.
//!
//! Driven single-threaded by the plugin's stream task (Go guarded with a
//! mutex for cross-goroutine Snapshot; the Rust caller owns the
//! sequencing, so no interior lock is needed).

use serde::Deserialize;

/// Usage mirrors the OpenAI usage object (from accumulated_token_usage).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Usage {
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub total_tokens: i64,
}

/// One incremental output event. Text and Reasoning are mutually
/// exclusive per delta (Go `Delta`).
#[derive(Debug, Clone, PartialEq)]
pub enum Delta {
    Text(String),
    Reasoning(String),
}

/// An upstream mid-stream error delivered via `event: hint` with payload
/// `{"type":"error","content":…,"finish_reason":…}`. Notable
/// finish_reason: `"parallel_chat_limit"` — another generation is
/// running on the same account; retry, ideally on another account.
#[derive(Debug, Clone, PartialEq, thiserror::Error)]
pub enum StreamError {
    /// The `event: hint` error shape.
    #[error("{}", hint_display(finish_reason, content))]
    Hint {
        kind: String,
        content: String,
        clear_response: bool,
        finish_reason: String,
    },
    /// A content_filter rejection (`data:{"code":"content_filter"}` or an
    /// error object naming it) — typed so the gateway can answer a clean
    /// 400 content_filter instead of a generic upstream failure.
    #[error("content_filter: {0}")]
    ContentFilter(String),
    /// Any other mid-stream error payload.
    #[error("stream error: {0}")]
    Other(String),
}

fn hint_display(finish_reason: &str, content: &str) -> String {
    if !finish_reason.is_empty() {
        format!("stream error ({finish_reason}): {content}")
    } else {
        format!("stream error: {content}")
    }
}

impl StreamError {
    /// The per-account parallel generation limit (Go `IsParallelLimit`).
    pub fn is_parallel_limit(&self) -> bool {
        matches!(self, StreamError::Hint { finish_reason, .. } if finish_reason == "parallel_chat_limit")
    }

    pub fn is_content_filter(&self) -> bool {
        matches!(self, StreamError::ContentFilter(_))
    }
}

/// One structured search hit from the upstream SEARCH fragment's results
/// array (web-search-research.md): url/title/snippet plus citation keying
/// (cite_index) and provenance (query_indexes).
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct SearchResult {
    #[serde(default)]
    pub url: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub snippet: String,
    #[serde(default)]
    pub site_name: String,
    #[serde(default)]
    pub site_icon: String,
    #[serde(default)]
    pub cite_index: i64,
    #[serde(default)]
    pub published_at: f64,
    #[serde(default)]
    pub query_indexes: Vec<i64>,
    #[serde(default)]
    pub provider: Option<String>,
}

/// The accumulated interpreter state (Go `State`).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct State {
    pub text: String,
    pub reasoning: String,
    pub usage: Usage,
    pub finished: bool,
    pub err: Option<StreamError>,
    /// Structured web-search hits when the upstream ran in search mode.
    pub search_results: Vec<SearchResult>,
    /// The queries the upstream model generated for the search fan-out.
    pub search_queries: Vec<String>,
}

/// The interpreter. Feed chunks; take a [`Snapshot`] (a `Clone` of state)
/// when needed.
#[derive(Debug, Default)]
pub struct Interpreter {
    state: State,
    last_path: String,
    last_op: String,
    /// Fragment type that the current append path targets.
    last_kind: String,
    pending: String,
    /// Most recent `event:` line (empty before the first).
    last_event: String,
}

impl Interpreter {
    pub fn new() -> Self {
        Self {
            last_kind: "RESPONSE".into(),
            ..Default::default()
        }
    }

    /// Consume a raw chunk of the SSE byte stream. Lines may split across
    /// chunks at any byte boundary.
    pub fn feed(&mut self, chunk: &str) {
        self.pending.push_str(chunk);
        while let Some(idx) = self.pending.find('\n') {
            let line = self.pending[..idx].trim_end_matches('\r').to_string();
            let rest = self.pending[idx + 1..].to_string();
            self.pending = rest;
            self.handle_line(&line);
        }
    }

    /// A copy of the accumulated state (Go `Snapshot`).
    pub fn snapshot(&self) -> &State {
        &self.state
    }

    pub fn into_state(self) -> State {
        self.state
    }

    /// handleLine: `event:` lines set the terminal flag when they announce
    /// close; `data:` lines are patch ops or payloads.
    fn handle_line(&mut self, line: &str) {
        if self.state.finished {
            return;
        }
        if !line.starts_with("data:") {
            if let Some(ev) = line.strip_prefix("event:") {
                self.last_event = ev.trim().to_string();
                if self.last_event == "close" {
                    self.state.finished = true;
                }
            }
            return;
        }
        let data = line.strip_prefix("data:").unwrap_or("").trim();
        if data.is_empty() {
            return;
        }
        if data == "[DONE]" {
            self.state.finished = true;
            return;
        }

        // The generic patch-op probe: unparseable payloads (e.g.
        // update_session events use data: too) are ignored.
        let Ok(op) = serde_json::from_str::<PatchOp>(data) else {
            return;
        };
        // `event: hint` carries error payloads with their own shape; the
        // generic error/code fields below don't exist there.
        if self.last_event == "hint" {
            if let Ok(hint) = serde_json::from_str::<HintPayload>(data) {
                if hint.kind == "error" {
                    self.state.err = Some(StreamError::Hint {
                        kind: hint.kind,
                        content: hint.content,
                        clear_response: hint.clear_response,
                        finish_reason: hint.finish_reason,
                    });
                    self.state.finished = true;
                }
            }
            return;
        }
        if op.error.is_some() || has_content_filter_code(op.code.as_deref()) {
            let is_filter = has_content_filter_code(op.code.as_deref())
                || op
                    .error
                    .as_ref()
                    .is_some_and(|v| v.to_string().contains("content_filter"));
            let error_text = op.error.as_ref().map(|v| v.to_string()).unwrap_or_default();
            let code_text = op.code.as_ref().map(|v| v.to_string()).unwrap_or_default();
            let detail = if !error_text.is_empty() {
                error_text
            } else if !code_text.is_empty() {
                code_text
            } else {
                "unknown error".into()
            };
            if is_filter {
                self.state.err = Some(StreamError::ContentFilter(detail));
            } else {
                self.state.err = Some(StreamError::Other(detail));
            }
            return;
        }
        let Some(v) = op.v else { return };
        self.apply(&op.p, &op.o, &v);
    }

    /// apply dispatches a single patch operation. path/op may be empty,
    /// meaning "repeat the previous ones" — except that a BATCH never
    /// carries over to a path-less payload: on the live wire a bare
    /// `{"v":"..."}` delta after a response BATCH is fragment content
    /// (search mode), so it routes through the initial-response handler
    /// like every other bare delta.
    fn apply(&mut self, path: &str, op: &str, v: &serde_json::Value) {
        if !path.is_empty() {
            self.last_path = path.to_string();
        }
        if !op.is_empty() {
            self.last_op = op.to_string();
        }
        if path.is_empty() && (op.is_empty() || self.last_op == "BATCH") {
            self.apply_initial_response(v);
        } else if self.last_op == "BATCH" {
            self.apply_batch(v);
        } else if self.last_path == "response/status" {
            self.apply_status(v);
        } else {
            self.apply_path_value(&self.last_path.clone(), v);
        }
    }

    /// The first full-response payload
    /// `{"v":{"response":{"fragments":[…]}}}`, or — when p/o are absent and
    /// v is a bare string — a delta appended to the current fragment.
    fn apply_initial_response(&mut self, v: &serde_json::Value) {
        // Bare string with no p/o: append to the last fragment kind.
        if let Some(bare) = v.as_str() {
            self.emit_fragment(&self.last_kind.clone(), bare);
            return;
        }
        let Ok(full) = serde_json::from_value::<InitialResponse>(v.clone()) else {
            return;
        };
        for f in &full.response.fragments {
            // The last fragment of the initial payload is the target of
            // subsequent bare-string deltas (omitted p/o): track its kind.
            self.last_kind = f.kind.to_uppercase();
            self.emit_fragment(&f.kind, &f.content);
            if f.kind.eq_ignore_ascii_case("SEARCH") || f.kind.eq_ignore_ascii_case("TOOL_SEARCH") {
                for q in &f.queries {
                    self.state.search_queries.push(q.query.clone());
                }
            }
        }
        if full.response.accumulated_token_usage > 0 {
            self.state.usage.total_tokens = full.response.accumulated_token_usage;
        }
        if full.response.status.eq_ignore_ascii_case("FINISHED") {
            self.state.finished = true;
        }
    }

    fn emit_fragment(&mut self, kind: &str, content: &str) {
        if content.is_empty() {
            return;
        }
        if kind.eq_ignore_ascii_case("THINK") || kind.eq_ignore_ascii_case("THINKING") {
            self.state.reasoning.push_str(content);
            return;
        }
        self.state.text.push_str(content);
    }

    fn apply_batch(&mut self, v: &serde_json::Value) {
        let Ok(items) = serde_json::from_value::<Vec<BatchItem>>(v.clone()) else {
            return;
        };
        for item in items {
            match item.p.as_str() {
                "accumulated_token_usage" => {
                    if let Some(n) = item.v.as_i64() {
                        self.state.usage.total_tokens = n;
                    }
                }
                // Search mode: the first RESPONSE fragment chunk arrives
                // nested inside a response BATCH as a fragments APPEND.
                // Route it through the same fragment-append handling as
                // the top-level path.
                "fragments" => self.apply_path_value("response/fragments", &item.v),
                // Nested results APPEND (search mode): same shape as the
                // response/fragments/-1/results path.
                "results" => self.apply_path_value("response/fragments/-1/results", &item.v),
                // quasi_status, has_pending_fragment and other noise: skip.
                _ => {}
            }
        }
    }

    fn apply_status(&mut self, v: &serde_json::Value) {
        if let Some(s) = v.as_str() {
            if s.trim().eq_ignore_ascii_case("FINISHED") {
                self.state.finished = true;
            }
        }
    }

    /// APPEND/SET against concrete paths.
    fn apply_path_value(&mut self, path: &str, v: &serde_json::Value) {
        if is_noise_path(path) {
            return;
        }
        if path == "response/fragments/-1/results" {
            // Structured search hits from the upstream SEARCH fragment.
            // Append: search+thinking mode emits one array per
            // TOOL_SEARCH round.
            if let Ok(results) = serde_json::from_value::<Vec<SearchResult>>(v.clone()) {
                if !results.is_empty() {
                    self.state.search_results.extend(results);
                }
            }
            return;
        }
        if path == "response/fragments/-1/content" || path == "response/content" {
            if let Some(s) = v.as_str() {
                let kind = self.last_kind.clone();
                self.emit_fragment(&kind, s);
            }
            return;
        }
        if path == "response/thinking_content" {
            if let Some(s) = v.as_str() {
                self.state.reasoning.push_str(s);
            }
            return;
        }
        if path == "response/fragments" {
            // APPEND of a new fragment object (e.g. THINK fragment
            // starting).
            if let Ok(frags) = serde_json::from_value::<Vec<Fragment>>(v.clone()) {
                for f in frags {
                    self.last_kind = f.kind.to_uppercase();
                    self.emit_fragment(&f.kind, &f.content);
                }
                return;
            }
            // Bare-string payload on the fragments path (p/o omitted after
            // a fragment APPEND): append to the current fragment kind.
            if let Some(bare) = v.as_str() {
                let kind = self.last_kind.clone();
                self.emit_fragment(&kind, bare);
            }
        }
        // Unknown path with a string value: only a real
        // fragment-content carryover can reach here because noise paths
        // were filtered. Ignore anything else.
    }
}

fn is_noise_path(path: &str) -> bool {
    if path == "response/search_status" {
        return true;
    }
    if path.starts_with("response/fragments/") && path.ends_with("/status") {
        return true;
    }
    [
        "quasi_status",
        "elapsed_secs",
        "token_usage",
        "pending_fragment",
        "conversation_mode",
    ]
    .iter()
    .any(|noise| path.contains(noise))
}

fn has_content_filter_code(code: Option<&str>) -> bool {
    code.is_some_and(|s| s.trim().eq_ignore_ascii_case("content_filter"))
}

// Wire shapes (serde mirrors of the Go anonymous structs).

#[derive(Debug, Deserialize)]
struct PatchOp {
    #[serde(default)]
    p: String,
    #[serde(default)]
    o: String,
    #[serde(default)]
    v: Option<serde_json::Value>,
    /// Raw error object/string; present only on error payloads.
    #[serde(default)]
    error: Option<serde_json::Value>,
    /// Raw code value; present only on error payloads.
    #[serde(default)]
    code: Option<String>,
}

#[derive(Debug, Deserialize)]
struct HintPayload {
    #[serde(rename = "type", default)]
    kind: String,
    #[serde(default)]
    content: String,
    #[serde(default)]
    clear_response: bool,
    #[serde(default)]
    finish_reason: String,
}

#[derive(Debug, Deserialize)]
struct InitialResponse {
    response: InnerResponse,
}

#[derive(Debug, Deserialize)]
struct InnerResponse {
    #[serde(default)]
    status: String,
    #[serde(default)]
    accumulated_token_usage: i64,
    #[serde(default)]
    fragments: Vec<Fragment>,
}

#[derive(Debug, Clone, Deserialize)]
struct Fragment {
    #[serde(rename = "type", default)]
    kind: String,
    /// `null` on the wire for SEARCH fragments awaiting content — Go's
    /// unmarshal tolerates it; mirror that instead of failing the parse.
    #[serde(default, deserialize_with = "null_as_empty")]
    content: String,
    #[serde(default)]
    queries: Vec<Query>,
}

fn null_as_empty<'de, D>(d: D) -> Result<String, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let v = Option::<String>::deserialize(d)?;
    Ok(v.unwrap_or_default())
}

#[derive(Debug, Clone, Deserialize)]
struct Query {
    #[serde(default)]
    query: String,
}

#[derive(Debug, Deserialize)]
struct BatchItem {
    #[serde(default)]
    p: String,
    #[serde(default)]
    v: serde_json::Value,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn collect(input: &str) -> State {
        let mut interp = Interpreter::new();
        interp.feed(input);
        interp.into_state()
    }

    #[test]
    fn omitted_path_and_op_are_sticky() {
        let st = collect(
            "data: {\"p\":\"response/fragments/-1/content\",\"o\":\"APPEND\",\"v\":\"he\"}\n\ndata: {\"v\":\"llo\"}\n",
        );
        assert_eq!(st.text, "hello");
    }

    #[test]
    fn batch_ops_applied_in_order() {
        let st = collect(
            "data: {\"p\":\"response\",\"o\":\"BATCH\",\"v\":[{\"p\":\"accumulated_token_usage\",\"v\":123},{\"p\":\"quasi_status\",\"v\":\"FINISHED\"}]}\n",
        );
        assert_eq!(st.usage.total_tokens, 123);
    }

    #[test]
    fn status_set_finished() {
        let st = collect("data: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n");
        assert!(st.finished);
    }

    #[test]
    fn think_fragments_map_to_reasoning() {
        let input = "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":1,\"type\":\"THINK\",\"content\":\"pondering\"},{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"answer\"}]}}}\n\
                     data: {\"v\":\" more\"}\n";
        let st = collect(input);
        assert_eq!(st.reasoning, "pondering");
        assert_eq!(st.text, "answer more");
    }

    #[test]
    fn skip_noise_paths() {
        let input = "data: {\"p\":\"quasi_status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n\
                     data: {\"p\":\"response/fragments/-1/status\",\"o\":\"SET\",\"v\":\"WIP\"}\n\
                     data: {\"p\":\"elapsed_secs\",\"o\":\"SET\",\"v\":3}\n";
        let st = collect(input);
        assert!(!st.finished, "quasi_status FINISHED must not finish");
        assert_eq!(st.text, "");
    }

    #[test]
    fn mid_stream_error_object() {
        let st = collect(
            "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"partial\"}]}}}\ndata: {\"error\":{\"code\":\"content_filter\"}}\n",
        );
        let err = st.err.expect("expected stream error");
        assert!(err.to_string().contains("content_filter"));
        assert!(matches!(err, StreamError::ContentFilter(_)));
        assert_eq!(st.text, "partial", "text before error is preserved");
    }

    /// A generic (non-content-filter) error object keeps the plain
    /// stream-error shape and must NOT classify as content_filter.
    #[test]
    fn generic_error_object_not_content_filter() {
        let st = collect("data: {\"error\":\"server exploded\"}\n");
        let err = st.err.expect("expected stream error");
        assert!(matches!(err, StreamError::Other(_)));
        assert!(!err.is_content_filter());
    }

    #[test]
    fn done_sentinel_tolerated() {
        let st = collect(
            "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"hi\"}]}}}\ndata: [DONE]\n",
        );
        assert_eq!(st.text, "hi");
        assert!(st.finished);
    }

    #[test]
    fn incremental_feed_across_chunk_boundaries() {
        let mut interp = Interpreter::new();
        for chunk in [
            "data: {\"p\":\"response/fragm",
            "ents/-1/content\",\"o\":\"APPEND\",\"v\":\"a\"}\n\ndata: {\"v\":\"b\"}\n",
        ] {
            interp.feed(chunk);
        }
        assert_eq!(interp.snapshot().text, "ab");
    }

    #[test]
    fn event_close_terminates() {
        let st = collect("event: close\ndata: {\"click_behavior\":\"none\"}\n");
        assert!(st.finished);
    }

    #[test]
    fn non_data_lines_ignored() {
        let st = collect(
            "event: ready\ndata: {\"request_message_id\":1}\n\nevent: title\ndata: {\"content\":\"你好\"}\n",
        );
        assert_eq!(st.text, "");
    }

    /// The upstream signals mid-stream errors via `event: hint` with a
    /// JSON payload (live-captured 2026-09-19: another generation is
    /// running on the same account). The interpreter must surface it, not
    /// swallow it into an empty completion.
    #[test]
    fn hint_error_surfaces() {
        let stream = "event: ready\ndata: {\"request_message_id\":1,\"response_message_id\":2,\"model_type\":\"default\"}\n\n\
                      event: hint\ndata: {\"type\":\"error\",\"content\":\"another generation is running\",\"clear_response\":true,\"finish_reason\":\"parallel_chat_limit\"}\n\n\
                      event: close\ndata: {}\n";
        let st = collect(stream);
        let err = st.err.expect("hint error must surface");
        let StreamError::Hint {
            finish_reason,
            content,
            clear_response,
            ..
        } = &err
        else {
            panic!("want Hint, got {err:?}");
        };
        assert_eq!(finish_reason, "parallel_chat_limit");
        assert!(content.contains("another generation"));
        assert!(*clear_response);
        assert!(err.is_parallel_limit());
    }

    #[test]
    fn stream_error_display_and_parallel_limit() {
        let e = StreamError::Hint {
            kind: "error".into(),
            content: "busy".into(),
            clear_response: false,
            finish_reason: "parallel_chat_limit".into(),
        };
        assert!(e.is_parallel_limit());
        assert_eq!(e.to_string(), "stream error (parallel_chat_limit): busy");
        let plain = StreamError::Hint {
            kind: "error".into(),
            content: "x".into(),
            clear_response: false,
            finish_reason: String::new(),
        };
        assert_eq!(plain.to_string(), "stream error: x");
        assert!(!plain.is_parallel_limit());
    }

    #[test]
    fn content_filter_code() {
        let st = collect("data: {\"code\":\"content_filter\",\"error\":\"blocked\"}\n\n");
        let err = st.err.expect("err");
        assert!(err.to_string().contains("blocked"));
        assert!(err.is_content_filter());
    }

    /// An error object naming content_filter (no code field) also
    /// classifies as content_filter: the gateway keys the typed 400 off
    /// it.
    #[test]
    fn content_filter_error_object_matches() {
        let st = collect("data: {\"error\":{\"code\":\"content_filter\"}}\n\n");
        assert!(st.err.expect("err").is_content_filter());
    }

    #[test]
    fn fragments_append_path() {
        let stream = "data: {\"v\":{\"response\":{\"fragments\":[{\"type\":\"RESPONSE\",\"content\":\"start\"}]}}}\n\
                      data: {\"p\":\"response/fragments\",\"o\":\"APPEND\",\"v\":[{\"type\":\"RESPONSE\",\"content\":\" more\"}]}\n\
                      data: {\"p\":\"response/fragments/-1/content\",\"v\":\" tail\"}\n";
        let st = collect(stream);
        assert_eq!(st.text, "start more tail");
    }

    /// Reasoning-mode regression (thinking default ON): a bare-string
    /// delta after the initial payload continues the LAST fragment's
    /// kind — a THINK fragment followed by `{"v":"..."}` deltas is all
    /// reasoning, not content; and a bare string on the
    /// response/fragments path (after a fragment APPEND, p/o omitted)
    /// appends to the current fragment kind.
    #[test]
    fn bare_delta_continues_last_fragment_kind() {
        let stream = "data: {\"v\":{\"response\":{\"fragments\":[{\"type\":\"THINK\",\"content\":\"think \"}]}}}\n\
                      data: {\"v\":\"deeply \"}\n\
                      data: {\"p\":\"response/fragments\",\"o\":\"APPEND\",\"v\":[{\"type\":\"RESPONSE\",\"content\":\"ans\"}]}\n\
                      data: {\"v\":\"wer\"}\n";
        let st = collect(stream);
        assert_eq!(st.reasoning, "think deeply ");
        assert_eq!(st.text, "answer");
    }

    /// Search-mode regressions (web-search-research.md risk #4):
    /// 1. a `response` BATCH may carry a fragments APPEND (first RESPONSE
    ///    chunk in search mode) — must route through fragment handling;
    /// 2. `response/fragments/-1/results` carries the structured search
    ///    results array — must be captured;
    /// 3. the SEARCH fragment's `queries` must be captured;
    /// 4. the `response/fragments/-1` BATCH (search-fragment status +
    ///    summary content) is noise — the summary must NOT leak into the
    ///    answer text.
    #[test]
    fn batch_nested_fragments_append_is_text() {
        let stream = "data: {\"v\":{\"response\":{\"fragments\":[{\"type\":\"SEARCH\",\"status\":\"WIP\",\"content\":null,\"queries\":[{\"query\":\"q1\"}],\"results\":[]}]}}}\n\
                      data: {\"p\":\"response/fragments/-1/results\",\"v\":[{\"url\":\"https://example.com\",\"title\":\"Example\",\"snippet\":\"s\",\"cite_index\":1}]}\n\
                      data: {\"p\":\"response/fragments/-1\",\"o\":\"BATCH\",\"v\":[{\"p\":\"status\",\"v\":\"FINISHED\"},{\"p\":\"content\",\"v\":\"搜索到 20 个网页\"}]}\n\
                      data: {\"p\":\"response\",\"o\":\"BATCH\",\"v\":[{\"p\":\"fragments\",\"o\":\"APPEND\",\"v\":[{\"id\":3,\"type\":\"RESPONSE\",\"content\":\"今天是\",\"references\":[],\"stage_id\":2}]},{\"p\":\"has_pending_fragment\",\"o\":\"SET\",\"v\":false}]}\n\
                      data: {\"v\":\"9月20日\"}\n\
                      data: {\"p\":\"response\",\"o\":\"BATCH\",\"v\":[{\"p\":\"accumulated_token_usage\",\"v\":350},{\"p\":\"quasi_status\",\"v\":\"FINISHED\"}]}\n\
                      event: close\n";
        let st = collect(stream);
        assert_eq!(
            st.text, "今天是9月20日",
            "first BATCH-nested fragment chunk must not be dropped"
        );
        assert!(st.finished, "event: close must finish");
        assert_eq!(st.usage.total_tokens, 350);
        assert_eq!(st.search_results.len(), 1);
        assert_eq!(st.search_results[0].url, "https://example.com");
        assert_eq!(st.search_results[0].title, "Example");
        assert_eq!(st.search_results[0].cite_index, 1);
        assert_eq!(st.search_queries, vec!["q1".to_string()]);
    }
}
