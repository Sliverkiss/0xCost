//! §6 error-table tests: every Go condition row pinned to its M3-revised
//! HTTP/type/code/Retry-After output, plus the Go writePoolError /
//! writeUpstreamError / setMuteRetryAfter semantics.

use std::time::{Duration, SystemTime};

use super::*;

fn later(secs: u64) -> SystemTime {
    SystemTime::now() + Duration::from_secs(secs)
}

fn biz(biz_code: i64, biz_msg: &str, mute_until: Option<SystemTime>) -> UpstreamError {
    UpstreamError::Biz {
        http_status: 200,
        code: 0,
        biz_code,
        msg: String::new(),
        biz_msg: biz_msg.into(),
        mute_until,
    }
}

fn assert_row(we: &WireError, status: u16, err_type: &str, code: &str) {
    assert_eq!(we.error.status, status, "{:?}", we.error);
    assert_eq!(we.error.err_type, err_type, "{:?}", we.error);
    assert_eq!(we.error.code.as_deref(), Some(code), "{:?}", we.error);
}

// ---- 400 family ----

#[test]
fn row_invalid_json() {
    let we = WireError::bad_request("invalid JSON body: x");
    assert_row(&we, 400, "invalid_request_error", "bad_request");
}

#[test]
fn row_prompt_over_cap() {
    let we = WireError::context_length_exceeded(2_000_001, 2_000_000);
    assert_row(&we, 400, "invalid_request_error", "context_length_exceeded");
    assert!(we.error.message.contains("2000001"));
    assert!(we.retry_after.is_none());
}

#[test]
fn row_image_fetch_failed() {
    assert_row(
        &WireError::image_fetch_failed(),
        400,
        "invalid_request_error",
        "image_fetch_failed",
    );
}

#[test]
fn row_content_filter_pre_commit() {
    let err = StreamError::ContentFilter("filtered".into());
    let we = WireError::from_stream(&err);
    assert_row(&we, 400, "invalid_request_error", "content_filter");
}

#[test]
fn row_unknown_model() {
    assert_row(
        &WireError::model_not_found("gpt-4"),
        404,
        "invalid_request_error",
        "model_not_found",
    );
}

// ---- 429 family ----

#[test]
fn row_pool_parked_known_deadline() {
    let we = WireError::from_pool(&PoolError::Parked {
        until: Some(later(3600)),
    });
    assert_row(&we, 429, "rate_limit_error", "pool_parked");
    let ra = we.retry_after.expect("Retry-After required");
    assert!((3599..=3600).contains(&ra), "ra={ra}");
}

#[test]
fn row_pool_parked_min_one_second() {
    // Subsecond remaining window: ceil + floor 1 (Go's clamps).
    let we = WireError::from_pool(&PoolError::Parked {
        until: Some(later(0)),
    });
    assert_eq!(we.retry_after, Some(1));
    let we = WireError::from_pool(&PoolError::Parked {
        until: Some(SystemTime::now()),
    });
    assert_eq!(we.retry_after, Some(1));
}

#[test]
fn row_pool_busy() {
    let we = WireError::from_pool(&PoolError::Busy {
        waited_secs: 30,
        retry_after_secs: 31,
    });
    assert_row(&we, 429, "rate_limit_error", "pool_busy");
    assert_eq!(we.retry_after, Some(31));
    // Floor 1.
    let we = WireError::from_pool(&PoolError::Busy {
        waited_secs: 30,
        retry_after_secs: 0,
    });
    assert_eq!(we.retry_after, Some(1));
}

#[test]
fn row_parallel_chat_limit() {
    let err = StreamError::Hint {
        kind: "error".into(),
        content: "limit".into(),
        clear_response: false,
        finish_reason: "parallel_chat_limit".into(),
    };
    let we = WireError::from_stream(&err);
    assert_row(&we, 429, "rate_limit_error", "parallel_chat_limit");
    assert_eq!(we.retry_after, Some(3));
}

#[test]
fn row_account_muted_local_deadline() {
    // Mute with a future upstream window and no pool reconciliation:
    // Retry-After = upstream + 1h grace (the LOCAL deadline: 600s window
    // + 3600s grace = ~4200), not the raw ts.
    let we = WireError::from_upstream(&biz(5, "muted", Some(later(600))), None);
    assert_row(&we, 429, "rate_limit_error", "account_muted");
    let ra = we.retry_after.expect("Retry-After required");
    assert!((4199..=4200).contains(&ra), "ra={ra} (want ~4200)");
}

#[test]
fn row_account_muted_pool_reconciled_wins() {
    // The pool already reconciled the mute with a shorter deadline: IT
    // is the Retry-After source (Go: lease.ParkUntil()).
    let we = WireError::from_upstream(&biz(5, "muted", Some(later(600))), Some(later(120)));
    let ra = we.retry_after.unwrap();
    assert!((119..=120).contains(&ra), "ra={ra} (want ~120)");
}

#[test]
fn row_account_muted_fallback_from_now() {
    // Windowless mute: 7d fallback from now.
    let we = WireError::from_upstream(&biz(5, "muted", None), None);
    let ra = we.retry_after.unwrap();
    assert!(ra >= 7 * 24 * 3600 - 2, "ra={ra}");
}

#[test]
fn row_account_muted_past_window_fallback() {
    // Upstream window already elapsed: fallback from now (7d), never a
    // sub-1s Retry-After.
    let we = WireError::from_upstream(
        &biz(
            5,
            "muted",
            Some(SystemTime::now() - Duration::from_secs(60)),
        ),
        None,
    );
    let ra = we.retry_after.unwrap();
    assert!(ra >= 7 * 24 * 3600 - 2, "ra={ra}");
}

// ---- 503 family ----

#[test]
fn row_risk_device() {
    let we = WireError::from_upstream(&biz(11, "risk", None), None);
    assert_row(&we, 503, "upstream_error", "upstream_unavailable");
    assert!(
        we.retry_after.is_none(),
        "risk is 503, not a Retry-After row"
    );
}

#[test]
fn row_no_accounts() {
    assert_row(
        &WireError::from_pool(&PoolError::NoAccounts),
        503,
        "upstream_error",
        "no_accounts",
    );
}

#[test]
fn row_no_eligible_no_retry_after() {
    let we = WireError::from_pool(&PoolError::NoEligible);
    assert_row(&we, 503, "upstream_error", "no_eligible_account");
    assert!(
        we.retry_after.is_none(),
        "never advertise a fabricated Retry-After"
    );
}

#[test]
fn row_no_eligible_for_retry() {
    let we = WireError::no_eligible_for_retry();
    assert_row(&we, 503, "upstream_error", "no_eligible_account");
}

#[test]
fn row_park_persistence() {
    let we = WireError::from_pool(&PoolError::ParkPersistence);
    assert_row(&we, 503, "upstream_error", "park_persistence_failed");
}

// ---- 502 family ----

#[test]
fn row_stream_cut() {
    assert_row(
        &WireError::stream_cut(),
        502,
        "upstream_error",
        "stream_error",
    );
    assert_row(
        &WireError::stream_transport(),
        502,
        "upstream_error",
        "stream_error",
    );
}

#[test]
fn row_upload_failed() {
    assert_row(
        &WireError::upload_failed(),
        502,
        "upstream_error",
        "upload_failed",
    );
}

#[test]
fn row_generic_upstream_failure() {
    // Biz non-ban, non-auth: the default row.
    let we = WireError::from_upstream(&biz(475, "some biz error", None), None);
    assert_row(&we, 502, "upstream_error", "upstream_failure");
    // The upstream biz_msg must NOT pass through to the client.
    assert!(
        !we.error.message.contains("some biz error"),
        "{:?}",
        we.error
    );
}

#[test]
fn row_banned() {
    let we = WireError::from_upstream(&biz(10, "banned", None), None);
    assert_row(&we, 502, "upstream_error", "account_banned");
    assert!(we.retry_after.is_none());
}

#[test]
fn row_http_5xx_says_unavailable() {
    let we = WireError::from_upstream(
        &UpstreamError::HttpStatus {
            status: 503,
            snippet: "boom".into(),
        },
        None,
    );
    assert_row(&we, 502, "upstream_error", "upstream_failure");
    assert_eq!(we.error.message, "upstream unavailable");
    // 4xx non-JSON: keeps the generic message.
    let we = WireError::from_upstream(
        &UpstreamError::HttpStatus {
            status: 404,
            snippet: "nf".into(),
        },
        None,
    );
    assert_eq!(we.error.message, "upstream request failed");
}

#[test]
fn row_stream_error_other() {
    let we = WireError::from_stream(&StreamError::Other("weird".into()));
    assert_row(&we, 502, "upstream_error", "upstream_failure");
    let we = WireError::from_stream(&StreamError::Hint {
        kind: "error".into(),
        content: "kaput".into(),
        clear_response: false,
        finish_reason: String::new(),
    });
    assert_row(&we, 502, "upstream_error", "upstream_failure");
}

// ---- 401 ----

#[test]
fn row_upstream_auth_failure() {
    let we = WireError::from_upstream(
        &UpstreamError::Biz {
            http_status: 401,
            code: 40_001,
            biz_code: 0,
            msg: "token expired".into(),
            biz_msg: String::new(),
            mute_until: None,
        },
        None,
    );
    assert_row(&we, 401, "invalid_api_key", "invalid_api_key");
    assert_eq!(we.error.message, "upstream authentication failed");
}

// ---- ladder-internal rows never surface raw ----

#[test]
fn row_no_alternative_maps_to_pool_failure() {
    // The facade converts NoAlternative before it surfaces; a bare one
    // falls back to pool_failure (Go's default arm).
    let we = WireError::from_pool(&PoolError::NoAlternative);
    assert_row(&we, 502, "upstream_error", "pool_failure");
}

#[test]
fn plugin_error_string_rendering() {
    // The host renders PluginError into the §4 body; spot-check the
    // Display contract keeps type/code/message.
    let we = WireError::from_pool(&PoolError::NoAccounts);
    let s = we.error.to_string();
    assert!(s.contains("503") && s.contains("no_accounts"), "{s}");
}
