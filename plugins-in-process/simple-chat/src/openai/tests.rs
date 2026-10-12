//! Go test ports: `openai_test.go`, `params_test.go`, `reasoning_test.go`,
//! `search_test.go` — offline fixtures only. The `strip_test.go` port
//! lives in [`tests::strip`].

use serde_json::json;

use super::*;

fn parse(body: serde_json::Value) -> Result<Request, ParseError> {
    parse_request(&body)
}

fn parse_str(body: &str) -> Result<Request, ParseError> {
    let v: serde_json::Value = serde_json::from_str(body).unwrap();
    parse_request(&v)
}

/// The surviving-message projection (Go `msgExpect`): role, string
/// content, and parts.
#[derive(Debug, Clone, PartialEq)]
struct MsgExpect {
    role: String,
    content: String,
    parts: Vec<(PartKind, &'static str, &'static str)>,
}

fn msg(role: &str, content: impl Into<String>) -> MsgExpect {
    MsgExpect {
        role: role.into(),
        content: content.into(),
        parts: vec![],
    }
}

fn msg_parts(
    role: &str,
    content: impl Into<String>,
    parts: Vec<(PartKind, &'static str, &'static str)>,
) -> MsgExpect {
    MsgExpect {
        role: role.into(),
        content: content.into(),
        parts,
    }
}

fn assert_messages(req: &Request, want: Vec<MsgExpect>) {
    assert_eq!(
        req.messages.len(),
        want.len(),
        "messages = {:?}, want {:?}",
        req.messages,
        want
    );
    for (m, w) in req.messages.iter().zip(want) {
        assert_eq!(m.role, w.role, "role mismatch: {:?}", m);
        assert_eq!(m.content, w.content, "content mismatch: {:?}", m);
        let got: Vec<(PartKind, &str, &str)> = m
            .parts
            .iter()
            .map(|p| (p.kind, p.text.as_str(), p.image_url.as_str()))
            .collect();
        assert_eq!(got, w.parts, "parts mismatch: {:?}", m);
    }
}

fn assert_stripped(req: &Request, want: &[&str]) {
    for w in want {
        assert!(
            req.stripped.iter().any(|s| s == w),
            "stripped list {:?} missing {w:?}",
            req.stripped
        );
    }
}

// ---- openai_test.go ----

#[test]
fn parse_request_rejects_bad_model() {
    let err = parse(json!({"model": "gpt-4", "messages": [{"role": "user", "content": "hi"}]}))
        .unwrap_err();
    assert!(err.to_string().contains("deepseek-flash"), "{err}");
}

#[test]
fn parse_request_tools_stripped_not_rejected() {
    let req = parse_str(
        r#"{"model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}], "tools": [{"type": "function"}]}"#,
    )
    .unwrap();
    assert_eq!(req.messages.len(), 1);
    assert_stripped(&req, &["tools"]);
}

#[test]
fn parse_request_rejects_empty_messages() {
    assert!(parse_str(r#"{"model": "deepseek-flash", "messages": []}"#).is_err());
}

#[test]
fn parse_request_accepts_valid() {
    let req = parse_str(
        r#"{
        "model": "deepseek-flash",
        "stream": true,
        "temperature": 0.5,
        "messages": [
            {"role": "system", "content": "You are terse."},
            {"role": "user", "content": "hello"}
        ]
    }"#,
    )
    .unwrap();
    assert!(req.stream, "stream flag lost");
    assert_eq!(req.temperature, 0.5);
}

#[test]
fn parse_request_accepts_multipart_content() {
    let req = parse_str(
        r#"{
        "model": "deepseek-flash",
        "messages": [{
            "role": "user",
            "content": [
                {"type": "text", "text": "describe this image"},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}
            ]
        }]
    }"#,
    )
    .unwrap();
    assert_eq!(req.messages.len(), 1);
    let m = &req.messages[0];
    assert_eq!(
        m.content, "",
        "string content should be empty for array message"
    );
    assert_eq!(m.parts.len(), 2);
    assert_eq!(
        (m.parts[0].kind, m.parts[0].text.as_str()),
        (PartKind::Text, "describe this image")
    );
    assert_eq!(
        (m.parts[1].kind, m.parts[1].image_url.as_str()),
        (PartKind::ImageUrl, "data:image/png;base64,aGVsbG8=")
    );
}

#[test]
fn flatten_messages_role_tagged() {
    let msgs = vec![
        Message {
            role: "system".into(),
            content: "be brief".into(),
            ..Default::default()
        },
        Message {
            role: "user".into(),
            content: "hi".into(),
            ..Default::default()
        },
        Message {
            role: "assistant".into(),
            content: "hello".into(),
            ..Default::default()
        },
        Message {
            role: "user".into(),
            content: "bye".into(),
            ..Default::default()
        },
    ];
    let got = flatten_messages(&msgs);
    for want in ["system: ", "user: ", "assistant: "] {
        assert!(got.contains(want), "missing role tag {want:?}: {got:?}");
    }
    assert!(
        got.contains("be brief") && got.contains("bye"),
        "content lost: {got:?}"
    );
    // System message flattened like any other role, not injected.
    assert_eq!(got.matches("system: ").count(), 1, "{got:?}");
}

#[test]
fn flatten_messages_multipart_content() {
    let msgs = vec![Message {
        role: "user".into(),
        parts: vec![
            ContentPart {
                kind: PartKind::Text,
                text: "what is in this image".into(),
                ..Default::default()
            },
            ContentPart {
                kind: PartKind::ImageUrl,
                image_url: "data:image/png;base64,aGVsbG8=".into(),
                ..Default::default()
            },
        ],
        ..Default::default()
    }];
    let got = flatten_messages(&msgs);
    assert!(
        got.contains("what is in this image"),
        "text part lost: {got:?}"
    );
    assert!(
        !got.contains("aGVsbG8="),
        "image data must not leak into prompt: {got:?}"
    );
}

#[test]
fn extract_data_url_image_inline() {
    // The pure half of Go TestExtractImagesDataURL (the live fetch half
    // is image_fetch's tests): a data URL decodes inline, never fetched.
    let msgs = vec![Message {
        role: "user".into(),
        parts: vec![
            ContentPart {
                kind: PartKind::Text,
                text: "look".into(),
                ..Default::default()
            },
            ContentPart {
                kind: PartKind::ImageUrl,
                image_url: "data:image/png;base64,aGVsbG8=".into(),
                ..Default::default()
            },
        ],
        ..Default::default()
    }];
    let urls = crate::image_fetch::http_image_urls(&msgs);
    assert!(urls.is_empty(), "data URL must not be fetched: {urls:?}");
    let (data, ext) = decode_data_url("data:image/png;base64,aGVsbG8=").unwrap();
    assert_eq!(data, b"hello");
    assert_eq!(ext, "png");
}

// ---- params_test.go ----

#[test]
fn parse_request_max_completion_tokens() {
    let cases = [
        (
            "max_completion_tokens alone maps onto max_tokens",
            json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":256}),
            256,
        ),
        (
            "max_tokens alone still works",
            json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":128}),
            128,
        ),
        (
            "both present: max_completion_tokens wins",
            json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":128,"max_completion_tokens":256}),
            256,
        ),
        (
            "zero max_completion_tokens falls back to max_tokens",
            json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":128,"max_completion_tokens":0}),
            128,
        ),
        (
            "neither present stays zero",
            json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}),
            0,
        ),
    ];
    for (name, body, want) in cases {
        let req = parse(body).unwrap();
        assert_eq!(req.max_tokens, want, "{name}");
    }
}

#[test]
fn parse_request_sampling_params_survive() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "temperature": 0.7, "top_p": 0.9, "max_completion_tokens": 512,
        "presence_penalty": 0.5, "frequency_penalty": 0.5, "stop": ["\n"]
    }))
    .unwrap();
    assert_eq!(req.temperature, 0.7);
    assert_eq!(req.top_p, 0.9);
    assert_eq!(req.max_tokens, 512);
}

// ---- reasoning_test.go ----

#[test]
fn parse_request_thinking_default_on() {
    let req =
        parse(json!({"model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}]}))
            .unwrap();
    assert!(
        req.thinking_enabled,
        "absent thinking switch must default to enabled"
    );
}

#[test]
fn parse_request_thinking_null_is_default() {
    let req = parse(json!({
        "model": "deepseek-flash", "thinking": null,
        "messages": [{"role": "user", "content": "hi"}]
    }))
    .unwrap();
    assert!(req.thinking_enabled);
}

#[test]
fn parse_request_thinking_explicit_enabled() {
    let req = parse(json!({
        "model": "deepseek-flash", "thinking": {"type": "enabled"},
        "messages": [{"role": "user", "content": "hi"}]
    }))
    .unwrap();
    assert!(req.thinking_enabled);
}

#[test]
fn parse_request_thinking_explicit_disabled() {
    let req = parse(json!({
        "model": "deepseek-flash", "thinking": {"type": "disabled"},
        "messages": [{"role": "user", "content": "hi"}]
    }))
    .unwrap();
    assert!(!req.thinking_enabled);
}

#[test]
fn parse_request_thinking_malformed_value() {
    for body in [
        json!({"model":"deepseek-flash","thinking":{"type":"banana"},"messages":[{"role":"user","content":"hi"}]}),
        json!({"model":"deepseek-flash","thinking":{"type":123},"messages":[{"role":"user","content":"hi"}]}),
        json!({"model":"deepseek-flash","thinking":{"level":"high"},"messages":[{"role":"user","content":"hi"}]}),
        json!({"model":"deepseek-flash","thinking":"disabled","messages":[{"role":"user","content":"hi"}]}),
        json!({"model":"deepseek-flash","thinking":true,"messages":[{"role":"user","content":"hi"}]}),
        json!({"model":"deepseek-flash","thinking":[],"messages":[{"role":"user","content":"hi"}]}),
    ] {
        assert!(parse(body).is_err(), "malformed switch must be rejected");
    }
}

// ---- search_test.go ----

#[test]
fn parse_search_switch() {
    struct Case {
        name: &'static str,
        body: serde_json::Value,
        want: bool,
        want_err: bool,
    }
    let cases = [
        Case {
            name: "absent",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}),
            want: false,
            want_err: false,
        },
        Case {
            name: "null",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"search":null}),
            want: false,
            want_err: false,
        },
        Case {
            name: "enabled",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"search":{"type":"enabled"}}),
            want: true,
            want_err: false,
        },
        Case {
            name: "disabled",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"search":{"type":"disabled"}}),
            want: false,
            want_err: false,
        },
        Case {
            name: "bad type value",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"search":{"type":"maybe"}}),
            want: false,
            want_err: true,
        },
        Case {
            name: "non-object",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"search":"enabled"}),
            want: false,
            want_err: true,
        },
        Case {
            name: "missing type",
            body: json!({"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}],"search":{}}),
            want: false,
            want_err: true,
        },
    ];
    for tc in cases {
        match parse(tc.body) {
            Ok(req) => {
                assert!(!tc.want_err, "{}: want error, got nil", tc.name);
                assert_eq!(req.search_enabled, tc.want, "{}", tc.name);
            }
            Err(_) => assert!(tc.want_err, "{}: unexpected error", tc.name),
        }
    }
}

// OpenAI-native search signals (dossier §5 F03) map onto search_enabled.
#[test]
fn openai_native_search_signals() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "web_search_options": {"search_context_size": "medium"}
    }))
    .unwrap();
    assert!(req.search_enabled, "web_search_options must enable search");

    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "web_search_status": "enabled"
    }))
    .unwrap();
    assert!(
        req.search_enabled,
        "web_search_status: enabled must enable search"
    );

    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "web_search_status": "disabled"
    }))
    .unwrap();
    assert!(
        !req.search_enabled,
        "web_search_status: disabled must not enable search"
    );
}

#[cfg(test)]
mod strip;
