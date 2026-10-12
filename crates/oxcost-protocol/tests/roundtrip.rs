//! Serde round-trip tests against canonical OpenAI JSON shapes (synthetic).

use oxcost_protocol::*;

fn roundtrip<T>(value: &T)
where
    T: serde::Serialize + serde::de::DeserializeOwned + PartialEq + std::fmt::Debug,
{
    let json = serde_json::to_string(value).expect("serialize");
    let back: T = serde_json::from_str(&json).expect("deserialize");
    assert_eq!(&back, value, "round-trip mismatch for {}", json);
}

#[test]
fn request_minimal_roundtrip() {
    let req: ChatCompletionRequest = serde_json::from_str(
        r#"{
            "model": "example-remote/example-chat",
            "messages": [
                {"role": "system", "content": "You are terse."},
                {"role": "user", "content": "Hello"}
            ]
        }"#,
    )
    .expect("parse minimal request");
    roundtrip(&req);
    // Optional fields must be absent, not null, on the wire.
    let json = serde_json::to_value(&req).unwrap();
    assert!(json.get("stream").is_none());
    assert!(json.get("tools").is_none());
}

#[test]
fn request_full_roundtrip() {
    let req: ChatCompletionRequest = serde_json::from_str(
        r#"{
            "model": "example-remote/example-chat",
            "messages": [
                {"role": "user", "content": [
                    {"type": "text", "text": "What is in this image?"},
                    {"type": "image_url", "image_url": {"url": "https://example.invalid/cat.png"}}
                ]},
                {"role": "assistant", "content": null, "tool_calls": [
                    {"id": "call_1", "type": "function",
                     "function": {"name": "lookup", "arguments": "{\"q\":\"cat\"}"}}
                ]},
                {"role": "tool", "tool_call_id": "call_1", "content": "a cat"}
            ],
            "temperature": 0.7,
            "top_p": 0.9,
            "n": 1,
            "stream": false,
            "stop": ["\n", "END"],
            "max_tokens": 128,
            "tools": [
                {"type": "function", "function": {
                    "name": "lookup",
                    "description": "Look things up",
                    "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}
                }}
            ],
            "tool_choice": {"type": "function", "function": {"name": "lookup"}}
        }"#,
    )
    .expect("parse full request");
    roundtrip(&req);

    let Some(UserContent::Parts(parts)) = req
        .messages
        .first()
        .map(|m| match m {
            ChatMessage::User(UserMessage { content }) => Some(content),
            _ => None,
        })
        .flatten()
    else {
        panic!("first message should be multimodal user content");
    };
    assert_eq!(parts.len(), 2);
}

#[test]
fn request_tool_choice_modes_roundtrip() {
    for mode in ["none", "auto", "required"] {
        let req: ChatCompletionRequest = serde_json::from_str(&format!(
            r#"{{"model": "m", "messages": [{{"role": "user", "content": "hi"}}],
                "tool_choice": "{mode}"}}"#
        ))
        .expect("parse tool_choice mode");
        roundtrip(&req);
        assert!(matches!(req.tool_choice, Some(ToolChoice::Mode(_))));
    }
}

#[test]
fn response_roundtrip() {
    let resp: ChatCompletionResponse = serde_json::from_str(
        r#"{
            "id": "chatcmpl-abc123",
            "object": "chat.completion",
            "created": 1760000000,
            "model": "example-remote/example-chat",
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": "Hello there."},
                    "finish_reason": "stop"
                }
            ],
            "usage": {"prompt_tokens": 9, "completion_tokens": 3, "total_tokens": 12}
        }"#,
    )
    .expect("parse response");
    roundtrip(&resp);
    assert_eq!(resp.choices[0].finish_reason, Some(FinishReason::Stop));
}

#[test]
fn response_tool_call_roundtrip() {
    let resp: ChatCompletionResponse = serde_json::from_str(
        r#"{
            "id": "chatcmpl-tool1",
            "object": "chat.completion",
            "created": 1760000001,
            "model": "m",
            "choices": [
                {
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": null,
                        "tool_calls": [
                            {"id": "call_9", "type": "function",
                             "function": {"name": "f", "arguments": "{}"}}
                        ]
                    },
                    "finish_reason": "tool_calls"
                }
            ]
        }"#,
    )
    .expect("parse tool-call response");
    roundtrip(&resp);
}

#[test]
fn sse_chunk_roundtrip() {
    let ev: SseEvent = serde_json::from_str(
        r#"{
            "id": "chatcmpl-abc123",
            "object": "chat.completion.chunk",
            "created": 1760000000,
            "model": "example-remote/example-chat",
            "choices": [
                {"index": 0, "delta": {"role": "assistant"}, "finish_reason": null}
            ]
        }"#,
    )
    .expect("parse first chunk");
    roundtrip(&ev);

    let mid: SseEvent = serde_json::from_str(
        r#"{
            "id": "chatcmpl-abc123",
            "object": "chat.completion.chunk",
            "created": 1760000000,
            "model": "example-remote/example-chat",
            "choices": [{"index": 0, "delta": {"content": "Hel"}}]
        }"#,
    )
    .expect("parse mid chunk");
    roundtrip(&mid);

    let tail: SseEvent = serde_json::from_str(
        r#"{
            "id": "chatcmpl-abc123",
            "object": "chat.completion.chunk",
            "created": 1760000000,
            "model": "example-remote/example-chat",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]
        }"#,
    )
    .expect("parse tail chunk");
    roundtrip(&tail);

    // Empty delta serializes as {} (all fields optional/skipped).
    assert_eq!(
        serde_json::to_value(&tail.choices[0].delta).unwrap(),
        serde_json::json!({})
    );
}

#[test]
fn sse_tool_call_delta_roundtrip() {
    let ev: SseEvent = serde_json::from_str(
        r#"{
            "id": "chatcmpl-t",
            "object": "chat.completion.chunk",
            "created": 1760000002,
            "model": "m",
            "choices": [
                {"index": 0, "delta": {"tool_calls": [
                    {"index": 0, "id": "call_1", "type": "function",
                     "function": {"name": "lookup", "arguments": ""}}
                ]}}
            ]
        }"#,
    )
    .expect("parse tool-call delta chunk");
    roundtrip(&ev);
}

#[test]
fn sse_done_wire_token() {
    assert_eq!(SseDone::WIRE, "[DONE]");
}
