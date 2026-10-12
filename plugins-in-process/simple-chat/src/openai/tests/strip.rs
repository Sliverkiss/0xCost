//! Go test port: `strip_test.go` — the junk-field stripping report, the
//! system-merge table, and message-shape residue. Offline fixtures only.

use serde_json::json;

use super::super::*;
use super::{assert_messages, assert_stripped, msg, msg_parts, parse, parse_str, MsgExpect};

fn kitchen_sink_body() -> serde_json::Value {
    // Built incrementally: the full kitchen-sink nesting exceeds the
    // default macro recursion budget (the Go fixture is one literal).
    let mut b = json!({
        "model": "deepseek-flash",
        "stream": true,
        "temperature": 0.7,
        "top_p": 0.9,
        "max_tokens": 512,
        "thinking": {"type": "disabled"},
        "search": {"type": "enabled"},
        "tools": [{"type": "function", "function": {"name": "x"}}],
        "tool_choice": "auto",
        "functions": [{"name": "f"}],
        "function_call": "none",
        "parallel_tool_calls": true,
        "logprobs": true,
        "top_logprobs": 5,
        "logit_bias": {"1": 2},
        "user": "u1",
        "store": false,
        "metadata": {"a": "b"},
        "service_tier": "auto",
        "response_format": {"type": "text"},
        "seed": 42,
        "stop": ["\n"],
        "n": 1,
        "stream_options": {"include_usage": true},
        "presence_penalty": 0.5,
        "frequency_penalty": 0.5,
    });
    b["messages"] = json!([
        {"role": "system", "content": "You are terse."},
        {"role": "system", "content": "Second system."},
        {"role": "user", "content": "hello", "name": "alice"},
        {"role": "assistant", "tool_calls": [{"id": "1", "function": {"name": "x", "arguments": "{}"}}]},
        {"role": "assistant", "content": "partial answer", "tool_calls": [{"id": "2", "function": {"name": "y", "arguments": "{}"}}]},
        {"role": "tool", "content": "tool result text"},
        {"role": "tool", "content": ""}
    ]);
    b["messages"]
        .as_array_mut()
        .unwrap()
        .push(json!({"role": "user", "content": [
            {"type": "text", "text": "describe"},
            {"type": "audio", "audio": "x"},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}
        ]}));
    b
}

#[test]
fn parse_request_kitchen_sink() {
    let req = parse(kitchen_sink_body()).unwrap();
    // Exactly what survives the sanitizer: the two system messages merge
    // into the first user message as a prefix; everything else unchanged.
    assert_messages(
        &req,
        vec![
            msg(
                "user",
                format!("You are terse.\n\nSecond system.{SYSTEM_MERGE_SEPARATOR}hello"),
            ),
            msg("assistant", "partial answer"),
            msg("tool", "tool result text"),
            msg_parts(
                "user",
                "",
                vec![
                    (PartKind::Text, "describe", ""),
                    (PartKind::ImageUrl, "", "data:image/png;base64,aGVsbG8="),
                ],
            ),
        ],
    );
    // Kept flags ride through untouched.
    assert!(req.stream && req.temperature == 0.7 && req.top_p == 0.9 && req.max_tokens == 512);
    assert!(
        !req.thinking_enabled,
        "thinking disabled must parse as disabled"
    );
    assert!(req.search_enabled, "search enabled must parse as enabled");
    // Every stripped category is reported, exactly once per field.
    assert_stripped(
        &req,
        &[
            "tools",
            "tool_choice",
            "functions",
            "function_call",
            "parallel_tool_calls",
            "logprobs",
            "top_logprobs",
            "logit_bias",
            "user",
            "store",
            "metadata",
            "service_tier",
            "response_format",
            "seed",
            "stop",
            "n",
            "stream_options",
            "presence_penalty",
            "frequency_penalty",
            "system_messages=2",
            "tool_messages=2",
            "junk_content_parts=1",
            "message_names=1",
        ],
    );
    for s in &req.stripped {
        if s.starts_with("system_messages") || s.starts_with("tool_messages") {
            assert!(
                s.contains('='),
                "counter entry {s:?} must be count-suffixed"
            );
        }
    }
}

#[test]
fn parse_request_tools_accepted() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "tools": [{"type": "function", "function": {"name": "x"}}], "tool_choice": "auto"
    }))
    .unwrap();
    assert_eq!(req.messages.len(), 1);
    assert_eq!(req.messages[0].content, "hi");
    assert_stripped(&req, &["tools", "tool_choice"]);
}

#[test]
fn parse_request_null_junk_fields_not_reported() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "tools": null, "user": null
    }))
    .unwrap();
    assert_eq!(
        req.stripped.len(),
        0,
        "null junk fields must not be reported"
    );
}

#[test]
fn parse_request_zero_and_empty_junk_not_reported() {
    // `0` and `[]` count as absent too (Go strippedFields' trimmed
    // string comparison).
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "n": 0, "stop": [], "stream_options": []
    }))
    .unwrap();
    assert_eq!(req.stripped.len(), 0, "{:?}", req.stripped);
    // Nonzero / nonempty DO report.
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [{"role": "user", "content": "hi"}],
        "n": 2, "stop": ["\n"], "logprobs": true, "store": false
    }))
    .unwrap();
    assert_stripped(&req, &["n", "stop", "logprobs", "store"]);
}

#[test]
fn parse_request_merges_all_system_messages_into_first_user() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [
            {"role": "system", "content": "s1"},
            {"role": "user", "content": "u1"},
            {"role": "system", "content": "s2"},
            {"role": "system", "content": "s3"},
            {"role": "assistant", "content": "a1"},
            {"role": "user", "content": "u2"}
        ]
    }))
    .unwrap();
    assert_messages(
        &req,
        vec![
            msg("user", format!("s1\n\ns2\n\ns3{SYSTEM_MERGE_SEPARATOR}u1")),
            msg("assistant", "a1"),
            msg("user", "u2"),
        ],
    );
    assert_stripped(&req, &["system_messages=3"]);
}

#[test]
fn parse_request_system_merge_table() {
    struct Case {
        name: &'static str,
        body: serde_json::Value,
        want_msgs: Vec<MsgExpect>,
        want_count: &'static str, // "" = no system_messages entry
        want_err: &'static str,   // "" = expect success
    }
    let cases = vec![
        Case {
            name: "multiple system messages merge in original order",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": "first"},
                {"role": "system", "content": "second"},
                {"role": "user", "content": "question"}
            ]}),
            want_msgs: vec![msg(
                "user",
                format!("first\n\nsecond{SYSTEM_MERGE_SEPARATOR}question"),
            )],
            want_count: "system_messages=2",
            want_err: "",
        },
        Case {
            name: "system-only conversation becomes a user message",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": "only system"}
            ]}),
            want_msgs: vec![msg("user", "only system")],
            want_count: "system_messages=1",
            want_err: "",
        },
        Case {
            name: "system between user turns still merges into the first user",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "user", "content": "u1"},
                {"role": "system", "content": "mid"},
                {"role": "assistant", "content": "a1"},
                {"role": "user", "content": "u2"}
            ]}),
            want_msgs: vec![
                msg("user", format!("mid{SYSTEM_MERGE_SEPARATOR}u1")),
                msg("assistant", "a1"),
                msg("user", "u2"),
            ],
            want_count: "system_messages=1",
            want_err: "",
        },
        Case {
            name: "empty system content skipped without counting",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": ""},
                {"role": "system", "content": "real"},
                {"role": "user", "content": "q"}
            ]}),
            want_msgs: vec![msg("user", format!("real{SYSTEM_MERGE_SEPARATOR}q"))],
            want_count: "system_messages=1",
            want_err: "",
        },
        Case {
            name: "multipart system content concatenates text parts",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": [
                    {"type": "text", "text": "part one"},
                    {"type": "text", "text": "part two"}
                ]},
                {"role": "user", "content": "q"}
            ]}),
            want_msgs: vec![msg(
                "user",
                format!("part one\npart two{SYSTEM_MERGE_SEPARATOR}q"),
            )],
            want_count: "system_messages=1",
            want_err: "",
        },
        Case {
            name: "system with only junk parts is skipped",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": [{"type": "audio", "audio": "x"}]},
                {"role": "user", "content": "q"}
            ]}),
            want_msgs: vec![msg("user", "q")],
            want_count: "",
            want_err: "",
        },
        Case {
            name: "all system messages empty still errors",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": ""}
            ]}),
            want_msgs: vec![],
            want_count: "",
            want_err: "messages must not be empty",
        },
        Case {
            name: "no user message: system merges into new leading user message",
            body: json!({"model": "deepseek-flash", "messages": [
                {"role": "system", "content": "s1"},
                {"role": "assistant", "content": "a1"},
                {"role": "system", "content": "s2"}
            ]}),
            want_msgs: vec![msg("user", "s1\n\ns2"), msg("assistant", "a1")],
            want_count: "system_messages=2",
            want_err: "",
        },
    ];
    for tc in cases {
        let got = parse(tc.body);
        if !tc.want_err.is_empty() {
            let err = got.expect_err(tc.name);
            assert!(err.to_string().contains(tc.want_err), "{}: {err}", tc.name);
            continue;
        }
        let req = got.unwrap_or_else(|e| panic!("{}: {e}", tc.name));
        assert_messages(&req, tc.want_msgs.clone());
        if tc.want_count.is_empty() {
            for s in &req.stripped {
                assert!(
                    !s.starts_with("system_messages"),
                    "{}: unexpected {s:?}",
                    tc.name
                );
            }
        } else {
            assert_stripped(&req, &[tc.want_count]);
        }
    }
}

#[test]
fn parse_request_tool_calls_carrier() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [
            {"role": "user", "content": "go"},
            {"role": "assistant", "tool_calls": [{"id": "1", "function": {"name": "x", "arguments": "{}"}}]},
            {"role": "assistant", "content": "did it", "function_call": {"name": "x", "arguments": "{}"}},
            {"role": "user", "content": "thanks"}
        ]
    }))
    .unwrap();
    assert_messages(
        &req,
        vec![
            msg("user", "go"),
            msg("assistant", "did it"),
            msg("user", "thanks"),
        ],
    );
    assert_stripped(&req, &["tool_messages=1"]);
}

#[test]
fn parse_request_tool_role_messages() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [
            {"role": "user", "content": "go"},
            {"role": "tool", "content": "result"},
            {"role": "tool", "content": ""},
            {"role": "tool", "tool_call_id": "1", "content": "another result"}
        ]
    }))
    .unwrap();
    assert_messages(
        &req,
        vec![
            msg("user", "go"),
            msg("tool", "result"),
            msg("tool", "another result"),
        ],
    );
    assert_stripped(&req, &["tool_messages=1"]);
}

#[test]
fn parse_request_junk_content_parts() {
    let req = parse(json!({
        "model": "deepseek-flash", "messages": [
            {"role": "user", "content": [
                {"type": "text", "text": "keep me"},
                {"type": "audio", "audio": "x"},
                {"type": "file", "file": {"url": "http://x"}},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}
            ]},
            {"role": "user", "content": [
                {"type": "video", "video": "x"},
                {"type": "audio", "audio": "y"}
            ]}
        ]
    }))
    .unwrap();
    assert_messages(
        &req,
        vec![
            msg_parts(
                "user",
                "",
                vec![
                    (PartKind::Text, "keep me", ""),
                    (PartKind::ImageUrl, "", "data:image/png;base64,aGVsbG8="),
                ],
            ),
            // All-junk array → empty parts, message survives as empty
            // text (same shape as a literal "content": []).
            msg_parts("user", "", vec![]),
        ],
    );
    assert_stripped(&req, &["junk_content_parts=4"]);
}

#[test]
fn parse_request_empty_messages_still_errors() {
    assert!(parse_str(r#"{"model": "deepseek-flash", "messages": []}"#).is_err());
}

// A tool-only transcript stripped to nothing names the cause (Go's
// stripped-everything error).
#[test]
fn parse_request_stripped_everything_names_cause() {
    let err = parse(json!({
        "model": "deepseek-flash",
        "messages": [
            {"role": "assistant", "tool_calls": [{"id": "1", "function": {"name": "x", "arguments": "{}"}}]}
        ]
    }))
    .unwrap_err();
    assert!(
        err.to_string()
            .contains("all messages were removed by sanitization"),
        "{err}"
    );
    assert_eq!(err, ParseError::StrippedEverything);
}

// Malformed content types.
#[test]
fn parse_request_content_number_rejected() {
    let err = parse(json!({
        "model": "deepseek-flash",
        "messages": [{"role": "user", "content": 42}]
    }))
    .unwrap_err();
    assert_eq!(err, ParseError::InvalidContent);
}

#[test]
fn data_url_decode_edge_cases() {
    // Unsupported MIME.
    assert!(decode_data_url("data:image/tiff;base64,aA==").is_none());
    // Missing comma.
    assert!(decode_data_url("data:image/png;base64").is_none());
    // Non-base64 data URL form (no ;base64 marker).
    assert!(decode_data_url("data:image/png,raw").is_none());
    // Bad base64 payload.
    assert!(decode_data_url("data:image/png;base64,!!!").is_none());
    // Not a data URL.
    assert!(decode_data_url("https://x/y.png").is_none());
}

#[test]
fn first_segment_strips_query_and_caps() {
    assert_eq!(first_segment("https://h/p?token=secret"), "https://h/p");
    assert_eq!(first_segment("https://h/p"), "https://h/p");
    let long = "a".repeat(200);
    assert_eq!(first_segment(&long).len(), 128);
}

#[test]
fn image_ext_by_mime_variants() {
    assert_eq!(image_ext_by_mime("image/png"), Some("png"));
    assert_eq!(image_ext_by_mime("Image/PNG"), Some("png"));
    assert_eq!(image_ext_by_mime("image/png; charset=binary"), Some("png"));
    assert_eq!(image_ext_by_mime("image/jpeg"), Some("jpg"));
    assert_eq!(image_ext_by_mime("image/jpg"), Some("jpg"));
    assert_eq!(image_ext_by_mime("image/webp"), Some("webp"));
    assert_eq!(image_ext_by_mime("image/gif"), Some("gif"));
    assert_eq!(image_ext_by_mime("text/html"), None);
    assert_eq!(image_ext_by_mime(""), None);
}
