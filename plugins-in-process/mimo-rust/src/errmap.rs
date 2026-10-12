//! Error mapping (Go `internal/api/errors.go` MapError table): translates
//! chat-pipeline errors into the OpenAI error body + HTTP status (SDD
//! §7.4). Table-driven and pure — the pipeline hands over a
//! [`PipelineError`], this module hands back a [`MappedError`], the host
//! adapter turns that into a `PluginError`.
//!
//! The one deliberate deviation (pinned in PORT_DESIGN §5.3): upstream
//! 5xx / transport / stall map to 502 `upstream_error` (not Go's
//! `api_error` type string) — the host contract reserves `api_error`
//! for host-internal failures, and `upstream_error` + preserved `code`
//! strings keep switch-on-code clients working.

use serde::Serialize;

use crate::sse::InBandError;
use crate::upstream::{UpstreamChatError, UpstreamError};

/// The OpenAI error body (SDD §7.4): `{"error":{message,type,param,code}}`.
/// `param` names the request field a parameter-level failure belongs to,
/// the way OpenAI's own errors do; null rather than invented for failures
/// that are not about a single field. `code` is always present (null when
/// there is no code) — a key OpenAI sends as null is sent as null, not
/// omitted (TDD OSTD-013).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ApiError {
    pub message: String,
    #[serde(rename = "type")]
    pub typ: String,
    pub param: Option<String>,
    pub code: Option<String>,
}

/// The full error body.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ErrorBody {
    pub error: ApiError,
}

/// The mapped outcome: the JSON body plus a Retry-After (seconds, header
/// only when > 0 — local recovery deadlines only, never account identity,
/// TDD ERRMAP-011).
#[derive(Debug, Clone, PartialEq)]
pub struct MappedError {
    /// HTTP status (the PluginError status the host sees).
    pub status: u16,
    pub body: ErrorBody,
    /// Retry-After in seconds; `None` → no header.
    pub retry_after: Option<u64>,
}

impl MappedError {
    fn new(status: u16, typ: &str, code: &str, message: impl Into<String>) -> Self {
        MappedError {
            status,
            body: ErrorBody {
                error: ApiError {
                    message: message.into(),
                    typ: typ.into(),
                    param: param_for_code(code),
                    code: code_of(code),
                },
            },
            retry_after: None,
        }
    }

    fn with_retry_after(mut self, seconds: u64) -> Self {
        if seconds > 0 {
            self.retry_after = Some(seconds);
        }
        self
    }
}

/// Maps an internal code onto the wire value: None — JSON null — when the
/// failure has no code of its own.
fn code_of(code: &str) -> Option<String> {
    if code.is_empty() {
        None
    } else {
        Some(code.to_string())
    }
}

/// The codes that ARE about one request field (Go errorParams). A code
/// missing here deliberately reports param:null instead of guessing a
/// field.
fn param_for_code(code: &str) -> Option<String> {
    let p = match code {
        "unsupported_reasoning_effort" => "reasoning_effort",
        "unsupported_search_context_size" => "web_search_options.search_context_size",
        "context_too_long" => "messages",
        _ => return None,
    };
    Some(p.to_string())
}

/// All request-level failures the pipeline raises before/around the
/// upstream call (Go chat.go sentinels; the image-family errors are
/// out of v1 scope — no image capability — but the variants stay so the
/// mapping table is complete).
#[derive(Debug, Clone, PartialEq)]
pub enum PipelineError {
    /// Pool empty or all disabled (ERRMAP-005).
    NoAccounts,
    /// All candidate accounts cooling (ERRMAP-003); retry-after from the
    /// nearest cooldown expiry.
    PoolParked { retry_after_seconds: u64 },
    /// All in-flight slots busy beyond the queue wait (ERRMAP-004).
    PoolBusy { retry_after_seconds: u64 },
    /// Unknown model → 404 naming available ids (API-013).
    ModelNotFound { requested: String },
    /// No sendable user message (ERRMAP-002/API-014).
    NoUserMessage,
    /// Replay cap exceeded (API-015).
    TooManyTurns { max: usize },
    /// Malformed JSON body (ERRMAP-002); code matches the audio surface's:
    /// one name for one meaning, never an empty code (ADR 0032).
    MalformedRequest,
    /// reasoning_effort outside the OpenAI set (OSTD-003).
    UnsupportedReasoningEffort,
    /// web_search_options.search_context_size outside low|medium|high
    /// (OSTD-005).
    UnsupportedSearchContextSize,
    /// Search switch outside disabled|auto|enabled — a client typo would
    /// otherwise be silently coerced upstream (M19, WS-002).
    UnsupportedSearchStatus,
    /// Image parts in a v1 request (no image capability; refused at
    /// decode, PARITY 3.x image family).
    ImageUrl { reason: String },
    /// sensitive_query / sensitive_title (ERRMAP-009): the platform's own
    /// event name and wording reach the client verbatim (ADR 0033).
    ContentFilter { reason: String, message: String },
    /// Channel char cap measured client-side (ERRMAP-010/API-016): the
    /// message names the measured size and the limit.
    QueryTooLong {
        limit: usize,
        actual: usize,
        model: String,
        /// How much of `actual` is preamble the client did not write
        /// (ADR 0015). Zero for a plain user message.
        overhead: usize,
    },
    /// The channel refuses this model id (web-surface-reverse §11.1):
    /// 400 — the request cannot succeed as written.
    UpstreamModelRejected { upstream_message: String },
    /// Transient platform throttle that outlived the retry (SDD §7.1):
    /// 503 + a short Retry-After, not a 502.
    UpstreamBusy { upstream_message: String },
    /// Upstream 401 surviving the refresh retry (ERRMAP-007/RENEW-006).
    AuthExpired,
    /// A non-200 upstream response (ERRMAP-006/008 and the too-long
    /// translation inside).
    Upstream(UpstreamError),
    /// Transport / stall / silence (SDD §7.4 row 9).
    UpstreamFailure { detail: String },
}

/// The upstream-busy Retry-After (Go busyRetryAfterSeconds, chat.go:2).
pub const BUSY_RETRY_AFTER_SECONDS: u64 = 2;

impl From<&UpstreamChatError> for PipelineError {
    fn from(e: &UpstreamChatError) -> Self {
        match e {
            UpstreamChatError::Upstream(ue) => PipelineError::Upstream((*ue).clone()),
            UpstreamChatError::AuthExpired(ue) => PipelineError::Upstream((**ue).clone()),
            UpstreamChatError::InBand(InBandError::Busy(m)) => PipelineError::UpstreamBusy {
                upstream_message: m.clone(),
            },
            UpstreamChatError::InBand(InBandError::ModelRejected(m)) => {
                PipelineError::UpstreamModelRejected {
                    upstream_message: m.clone(),
                }
            }
            UpstreamChatError::InBand(InBandError::ContentFilter { reason, message }) => {
                PipelineError::ContentFilter {
                    reason: reason.clone(),
                    message: message.clone(),
                }
            }
            UpstreamChatError::InBand(InBandError::Stream(m)) => {
                PipelineError::UpstreamFailure { detail: m.clone() }
            }
            // Empty response and stream-shape failures after a retry are
            // upstream failures (Go default arm).
            UpstreamChatError::EmptyResponse => PipelineError::UpstreamFailure {
                detail: "upstream returned an empty response".into(),
            },
            UpstreamChatError::Transport(m) => PipelineError::UpstreamFailure { detail: m.clone() },
            UpstreamChatError::IdleStall => PipelineError::UpstreamFailure {
                detail: "upstream stream stalled (no SSE event within idle timeout)".into(),
            },
            UpstreamChatError::SilentUpstream { window } => PipelineError::UpstreamFailure {
                detail: format!("upstream sent no bytes within the silence window ({window:?})"),
            },
        }
    }
}

/// upstreamTooLong: an upstream refusal about the request being too long,
/// across the wordings the platform uses — the chat channel answers
/// `Sorry, the text you sent is too long!`, the fastchat channel
/// `query is too long`, the audio surface `长度不能超过`.
fn upstream_too_long(s: &str) -> bool {
    if s.is_empty() {
        return false;
    }
    let low = s.to_lowercase();
    for marker in [
        "too long",
        "query is too long",
        "context_too_long",
        "too_long",
    ] {
        if low.contains(marker) {
            return true;
        }
    }
    s.contains("长度不能超过") || s.contains("内容过长") || s.contains("文本过长")
}

/// The client-facing too-long translation, with the measured ceilings
/// (ADR 0015).
pub fn upstream_too_long_message() -> String {
    "the upstream refused this request as too long; send fewer or shorter messages \
     (measured ceilings: 50000 characters on the ultraspeed channel, 102400 on other \
     models, counted on the composed query text — a system prompt or injected \
     tool-protocol prompt uses the same budget)"
        .to_string()
}

/// queryTooLong message (Go queryTooLongError.Error): names the number
/// that matters — "92311 chars over a 90000 limit on this model" is
/// actionable, "the query is too long" is not.
fn query_too_long_message(limit: usize, actual: usize, model: &str, overhead: usize) -> String {
    let suffix = if overhead > 0 {
        format!(
            " (of which {overhead} chars are the injected/system preamble, {} are the message)",
            actual - overhead
        )
    } else {
        String::new()
    };
    format!(
        "query is too long for this model channel: {actual} chars, limit {limit} (model {model:?});{suffix} send fewer or shorter messages"
    )
}

/// withUpstream (ADR 0033): appends the platform's own wording, verbatim,
/// to a sentence we wrote. No upstream text → the sentence stands alone,
/// never a dangling colon.
fn with_upstream(ours: &str, upstream: &str) -> String {
    let m = upstream.trim();
    if !m.is_empty() && !ours.contains(m) {
        return format!("{ours}: {m}");
    }
    ours.to_string()
}

/// MapError (SDD §7.4). Call only BEFORE the first byte reaches the
/// client; post-first-byte stream failures take the SSE error-frame path
/// instead (TDD API-021, ERRMAP-012).
pub fn map_error(err: &PipelineError) -> MappedError {
    match err {
        PipelineError::NoAccounts => {
            MappedError::new(503, "api_error", "no_accounts", "no accounts available")
        }
        PipelineError::PoolParked {
            retry_after_seconds,
        } => MappedError::new(
            429,
            "rate_limit_error",
            "pool_parked",
            "all accounts are cooling down; retry later",
        )
        .with_retry_after(*retry_after_seconds),
        PipelineError::PoolBusy {
            retry_after_seconds,
        } => MappedError::new(
            429,
            "rate_limit_error",
            "pool_busy",
            "all account slots busy; retry later",
        )
        .with_retry_after(*retry_after_seconds),
        PipelineError::ModelNotFound { requested } => MappedError::new(
            404,
            "invalid_request_error",
            "model_not_found",
            format!(
                "the model {requested:?} does not exist; available ids: \
                 mimo-v2.6-pro, mimo-v2.6-flash, mimo-v2.6-pro-ultraspeed-studio"
            ),
        ),
        PipelineError::NoUserMessage => MappedError::new(
            400,
            "invalid_request_error",
            "no_user_message",
            "no sendable user message in the request",
        ),
        PipelineError::TooManyTurns { max } => MappedError::new(
            400,
            "invalid_request_error",
            "too_many_turns",
            format!(
                "the replayed conversation has more than {max} turns; \
                 send fewer messages per request"
            ),
        ),
        PipelineError::MalformedRequest => MappedError::new(
            400,
            "invalid_request_error",
            "malformed_request_body",
            "malformed request body",
        ),
        PipelineError::UnsupportedReasoningEffort => MappedError::new(
            400,
            "invalid_request_error",
            "unsupported_reasoning_effort",
            "reasoning_effort must be one of: none, minimal, low, medium, high",
        ),
        PipelineError::UnsupportedSearchContextSize => MappedError::new(
            400,
            "invalid_request_error",
            "unsupported_search_context_size",
            "web_search_options.search_context_size must be one of: low, medium, high",
        ),
        PipelineError::UnsupportedSearchStatus => MappedError::new(
            400,
            "invalid_request_error",
            "unsupported_search_status",
            "web_search_status must be one of: disabled, auto, enabled",
        ),
        PipelineError::ImageUrl { reason } => MappedError::new(
            400,
            "invalid_request_error",
            "unsupported_image_url",
            reason.clone(),
        ),
        PipelineError::ContentFilter { reason, message } => MappedError::new(
            400,
            "invalid_request_error",
            "content_filter",
            format!(
                "content filtered by upstream ({reason}){sep}{message}",
                sep = if message.trim().is_empty() { "" } else { ": " }
            ),
        ),
        PipelineError::QueryTooLong {
            limit,
            actual,
            model,
            overhead,
        } => MappedError::new(
            400,
            "invalid_request_error",
            "context_too_long",
            query_too_long_message(*limit, *actual, model, *overhead),
        ),
        PipelineError::UpstreamModelRejected { upstream_message } => MappedError::new(
            400,
            "invalid_request_error",
            "model_not_available_on_channel",
            with_upstream(
                "the upstream channel does not serve this model id",
                upstream_message,
            ),
        ),
        PipelineError::UpstreamBusy { upstream_message } => MappedError::new(
            503,
            "api_error",
            "upstream_busy",
            with_upstream("upstream is busy; retry shortly", upstream_message),
        )
        .with_retry_after(BUSY_RETRY_AFTER_SECONDS),
        PipelineError::AuthExpired => MappedError::new(
            401,
            "invalid_request_error",
            "upstream_auth_expired",
            "upstream session expired and could not be refreshed",
        ),
        PipelineError::Upstream(ue) => match ue.status {
            429 => MappedError::new(
                429,
                "rate_limit_error",
                "upstream_rate_limited",
                "upstream rate limited the account",
            ),
            401 => MappedError::new(
                401,
                "invalid_request_error",
                "upstream_auth_expired",
                "upstream session expired and could not be refreshed",
            ),
            _ if upstream_too_long(&ue.body) => MappedError::new(
                400,
                "invalid_request_error",
                "context_too_long",
                upstream_too_long_message(),
            ),
            _ => MappedError::new(
                502,
                "upstream_error",
                "upstream_failure",
                format!("upstream error {}", ue.status),
            ),
        },
        PipelineError::UpstreamFailure { detail } => {
            if upstream_too_long(detail) {
                return MappedError::new(
                    400,
                    "invalid_request_error",
                    "context_too_long",
                    upstream_too_long_message(),
                );
            }
            // The pinned deviation (PORT_DESIGN §5.3): 502 upstream_error,
            // not Go's api_error type — host contract reserves api_error.
            MappedError::new(
                502,
                "upstream_error",
                "upstream_failure",
                format!("upstream failure: {detail}"),
            )
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
}
