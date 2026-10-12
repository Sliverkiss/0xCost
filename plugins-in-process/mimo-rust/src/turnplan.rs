//! Turn planner + permissive request decoding: 1:1 port of Go
//! `internal/adapter/turnplan.go` + the decode/validation half of
//! `internal/api/request.go` (SDD §5.2/§5.3). Pure functions, no I/O —
//! offline testable against the same TDD vectors.

use serde_json::Value;

use crate::models::normalize_model;

/// One inbound message projected onto the planner's shape (Go `adapter.Msg`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Msg {
    pub role: String,
    pub content: String,
}

/// The queries one client request replays upstream, in order (Go `TurnPlan`).
/// v1 carries no images (image input is out of scope), so the plan is text
/// only — the Go `Media` dimension is dropped, not silently ignored: a request
/// with image parts is refused at decode time (see [`ParseError::ImageUrl`]).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TurnPlan {
    pub queries: Vec<String>,
}

/// The query sent for a user message that carries images but no text
/// (Go `DefaultImageOnlyQuery`, turnplan.go:28). Kept verbatim even though v1
/// refuses images, so the planner stays textually 1:1.
pub const DEFAULT_IMAGE_ONLY_QUERY: &str = "请描述这张图片。";

/// Errors raised while decoding/validating one request body (Go request.go
/// sentinels; the mapper translates them — see `errmap`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ParseError {
    /// Malformed JSON body / non-object / no messages / empty role.
    MalformedRequest,
    /// `reasoning_effort` outside the OpenAI set.
    UnsupportedReasoningEffort,
    /// `web_search_options.search_context_size` outside low|medium|high.
    UnsupportedSearchContextSize,
    /// `web_search_status` outside disabled|auto|enabled.
    UnsupportedSearchStatus,
    /// An image part arrived on a chat-only v1 plugin (capability-gated).
    ImageUrl(String),
    /// A user message content that is neither string nor parts array.
    MalformedContent,
}

/// The decoded, validated request (Go `chatRequest` after the pre-pool
/// validation pass — everything the pipeline needs, nothing the upstream
/// does not).
#[derive(Debug, Clone, Default)]
pub struct ChatInput {
    pub msgs: Vec<Msg>,
    /// Echoed model id exactly as the client named it.
    pub requested_model: String,
    pub stream: bool,
    pub temperature: Option<f64>,
    pub top_p: Option<f64>,
    /// Reasoning switch resolved per Go precedence (reasoning_effort >
    /// enable_thinking > reasoning.type > model default, ADR 0031).
    pub thinking: bool,
    /// Upstream search switch (disabled|auto|enabled).
    pub search: String,
    /// include_citations (default true, M19).
    pub citations: bool,
}

/// Which field set the thinking switch, for tests.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ThinkingSource {
    Effort,
    EnableThinking,
    ReasoningType,
    Default,
}

/// Permissive decode + validation of one chat body (Go decodeChatBody +
/// the pre-pool checks chat.go:38-105). Returns the planner-shaped messages
/// and the resolved knobs, or the first validation error in Go's order:
/// decode → reasoning_effort → search_context_size → search_status → image
/// refusal (v1) → content flattening.
pub fn decode_chat_body(body: &Value, default_model: &str) -> Result<ChatInput, ParseError> {
    decode_chat_body_with_default_thinking(body, default_model, true)
}

/// Same, with the model-scoped thinking default injected (Go
/// `thinkingDefaultFor`: catalog-declared when known, else ON).
pub fn decode_chat_body_with_default_thinking(
    body: &Value,
    default_model: &str,
    thinking_default_on: bool,
) -> Result<ChatInput, ParseError> {
    let obj = body.as_object().ok_or(ParseError::MalformedRequest)?;
    let messages = obj
        .get("messages")
        .and_then(Value::as_array)
        .ok_or(ParseError::MalformedRequest)?;
    if messages.is_empty() {
        return Err(ParseError::MalformedRequest);
    }

    // reasoning_effort (OSTD-003): a typo is a 400 naming the set.
    let effort = field_str(obj, "reasoning_effort");
    match effort.trim().to_ascii_lowercase().as_str() {
        "" | "none" | "minimal" | "low" | "medium" | "high" => {}
        _ => return Err(ParseError::UnsupportedReasoningEffort),
    }

    // web_search_options.search_context_size (OSTD-005): accepted, validated,
    // reported as dropped (the upstream switch has no size dimension).
    if let Some(wso) = obj.get("web_search_options") {
        let size = wso
            .get("search_context_size")
            .and_then(Value::as_str)
            .unwrap_or_default();
        match size.trim().to_ascii_lowercase().as_str() {
            "" | "low" | "medium" | "high" => {}
            _ => return Err(ParseError::UnsupportedSearchContextSize),
        }
    }

    // web_search_status (M19, TDD WS-002).
    let status = field_str(obj, "web_search_status");
    let status = match status.trim().to_ascii_lowercase().as_str() {
        "" | "auto" | "enabled" | "disabled" => status.trim().to_ascii_lowercase(),
        _ => return Err(ParseError::UnsupportedSearchStatus),
    };

    // Model id: "auto"/"" → the deployment default; the ":online" suffix is
    // the search shorthand (M19, TDD WS-003).
    let mut requested = field_str(obj, "model");
    let mut search = status;
    if let Some(base) = requested.strip_suffix(":online") {
        requested = base.to_string();
        if search.is_empty() {
            search = "enabled".to_string();
        }
    }
    if requested.trim().is_empty() || requested.trim().eq_ignore_ascii_case("auto") {
        requested = default_model.to_string();
    }
    if search.is_empty() {
        // web_search_options present = search this turn; else disabled.
        search = if obj.get("web_search_options").is_some() {
            "enabled".to_string()
        } else {
            "disabled".to_string()
        };
    }

    // Messages → planner shape. Content: string, or a parts array whose text
    // parts join with '\n' (permissive decode — image parts are refused: v1
    // declares no image capability, and silently ignoring them would look
    // like a working upload).
    let mut msgs = Vec::with_capacity(messages.len());
    for m in messages {
        let obj = m.as_object().ok_or(ParseError::MalformedRequest)?;
        let role = field_str(obj, "role");
        if role.is_empty() {
            return Err(ParseError::MalformedRequest);
        }
        let content = match m.get("content") {
            None => String::new(),
            Some(Value::String(s)) => s.clone(),
            Some(Value::Array(parts)) => {
                let mut texts = Vec::with_capacity(parts.len());
                for p in parts {
                    match p.get("type").and_then(Value::as_str).unwrap_or("") {
                        "text" | "" => texts.push(
                            p.get("text")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .to_string(),
                        ),
                        "image_url" => {
                            return Err(ParseError::ImageUrl(
                                "image parts are not supported by this plugin".into(),
                            ))
                        }
                        // Unknown part types ignored, not rejected (SDD §5.2).
                        _ => {}
                    }
                }
                texts.join("\n")
            }
            Some(_) => return Err(ParseError::MalformedContent),
        };
        msgs.push(Msg { role, content });
    }

    // Thinking switch (ADR 0031).
    let (thinking, _src) = resolve_thinking(obj, &effort, thinking_default_on);

    // Citations default true (M19).
    let citations = obj
        .get("include_citations")
        .and_then(Value::as_bool)
        .unwrap_or(true);

    Ok(ChatInput {
        stream: obj.get("stream").and_then(Value::as_bool).unwrap_or(false),
        temperature: obj.get("temperature").and_then(Value::as_f64),
        top_p: obj.get("top_p").and_then(Value::as_f64),
        requested_model: requested,
        msgs,
        thinking,
        search,
        citations,
    })
}

/// thinkingEnabled (request.go:169-180): `reasoning_effort` decides whenever
/// present ("none" = explicit off, every other accepted value = on);
/// otherwise `enable_thinking`; otherwise `reasoning.type`; otherwise the
/// model's declared default.
fn resolve_thinking(
    obj: &serde_json::Map<String, Value>,
    effort: &str,
    default_on: bool,
) -> (bool, ThinkingSource) {
    let v = effort.trim().to_ascii_lowercase();
    if !v.is_empty() {
        return (v != "none", ThinkingSource::Effort);
    }
    if let Some(b) = obj.get("enable_thinking").and_then(Value::as_bool) {
        return (b, ThinkingSource::EnableThinking);
    }
    if let Some(r) = obj
        .get("reasoning")
        .and_then(|r| r.get("type"))
        .and_then(Value::as_str)
    {
        if !r.trim().is_empty() {
            return (
                r.eq_ignore_ascii_case("enabled"),
                ThinkingSource::ReasoningType,
            );
        }
    }
    (default_on, ThinkingSource::Default)
}

/// PlanFullReplay (turnplan.go:49-92): one query per user message, in order;
/// system messages concatenate and splice into the first user query as
/// `[systems]\n\n---\n\n<first>` (API-017); assistant history is not re-sent
/// (accepted trade-off, SDD §5.3B). Errors: no sendable user message, or
/// replay cap exceeded.
pub fn plan_full_replay(msgs: &[Msg], max_turns: usize) -> Result<TurnPlan, PlanError> {
    let mut system_parts: Vec<String> = Vec::new();
    let mut user_contents: Vec<&Msg> = Vec::new();
    for m in msgs {
        match m.role.as_str() {
            "system" => {
                let t = m.content.trim();
                if !t.is_empty() {
                    system_parts.push(t.to_string());
                }
            }
            "user" if !m.content.is_empty() => {
                user_contents.push(m);
            }
            _ => {}
        }
    }
    if user_contents.is_empty() {
        return Err(PlanError::NoUserMessage);
    }
    if max_turns > 0 && user_contents.len() > max_turns {
        return Err(PlanError::TooManyTurns {
            turns: user_contents.len(),
            cap: max_turns,
        });
    }
    let mut queries = Vec::with_capacity(user_contents.len());
    for (i, m) in user_contents.iter().enumerate() {
        let mut q = m.content.clone();
        if !system_parts.is_empty() && i == 0 {
            q = format!("{}\n\n---\n\n{}", system_parts.join("\n\n"), q);
        }
        queries.push(q);
    }
    Ok(TurnPlan { queries })
}

/// Planner errors (Go ErrNoUserMessage / ErrTooManyTurns).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PlanError {
    NoUserMessage,
    TooManyTurns { turns: usize, cap: usize },
}

/// ComposedFirstQueryLen (turnplan.go:103-125): the length (in chars) of the
/// query the planner would send first, plus how much of it is preamble the
/// client did not write — the cap check the API runs BEFORE any pool
/// interaction (TDD API-016).
pub fn composed_first_query_len(msgs: &[Msg]) -> (usize, usize) {
    let mut system_parts: Vec<&str> = Vec::new();
    let mut first = "";
    for m in msgs {
        match m.role.as_str() {
            "system" => {
                let t = m.content.trim();
                if !t.is_empty() {
                    system_parts.push(t);
                }
            }
            "user" if first.is_empty() && !m.content.is_empty() => {
                first = &m.content;
            }
            _ => {}
        }
    }
    let mut composed = first.chars().count();
    let mut overhead = 0;
    if !system_parts.is_empty() {
        let preamble = format!("{}\n\n---\n\n", system_parts.join("\n\n"));
        overhead = preamble.chars().count();
        composed += overhead;
    }
    (composed, overhead)
}

/// Resolves the model through the static catalog, returning the fastchat
/// routing bit alongside (chat.go:86-91 — the superseded-id `redirectTo`
/// resolution is upstream-catalog driven and lands with the live catalog
/// milestone; v1 normalizes against the static floor exactly like Go's
/// fallback path).
pub fn resolve_model(
    input: &ChatInput,
    default_model: &str,
) -> Result<crate::models::NormalizedModel, crate::models::ModelNotFound> {
    normalize_model(&input.requested_model, default_model, None)
}

fn field_str(obj: &serde_json::Map<String, Value>, key: &str) -> String {
    obj.get(key)
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn decode(v: Value) -> Result<ChatInput, ParseError> {
        decode_chat_body(&v, "mimo-v2.6-pro")
    }

    fn body(model: &str, content: &str) -> Value {
        json!({"model": model, "messages": [{"role": "user", "content": content}]})
    }

    #[test]
    fn plain_body_decodes() {
        let i = decode(body("mimo-v2.6-flash", "hi")).unwrap();
        assert_eq!(i.msgs.len(), 1);
        assert_eq!(i.msgs[0].content, "hi");
        assert!(!i.stream);
        assert!(i.thinking); // default on (ADR 0031)
        assert_eq!(i.search, "disabled");
        assert!(i.citations);
    }

    #[test]
    fn auto_model_resolves_to_default() {
        let i = decode(body("auto", "x")).unwrap();
        assert_eq!(i.requested_model, "mimo-v2.6-pro");
    }

    #[test]
    fn missing_messages_or_non_object_malformed() {
        assert_eq!(
            decode(json!({"model": "m"})).unwrap_err(),
            ParseError::MalformedRequest
        );
        assert_eq!(
            decode(json!([1])).unwrap_err(),
            ParseError::MalformedRequest
        );
        assert_eq!(
            decode(json!({"model": "m", "messages": []})).unwrap_err(),
            ParseError::MalformedRequest
        );
    }

    #[test]
    fn empty_role_is_malformed() {
        let mut v = body("m", "x");
        v["messages"][0]["role"] = json!("");
        assert_eq!(decode(v).unwrap_err(), ParseError::MalformedRequest);
    }

    // TDD API-010/011, OSTD-001..003: thinking precedence.
    #[test]
    fn thinking_precedence() {
        // reasoning_effort wins over everything.
        let mut v = body("m", "x");
        v["reasoning_effort"] = json!("none");
        v["enable_thinking"] = json!(true);
        assert!(!decode(v).unwrap().thinking);

        let mut v = body("m", "x");
        v["reasoning_effort"] = json!("low");
        assert!(decode(v).unwrap().thinking);

        // enable_thinking explicit false is off.
        let mut v = body("m", "x");
        v["enable_thinking"] = json!(false);
        assert!(!decode(v).unwrap().thinking);

        // reasoning.type decides when the others are absent.
        let mut v = body("m", "x");
        v["reasoning"] = json!({"type": "disabled"});
        assert!(!decode(v.clone()).unwrap().thinking);
        v["reasoning"] = json!({"type": "enabled"});
        assert!(decode(v).unwrap().thinking);

        // Default applies when nothing was sent.
        let v = body("m", "x");
        assert!(
            !decode_chat_body_with_default_thinking(&v, "mimo-v2.6-pro", false)
                .unwrap()
                .thinking
        );
    }

    #[test]
    fn bad_reasoning_effort_rejected() {
        let mut v = body("m", "x");
        v["reasoning_effort"] = json!("ultra");
        assert_eq!(
            decode(v).unwrap_err(),
            ParseError::UnsupportedReasoningEffort
        );
    }

    // TDD WS-002 / OSTD-005.
    #[test]
    fn bad_search_context_size_and_status_rejected() {
        let mut v = body("m", "x");
        v["web_search_options"] = json!({"search_context_size": "huge"});
        assert_eq!(
            decode(v).unwrap_err(),
            ParseError::UnsupportedSearchContextSize
        );

        let mut v = body("m", "x");
        v["web_search_status"] = json!("sometimes");
        assert_eq!(decode(v).unwrap_err(), ParseError::UnsupportedSearchStatus);
    }

    // TDD WS-001/003: search switch resolution.
    #[test]
    fn search_resolution() {
        let mut v = body("m", "x");
        v["web_search_options"] = json!({});
        assert_eq!(decode(v).unwrap().search, "enabled");

        let mut v = body("m", "x");
        v["web_search_status"] = json!("auto");
        assert_eq!(decode(v).unwrap().search, "auto");

        // status wins over options.
        let mut v = body("m", "x");
        v["web_search_status"] = json!("disabled");
        v["web_search_options"] = json!({});
        assert_eq!(decode(v).unwrap().search, "disabled");

        // :online suffix turns search on.
        let i = decode(body("mimo-v2.6-flash:online", "x")).unwrap();
        assert_eq!(i.requested_model, "mimo-v2.6-flash");
        assert_eq!(i.search, "enabled");
    }

    #[test]
    fn include_citations_default_true() {
        let mut v = body("m", "x");
        v["include_citations"] = json!(false);
        assert!(!decode(v).unwrap().citations);
    }

    // 1.4: permissive content decode.
    #[test]
    fn content_parts_join_with_newline() {
        let mut v = body("m", "x");
        v["messages"][0]["content"] =
            json!([{"type": "text", "text": "a"}, {"type": "text", "text": "b"}]);
        assert_eq!(decode(v).unwrap().msgs[0].content, "a\nb");
    }

    #[test]
    fn unknown_part_types_ignored() {
        let mut v = body("m", "x");
        v["messages"][0]["content"] = json!([{"type": "audio", "audio": {}}, {"text": "t"}]);
        assert_eq!(decode(v).unwrap().msgs[0].content, "t");
    }

    #[test]
    fn image_parts_refused_in_v1() {
        let mut v = body("m", "x");
        v["messages"][0]["content"] =
            json!([{"type": "image_url", "image_url": {"url": "https://x/a.png"}}]);
        assert!(matches!(decode(v).unwrap_err(), ParseError::ImageUrl(_)));
    }

    #[test]
    fn non_string_non_array_content_malformed() {
        let mut v = body("m", "x");
        v["messages"][0]["content"] = json!(42);
        assert_eq!(decode(v).unwrap_err(), ParseError::MalformedContent);
    }

    // TDD API-017: system splice.
    #[test]
    fn plan_splices_systems_into_first_user_query() {
        let msgs = vec![
            Msg {
                role: "system".into(),
                content: "be terse".into(),
            },
            Msg {
                role: "system".into(),
                content: "and dry".into(),
            },
            Msg {
                role: "user".into(),
                content: "hello".into(),
            },
            Msg {
                role: "assistant".into(),
                content: "hi".into(),
            },
            Msg {
                role: "user".into(),
                content: "again".into(),
            },
        ];
        let plan = plan_full_replay(&msgs, 20).unwrap();
        assert_eq!(
            plan.queries,
            vec![
                "be terse\n\nand dry\n\n---\n\nhello".to_string(),
                "again".to_string()
            ]
        );
    }

    #[test]
    fn plan_no_user_message() {
        let msgs = vec![Msg {
            role: "system".into(),
            content: "s".into(),
        }];
        assert_eq!(
            plan_full_replay(&msgs, 20).unwrap_err(),
            PlanError::NoUserMessage
        );
        // Assistant-only history is equally unsendable.
        let msgs = vec![Msg {
            role: "assistant".into(),
            content: "a".into(),
        }];
        assert_eq!(
            plan_full_replay(&msgs, 20).unwrap_err(),
            PlanError::NoUserMessage
        );
    }

    // TDD API-015: replay cap.
    #[test]
    fn plan_replay_cap() {
        let msgs: Vec<Msg> = (0..21)
            .map(|i| Msg {
                role: "user".into(),
                content: format!("m{i}"),
            })
            .collect();
        assert_eq!(
            plan_full_replay(&msgs, 20).unwrap_err(),
            PlanError::TooManyTurns { turns: 21, cap: 20 }
        );
        assert!(plan_full_replay(&msgs[..20], 20).is_ok());
    }

    // Composed first query length counts the preamble (ADR 0015).
    #[test]
    fn composed_first_query_len_with_overhead() {
        let msgs = vec![
            Msg {
                role: "system".into(),
                content: "abc".into(),
            },
            Msg {
                role: "user".into(),
                content: "hello".into(),
            },
        ];
        let (composed, overhead) = composed_first_query_len(&msgs);
        assert_eq!(composed, "abc\n\n---\n\nhello".chars().count());
        assert_eq!(overhead, "abc\n\n---\n\n".chars().count());
        assert_eq!(composed - overhead, 5);

        // No system: overhead zero, composed = user text.
        let msgs = vec![Msg {
            role: "user".into(),
            content: "hello".into(),
        }];
        assert_eq!(composed_first_query_len(&msgs), (5, 0));
    }

    #[test]
    fn plan_allows_empty_user_content_only_when_systems_exist() {
        // The Go planner keeps empty user messages only when they carry
        // media; with text-only Msgs an empty content is dropped.
        let msgs = vec![Msg {
            role: "user".into(),
            content: String::new(),
        }];
        assert_eq!(
            plan_full_replay(&msgs, 20).unwrap_err(),
            PlanError::NoUserMessage
        );
    }
}
