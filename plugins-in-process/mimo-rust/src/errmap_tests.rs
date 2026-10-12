//! errmap unit tests (Go errors.go MapError table, PARITY §3 rows).
//! Included from errmap.rs to keep that file under the 800-line cap.

use super::*;
use std::time::Duration;

fn typ(e: &MappedError) -> &str {
    &e.body.error.typ
}

fn code(e: &MappedError) -> &str {
    e.body.error.code.as_deref().unwrap_or("")
}

fn body_of(e: &MappedError) -> serde_json::Value {
    serde_json::to_value(&e.body).unwrap()
}

// 3.1: no accounts → 503 api_error no_accounts.
#[test]
fn row_no_accounts() {
    let m = map_error(&PipelineError::NoAccounts);
    assert_eq!(m.status, 503);
    assert_eq!(typ(&m), "api_error");
    assert_eq!(code(&m), "no_accounts");
    assert!(m.retry_after.is_none());
}

// 3.2: parked → 429 rate_limit_error pool_parked + Retry-After.
#[test]
fn row_pool_parked() {
    let m = map_error(&PipelineError::PoolParked {
        retry_after_seconds: 30,
    });
    assert_eq!(m.status, 429);
    assert_eq!(typ(&m), "rate_limit_error");
    assert_eq!(code(&m), "pool_parked");
    assert_eq!(m.retry_after, Some(30));
}

// 3.3: queue wait → 429 rate_limit_error pool_busy + Retry-After.
#[test]
fn row_pool_busy() {
    let m = map_error(&PipelineError::PoolBusy {
        retry_after_seconds: 12,
    });
    assert_eq!(m.status, 429);
    assert_eq!(typ(&m), "rate_limit_error");
    assert_eq!(code(&m), "pool_busy");
    assert_eq!(m.retry_after, Some(12));
}

// 3.4: unknown model → 404 invalid_request_error model_not_found.
#[test]
fn row_model_not_found() {
    let m = map_error(&PipelineError::ModelNotFound {
        requested: "gpt-4o".into(),
    });
    assert_eq!(m.status, 404);
    assert_eq!(typ(&m), "invalid_request_error");
    assert_eq!(code(&m), "model_not_found");
    assert!(m.body.error.message.contains("gpt-4o"));
}

// 3.5: no user message → 400 no_user_message.
#[test]
fn row_no_user_message() {
    let m = map_error(&PipelineError::NoUserMessage);
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "no_user_message");
}

// 3.6: replay cap → 400 too_many_turns.
#[test]
fn row_too_many_turns() {
    let m = map_error(&PipelineError::TooManyTurns { max: 50 });
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "too_many_turns");
    assert!(m.body.error.message.contains("50"));
}

// 3.7: malformed body → 400 malformed_request_body.
#[test]
fn row_malformed_request() {
    let m = map_error(&PipelineError::MalformedRequest);
    assert_eq!(m.status, 400);
    assert_eq!(typ(&m), "invalid_request_error");
    assert_eq!(code(&m), "malformed_request_body");
}

// 3.8/3.9: effort and search_context_size → param naming the field.
#[test]
fn rows_param_names_the_field() {
    let m = map_error(&PipelineError::UnsupportedReasoningEffort);
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "unsupported_reasoning_effort");
    assert_eq!(m.body.error.param.as_deref(), Some("reasoning_effort"));

    let m = map_error(&PipelineError::UnsupportedSearchContextSize);
    assert_eq!(code(&m), "unsupported_search_context_size");
    assert_eq!(
        m.body.error.param.as_deref(),
        Some("web_search_options.search_context_size")
    );
}

// 3.10: unsupported search status → 400 unsupported_search_status.
#[test]
fn row_unsupported_search_status() {
    let m = map_error(&PipelineError::UnsupportedSearchStatus);
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "unsupported_search_status");
}

// 3.11: image url refusal (v1: decode refusal).
#[test]
fn row_image_url() {
    let m = map_error(&PipelineError::ImageUrl {
        reason: "image parts are not supported by this model".into(),
    });
    assert_eq!(m.status, 400);
    assert_eq!(typ(&m), "invalid_request_error");
    assert_eq!(code(&m), "unsupported_image_url");
}

// 3.12: content filter → verbatim reason + message (ADR 0033).
#[test]
fn row_content_filter() {
    let m = map_error(&PipelineError::ContentFilter {
        reason: "sensitive_query".into(),
        message: "您的查询包含敏感内容".into(),
    });
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "content_filter");
    assert!(m.body.error.message.contains("sensitive_query"));
    assert!(m.body.error.message.contains("您的查询包含敏感内容"));
}

// 3.13: client-side too long → context_too_long + numbers + param.
#[test]
fn row_query_too_long() {
    let m = map_error(&PipelineError::QueryTooLong {
        limit: 102400,
        actual: 110000,
        model: "mimo-v2.6-pro".into(),
        overhead: 200,
    });
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "context_too_long");
    assert_eq!(m.body.error.param.as_deref(), Some("messages"));
    let msg = &m.body.error.message;
    assert!(msg.contains("110000"));
    assert!(msg.contains("102400"));
    assert!(msg.contains("mimo-v2.6-pro"));
    assert!(msg.contains("200 chars are the injected/system preamble"));
    // Zero overhead: no preamble suffix.
    let m = map_error(&PipelineError::QueryTooLong {
        limit: 102400,
        actual: 110000,
        model: "mimo-v2.6-pro".into(),
        overhead: 0,
    });
    assert!(!m.body.error.message.contains("preamble"));
}

// 3.14: channel model refusal → 400 model_not_available_on_channel,
// upstream wording appended verbatim.
#[test]
fn row_upstream_model_rejected() {
    let m = map_error(&PipelineError::UpstreamModelRejected {
        upstream_message: "模型不存在".into(),
    });
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "model_not_available_on_channel");
    assert!(m.body.error.message.contains("模型不存在"));
    // Empty upstream message: sentence stands alone.
    let m = map_error(&PipelineError::UpstreamModelRejected {
        upstream_message: String::new(),
    });
    assert_eq!(
        m.body.error.message,
        "the upstream channel does not serve this model id"
    );
}

// 3.15: upstream busy surviving retry → 503 + Retry-After 2.
#[test]
fn row_upstream_busy() {
    let m = map_error(&PipelineError::UpstreamBusy {
        upstream_message: "当前访问较为繁忙".into(),
    });
    assert_eq!(m.status, 503);
    assert_eq!(code(&m), "upstream_busy");
    assert_eq!(m.retry_after, Some(BUSY_RETRY_AFTER_SECONDS));
    assert!(m.body.error.message.contains("当前访问较为繁忙"));
}

// 3.16: 401 after refresh retry → 401 upstream_auth_expired.
#[test]
fn row_auth_expired() {
    let m = map_error(&PipelineError::AuthExpired);
    assert_eq!(m.status, 401);
    assert_eq!(typ(&m), "invalid_request_error");
    assert_eq!(code(&m), "upstream_auth_expired");
}

// 3.17: upstream 429 → 429 rate_limit_error upstream_rate_limited.
#[test]
fn row_upstream_429() {
    let m = map_error(&PipelineError::Upstream(UpstreamError {
        status: 429,
        body: "limited".into(),
        retry_after: None,
    }));
    assert_eq!(m.status, 429);
    assert_eq!(typ(&m), "rate_limit_error");
    assert_eq!(code(&m), "upstream_rate_limited");
    // The upstream's own Retry-After is NOT echoed (ERRMAP-011).
    assert!(m.retry_after.is_none());
}

// 3.18: upstream prose "too long" in a 5xx envelope → 400
// context_too_long (the client's mistake, the client's status).
#[test]
fn row_upstream_too_long_translation() {
    for body in [
        "Sorry, the text you sent is too long!",
        "query is too long",
        "长度不能超过限制",
        "内容过长",
        "文本过长",
        "code context_too_long",
        "too_long_error",
    ] {
        let m = map_error(&PipelineError::Upstream(UpstreamError {
            status: 502,
            body: body.into(),
            retry_after: None,
        }));
        assert_eq!(m.status, 400, "body {body:?}");
        assert_eq!(code(&m), "context_too_long");
    }
    // Not too long: stays a 502.
    let m = map_error(&PipelineError::Upstream(UpstreamError {
        status: 500,
        body: "internal error".into(),
        retry_after: None,
    }));
    assert_eq!(m.status, 502);
    assert_eq!(code(&m), "upstream_failure");
}

// 3.19: upstream 5xx → 502 (pinned deviation: type upstream_error,
// code preserved).
#[test]
fn row_upstream_5xx_deviation() {
    let m = map_error(&PipelineError::Upstream(UpstreamError {
        status: 500,
        body: "boom".into(),
        retry_after: None,
    }));
    assert_eq!(m.status, 502);
    assert_eq!(typ(&m), "upstream_error");
    assert_eq!(code(&m), "upstream_failure");
    assert_eq!(m.body.error.message, "upstream error 500");
}

// 3.20: transport/stall/silence → 502 upstream_error upstream_failure
// (deviation from Go's api_error; codes preserved).
#[test]
fn row_upstream_failure_deviation() {
    let m = map_error(&PipelineError::UpstreamFailure {
        detail: "connection reset".into(),
    });
    assert_eq!(m.status, 502);
    assert_eq!(typ(&m), "upstream_error");
    assert_eq!(code(&m), "upstream_failure");
    assert_eq!(m.body.error.message, "upstream failure: connection reset");
    // Mid-stream "too long" error event → same translation.
    let m = map_error(&PipelineError::UpstreamFailure {
        detail: "Sorry, the text you sent is too long!".into(),
    });
    assert_eq!(m.status, 400);
    assert_eq!(code(&m), "context_too_long");
}

// 3.21: the JSON body always carries all four keys (OSTD-013).
#[test]
fn body_always_carries_four_keys() {
    let m = map_error(&PipelineError::NoAccounts);
    let v = body_of(&m);
    let e = &v["error"];
    for k in ["message", "type", "param", "code"] {
        assert!(e.get(k).is_some(), "key {k} missing");
    }
    assert!(e["param"].is_null());
    assert!(e["code"].is_null() || e["code"].is_string());
}

// 3.19: gateway budget/lease-TTL deadline → 504 api_error
// chat_timeout, message naming the knob that fired (errors.go:261-264).
#[test]
fn row_chat_timeout_names_the_knob() {
    // Lease (silence window) fired: MIMO2API_LEASE_TTL_S named.
    let m = map_error(&PipelineError::ChatTimeout {
        budget_ms: 30_000,
        where_phase: "waiting for the upstream",
        lease: true,
    });
    assert_eq!(m.status, 504);
    assert_eq!(typ(&m), "api_error");
    assert_eq!(code(&m), "chat_timeout");
    assert!(
        m.body.error.message.contains("MIMO2API_LEASE_TTL_S"),
        "message must name the lease knob: {}",
        m.body.error.message
    );
    // Wall-clock budget fired: MIMO2API_REQUEST_TIMEOUT_MS named.
    let m = map_error(&PipelineError::ChatTimeout {
        budget_ms: 120_000,
        where_phase: "waiting for the upstream",
        lease: false,
    });
    assert_eq!(m.status, 504);
    assert!(m.body.error.message.contains("MIMO2API_REQUEST_TIMEOUT_MS"));
    // Budget rounds to whole seconds (Go time.Duration.Round).
    assert!(m.body.error.message.contains("120s"));
    // Empty phase falls back to the default wording.
    let m = map_error(&PipelineError::ChatTimeout {
        budget_ms: 1_500,
        where_phase: "",
        lease: true,
    });
    assert!(m.body.error.message.contains("waiting for its first byte"));
    assert!(m.body.error.message.contains("2s"));
}

// 3.22: UpstreamChatError → PipelineError conversion (retry ladder
// output feeds map_error directly).
#[test]
fn upstream_chat_error_conversion() {
    use crate::sse::InBandError::*;
    let cases: Vec<(UpstreamChatError, u16, &str)> = vec![
        (
            UpstreamChatError::Transport("conn refused".into()),
            502,
            "upstream_failure",
        ),
        (UpstreamChatError::IdleStall, 502, "upstream_failure"),
        // Silence-before-any-byte is a gateway deadline, not an
        // upstream failure (3.19 / ADR 0036).
        (
            UpstreamChatError::SilentUpstream {
                window: Duration::from_secs(30),
            },
            504,
            "chat_timeout",
        ),
        (UpstreamChatError::EmptyResponse, 502, "upstream_failure"),
        (
            UpstreamChatError::InBand(Busy("繁忙".into())),
            503,
            "upstream_busy",
        ),
        (
            UpstreamChatError::InBand(ModelRejected("模型名称错误".into())),
            400,
            "model_not_available_on_channel",
        ),
        (
            UpstreamChatError::InBand(ContentFilter {
                reason: "sensitive_query".into(),
                message: "m".into(),
            }),
            400,
            "content_filter",
        ),
    ];
    for (err, status, want_code) in cases {
        let p = PipelineError::from(&err);
        let m = map_error(&p);
        assert_eq!(m.status, status, "{err:?}");
        assert_eq!(code(&m), want_code, "{err:?}");
    }
}

#[test]
fn with_upstream_appends_verbatim() {
    assert_eq!(with_upstream("ours", "theirs"), "ours: theirs");
    assert_eq!(with_upstream("ours", "  "), "ours");
    assert_eq!(with_upstream("ours theirs", "theirs"), "ours theirs");
}
