//! The §6 error mapping → contract §4 (M3 revised spelling), ported from
//! Go `server.go`'s `writePoolError`/`writeUpstreamError`/`setMuteRetryAfter`
//! and pinned to the dossier's table.
//!
//! Output shape: [`WireError`] — the `PluginError` the host relays (HTTP
//! status, §4 `type`/`code` strings, client-safe message) plus the
//! optional `Retry-After` seconds. Client-facing text is stable and
//! minimal (gap-analysis §4.6): raw upstream `biz_msg` internals never
//! pass through.

use std::time::{Duration, SystemTime};

use oxcost_sdk::PluginError;

use crate::account::BanState;
use crate::manager::local_mute_deadline;
use crate::pool::PoolError;
use crate::sse::StreamError;
use crate::wire::UpstreamError;

/// The mapped client-facing error: the SDK error plus the §4 Retry-After
/// side-channel (the host sets the HTTP header; the SDK `PluginError`
/// has no header field — M3 OQ #2, answered: Retry-After rides this
/// struct, the host projects it).
#[derive(Debug, Clone, PartialEq)]
pub struct WireError {
    pub error: PluginError,
    /// `Retry-After` seconds (429 family + risk rows); `None` = absent.
    pub retry_after: Option<u64>,
}

impl WireError {
    fn plain(status: u16, err_type: &str, code: &str, message: &str) -> Self {
        WireError {
            error: PluginError {
                status,
                err_type: err_type.into(),
                message: message.into(),
                param: None,
                code: Some(code.into()),
            },
            retry_after: None,
        }
    }

    /// 400 `invalid_request_error`/`bad_request`.
    pub fn bad_request(message: &str) -> Self {
        Self::plain(400, "invalid_request_error", "bad_request", message)
    }

    /// 400 `invalid_request_error`/`context_length_exceeded`.
    pub fn context_length_exceeded(len: usize, cap: usize) -> Self {
        Self::plain(
            400,
            "invalid_request_error",
            "context_length_exceeded",
            &format!("prompt is too long: {len} characters exceeds the {cap}-character limit"),
        )
    }

    /// 400 `invalid_request_error`/`image_fetch_failed`.
    pub fn image_fetch_failed() -> Self {
        Self::plain(
            400,
            "invalid_request_error",
            "image_fetch_failed",
            "image fetch failed",
        )
    }

    /// 400 `invalid_request_error`/`content_filter`.
    pub fn content_filter() -> Self {
        Self::plain(
            400,
            "invalid_request_error",
            "content_filter",
            "content filtered by upstream",
        )
    }

    /// 404 `invalid_request_error`/`model_not_found`.
    pub fn model_not_found(model: &str) -> Self {
        Self::plain(
            404,
            "invalid_request_error",
            "model_not_found",
            &format!("unknown model {model:?}: this proxy serves \"deepseek-flash\" only"),
        )
    }

    /// 502 `upstream_error`/`stream_error`.
    pub fn stream_cut() -> Self {
        Self::plain(
            502,
            "upstream_error",
            "stream_error",
            "upstream stream terminated early",
        )
    }

    /// 502 `upstream_error`/`upload_failed`.
    pub fn upload_failed() -> Self {
        Self::plain(
            502,
            "upstream_error",
            "upload_failed",
            "image upload failed",
        )
    }

    /// Map a pool admission failure (Go `writePoolError`).
    pub fn from_pool(err: &PoolError) -> Self {
        match err {
            PoolError::ParkPersistence => Self::plain(
                503,
                "upstream_error",
                "park_persistence_failed",
                "park persistence unavailable",
            ),
            PoolError::Parked { until } => {
                // The deadline is the pool's local eligibility time, not
                // QueueWait. Ceil protects subsecond remaining windows
                // from premature retries.
                let retry = until.map(|t| ceil_retry_after(&t)).unwrap_or_default();
                WireError {
                    error: PluginError {
                        status: 429,
                        err_type: "rate_limit_error".into(),
                        message: "all accounts temporarily parked, retry later".into(),
                        param: None,
                        code: Some("pool_parked".into()),
                    },
                    retry_after: Some(retry.max(1)),
                }
            }
            PoolError::NoEligible => Self::plain(
                503,
                "upstream_error",
                "no_eligible_account",
                "no eligible account with known recovery",
            ),
            PoolError::Busy {
                retry_after_secs, ..
            } => WireError {
                error: PluginError {
                    status: 429,
                    err_type: "rate_limit_error".into(),
                    message: "all accounts busy, retry later".into(),
                    param: None,
                    code: Some("pool_busy".into()),
                },
                retry_after: Some((*retry_after_secs).max(1)),
            },
            PoolError::NoAccounts => Self::plain(
                503,
                "upstream_error",
                "no_accounts",
                "no accounts available (all banned)",
            ),
            // NoAlternative is a ladder-internal condition (the facade
            // converts it into either the previous error or
            // no_eligible_account before it can surface); a bare one maps
            // to the Go pool_failure fallback.
            PoolError::NoAlternative | PoolError::Invalid(_) => Self::plain(
                502,
                "upstream_error",
                "pool_failure",
                "account pool unavailable",
            ),
        }
    }

    /// Map an upstream failure (Go `writeUpstreamError`).
    /// `lease_park_until` is the lease's pool-reconciled park deadline (Go
    /// `lease.ParkUntil()`): when the pool already reconciled this error
    /// with prior reports, IT is the Retry-After source, never the raw
    /// upstream timestamp.
    pub fn from_upstream(err: &UpstreamError, lease_park_until: Option<SystemTime>) -> Self {
        let mut out = Self::plain(
            502,
            "upstream_error",
            "upstream_failure",
            "upstream request failed",
        );
        match err.ban_kind() {
            BanState::Banned => {
                out.error.code = Some("account_banned".into());
                out.error.message = "account banned upstream".into();
            }
            BanState::Muted => {
                out.error.status = 429;
                out.error.err_type = "rate_limit_error".into();
                out.error.code = Some("account_muted".into());
                out.error.message = "account muted upstream".into();
                // Retry-After = the LOCAL park deadline: the pool's
                // reconciled value when present, else the same local
                // policy as parking (upstream ts +1h / 7d fallback).
                let until = lease_park_until.unwrap_or_else(|| {
                    let mute_until = if let UpstreamError::Biz { mute_until, .. } = err {
                        *mute_until
                    } else {
                        None
                    };
                    local_mute_deadline(
                        mute_until,
                        Duration::from_secs(7 * 24 * 3600),
                        SystemTime::now(),
                    )
                });
                out.retry_after = Some(ceil_retry_after(&until).max(1));
            }
            BanState::RiskDevice => {
                // Spec table: risk device → 503 upstream_unavailable (the
                // account is parked for a cooldown, but the gateway keeps
                // serving — the client should retry, not conclude its
                // request was bad).
                out.error.status = 503;
                out.error.code = Some("upstream_unavailable".into());
                out.error.message = "account flagged as risk device upstream".into();
            }
            BanState::None => {}
        }
        if err.is_auth_failure() {
            out.error.status = 401;
            out.error.err_type = "invalid_api_key".into();
            out.error.code = Some("invalid_api_key".into());
            out.error.message = "upstream authentication failed".into();
        }
        if let UpstreamError::HttpStatus { status, .. } = err {
            if *status >= 500 {
                out.error.message = "upstream unavailable".into();
            }
        }
        out
    }

    /// Map a mid-stream SSE error (Go `writeUpstreamError`'s StreamError
    /// arms): content_filter → 400; parallel limit → 429 with
    /// `Retry-After: 3`; anything else → 502 upstream_failure.
    pub fn from_stream(err: &StreamError) -> Self {
        if err.is_content_filter() {
            return Self::content_filter();
        }
        if err.is_parallel_limit() {
            return WireError {
                error: PluginError {
                    status: 429,
                    err_type: "rate_limit_error".into(),
                    message: "another generation is running on this account; retry shortly".into(),
                    param: None,
                    code: Some("parallel_chat_limit".into()),
                },
                retry_after: Some(3),
            };
        }
        Self::plain(
            502,
            "upstream_error",
            "upstream_failure",
            "upstream request failed",
        )
    }

    /// Map a transport cut mid-stream (both deliver paths): 502
    /// `stream_error` — the stream terminated early, cut-vs-EOF already
    /// classified by the watchdog (R2).
    pub fn stream_transport() -> Self {
        Self::stream_cut()
    }

    /// The 503 no-eligible-account retry answer (Go's ladder fallback
    /// when a same-account retry's account vanished).
    pub fn no_eligible_for_retry() -> Self {
        Self::plain(
            503,
            "upstream_error",
            "no_eligible_account",
            "no eligible account available for retry",
        )
    }
}

/// Retry-After seconds until `t`, rounded up, floor 0 (callers clamp to
/// ≥1 where the table demands it — Go `math.Ceil(time.Until(...))`).
fn ceil_retry_after(t: &SystemTime) -> u64 {
    match t.duration_since(SystemTime::now()) {
        Ok(d) => d.as_secs_f64().ceil() as u64,
        Err(_) => 0,
    }
}

#[cfg(test)]
mod tests;
