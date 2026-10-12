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
    /// Our own budget/lease-TTL deadline (Go requestTimeoutError, ADR
    /// 0019/0036): 504 with a message naming the knob that fired — the
    /// account was serving correctly, so no pool penalty and no retry.
    /// `budget_ms` is the deadline that actually bit (lease TTL when
    /// shorter or request budget unset), `where` names the phase.
    ChatTimeout {
        budget_ms: u64,
        where_phase: &'static str,
        /// True when the silence window (LEASE_TTL) was the bound that
        /// fired, not the wall-clock request budget — the message must
        /// name the right knob or an operator turns the wrong dial.
        lease: bool,
    },
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
            // Our own patience, not the account's (ADR 0036): the silence
            // window fired before any upstream byte — 504 naming the
            // knob, no pool penalty. The lease bound is the silence
            // window, so `lease: true` unless a request budget was
            // configured shorter than it.
            UpstreamChatError::SilentUpstream { window } => PipelineError::ChatTimeout {
                budget_ms: window.as_millis() as u64,
                where_phase: "waiting for its first byte",
                lease: true,
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

/// chat_timeout message (Go requestTimeoutError.Error, errors.go:237-245):
/// names the knob the operator has to turn. Naming only the bound that
/// actually fired matters: on the shipped default (REQUEST_TIMEOUT_MS=0)
/// the bound is the silence window, and pointing at a disabled knob
/// sends an operator to the wrong environment variable (ADR 0036). The
/// knob names are the Go deployment's (MIMO2API_*); the port's env
/// contract (config module) documents the mapping.
fn chat_timeout_message(budget_ms: u64, where_phase: &str, lease: bool) -> String {
    let knob = if lease {
        "MIMO2API_LEASE_TTL_S"
    } else {
        "MIMO2API_REQUEST_TIMEOUT_MS"
    };
    let where_phase = if where_phase.is_empty() {
        "waiting for its first byte"
    } else {
        where_phase
    };
    let budget_secs = budget_ms.div_ceil(1000); // Go time.Duration.Round(s)
    format!(
        "the upstream stayed silent for the gateway's {budget_secs}s {where_phase}; \
         the gateway gave up on the request (raise {knob} if the platform is simply slow). \
         Retry, or send fewer messages per request"
    )
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
        // 3.19: our own budget/lease-TTL deadline (Go requestTimeoutError,
        // errors.go:220-245): 504 api_error chat_timeout, message naming
        // the knob that fired — no account penalty, no retry.
        PipelineError::ChatTimeout {
            budget_ms,
            where_phase,
            lease,
        } => MappedError::new(
            504,
            "api_error",
            "chat_timeout",
            chat_timeout_message(*budget_ms, where_phase, *lease),
        ),
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
#[path = "errmap_tests.rs"]
mod tests;
