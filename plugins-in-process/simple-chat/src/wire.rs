//! Wire semantics — the pure, offline-testable half of the Go `client.go`
//! port: the response envelope + BizError/HTTPStatusError taxonomy with
//! the IsAuthFailure/IsRetryable/BanKind classifiers and the small
//! envelope field extractors. Request serializers live in [`crate::body`],
//! the impersonation header table in [`crate::headers`]; only the reqwest
//! call sites in [`crate::client`] need a live upstream.

use std::time::{Duration, SystemTime};

use serde::Deserialize;

use crate::account::BanState;
use crate::timeutil;

/// Production upstream host (Go `DefaultBaseURL`). A constant, not
/// configuration; tests inject a local URL.
pub const DEFAULT_BASE_URL: &str = "https://chat.deepseek.com";

/// Whole-request cap for pre-flight JSON calls (login, session, PoW,
/// fetch_files, delete). Go `preflightTimeout`.
pub const PREFLIGHT_TIMEOUT: Duration = Duration::from_secs(60);

/// Completion-stream idle watchdog window: no bytes for this long → the
/// stream is cut and the read surfaces a typed
/// [`UpstreamError::StreamIdle`]. NOT a whole-request cap — a progressing
/// generation runs unbounded (Go `StreamIdleTimeout`).
pub const STREAM_IDLE_TIMEOUT: Duration = Duration::from_secs(180);

/// Spacing for the one pre-flight transport retry (Go
/// `preflightRetryBackoff`).
#[allow(dead_code)] // pool/manager consume next
pub const PREFLIGHT_RETRY_BACKOFF: Duration = Duration::from_millis(250);

/// One account's credentials and endpoint options (Go `Config`).
#[derive(Debug, Clone, Default)]
pub struct Config {
    /// Empty means [`DEFAULT_BASE_URL`]; tests inject a local URL.
    pub base_url: String,
    pub mobile: String,
    pub email: String,
    pub password: String,
    /// Per-account device identity on the login wire; empty falls back to
    /// the fixed default mint (tests, single-client use — Go
    /// `defaultDeviceID`). The pool always resolves per-account ids.
    pub device_id: String,
    /// `""` (android, default) or `"web"`.
    pub channel: String,
}

/// The outer response wrapper of every JSON endpoint (Go `envelope`).
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct Envelope {
    pub code: i64,
    #[serde(default)]
    pub msg: String,
    #[serde(default)]
    pub data: EnvelopeData,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
pub struct EnvelopeData {
    #[serde(default)]
    pub biz_code: i64,
    #[serde(default)]
    pub biz_msg: String,
    /// `null` when the endpoint returns no biz_data.
    #[serde(default)]
    pub biz_data: Option<serde_json::Value>,
}

/// Upstream failure taxonomy (Go `BizError`/`HTTPStatusError`/transport)
/// as one enum, keeping the `errors.As`-style classification pure.
#[derive(Debug, Clone, thiserror::Error, PartialEq)]
pub enum UpstreamError {
    /// Nonzero biz_code (or outer code, or non-200 with a JSON body).
    #[error("{}", biz_error_display(*http_status, *code, *biz_code, msg, biz_msg))]
    Biz {
        http_status: u16,
        code: i64,
        biz_code: i64,
        msg: String,
        biz_msg: String,
        /// Upstream mute_until for biz 5 (muted), when sent.
        mute_until: Option<SystemTime>,
    },
    /// Non-200 completion/preflight response with a non-JSON (or
    /// 5xx-envelope-free) body.
    #[error("upstream: completion http {status}: {snippet}")]
    HttpStatus { status: u16, snippet: String },
    /// The R2 watchdog: the completion stream produced no bytes for the
    /// idle window and was cut. Typed — NEVER a silent EOF.
    #[error("upstream: completion stream idle timeout (no bytes for {idle_secs}s)")]
    StreamIdle { idle_secs: u64 },
    /// Transport-level failure (connect, TLS, reset, timeout).
    #[error("upstream: transport: {0}")]
    Transport(String),
    /// Malformed upstream payload (bad JSON, missing field).
    #[error("upstream: {0}")]
    Malformed(String),
}

/// Go `BizError.Error()`: biz_msg tier, then msg tier, then http tier.
fn biz_error_display(
    http_status: u16,
    code: i64,
    biz_code: i64,
    msg: &str,
    biz_msg: &str,
) -> String {
    if !biz_msg.is_empty() {
        format!("upstream: biz_code {biz_code}: {biz_msg}")
    } else if !msg.is_empty() {
        format!("upstream: code {code}: {msg}")
    } else {
        format!("upstream: http {http_status}")
    }
}

impl UpstreamError {
    /// Account-level failure codes (recon §5: 10 banned, 5 muted, 11 risk
    /// device) — Go `BanKind`.
    pub fn ban_kind(&self) -> BanState {
        if let UpstreamError::Biz { biz_code, .. } = self {
            match *biz_code {
                10 => BanState::Banned,
                5 => BanState::Muted,
                11 => BanState::RiskDevice,
                _ => BanState::None,
            }
        } else {
            BanState::None
        }
    }

    /// Does this error warrant a token refresh? (recon §5: 401/403, outer
    /// codes 40001-40003, or auth-indicative biz messages) — Go
    /// `IsAuthFailure`. Ban states are never auth failures.
    pub fn is_auth_failure(&self) -> bool {
        let UpstreamError::Biz {
            http_status,
            code,
            biz_code,
            msg,
            biz_msg,
            ..
        } = self
        else {
            return false;
        };
        if self.ban_kind() != BanState::None {
            return false;
        }
        if *http_status == 401 || *http_status == 403 {
            return true;
        }
        if (40_001..=40_003).contains(code) || (40_001..=40_003).contains(biz_code) {
            return true;
        }
        let combined = format!("{} {}", msg, biz_msg).to_lowercase();
        [
            "token",
            "unauthorized",
            "expired",
            "not login",
            "login required",
            "invalid jwt",
        ]
        .iter()
        .any(|kw| combined.contains(kw))
    }

    /// Worth another attempt? Transport errors and HTTP ≥ 500 yes;
    /// upstream rejections (Biz) no — the upstream spoke, retrying is
    /// wasted load (Go `IsRetryable`: anything that is neither BizError
    /// nor HTTPStatusError is retryable).
    pub fn is_retryable(&self) -> bool {
        match self {
            UpstreamError::Biz { .. } => false,
            UpstreamError::HttpStatus { status, .. } => *status >= 500,
            UpstreamError::StreamIdle { .. } => true,
            UpstreamError::Transport(_) => true,
            UpstreamError::Malformed(_) => true,
        }
    }
}

/// Extract a mute_until timestamp from raw biz_data (Go `parseMuteUntil`):
/// probe sites `mute_until`, `chat.mute_until`, `user.chat.mute_until`;
/// unix seconds (number or string) or RFC3339; null/garbage → `None`.
pub fn parse_mute_until(raw: Option<&serde_json::Value>) -> Option<SystemTime> {
    let v = raw?;
    for path in [
        &["mute_until"] as &[&str],
        &["chat", "mute_until"],
        &["user", "chat", "mute_until"],
    ] {
        let mut cur = v;
        let mut found = true;
        for seg in path {
            match cur.get(*seg) {
                Some(next) => cur = next,
                None => {
                    found = false;
                    break;
                }
            }
        }
        if found {
            let text = match cur {
                serde_json::Value::Number(n) => n.to_string(),
                serde_json::Value::String(s) => s.clone(),
                _ => cur.to_string(),
            };
            if let Some(t) = timeutil::parse_mute_until_value(&text) {
                return Some(t);
            }
        }
    }
    None
}

/// Classify a JSON response into an envelope or an error (Go
/// `postJSON`'s post-send half, shared by every JSON endpoint). `body` is
/// the raw response bytes; `status` the HTTP status.
pub fn decode_envelope(status: u16, path: &str, body: &[u8]) -> Result<Envelope, UpstreamError> {
    let env: Envelope = match serde_json::from_slice(body) {
        Ok(env) => env,
        Err(e) => {
            // Bad JSON: 5xx → HTTPStatus (retryable); 401/403 → Biz (auth,
            // relogin path); else malformed.
            if status >= 500 {
                return Err(UpstreamError::HttpStatus {
                    status,
                    snippet: String::from_utf8_lossy(&body[..body.len().min(200)]).into(),
                });
            }
            if status == 401 || status == 403 {
                return Err(UpstreamError::Biz {
                    http_status: status,
                    code: 0,
                    biz_code: 0,
                    msg: String::new(),
                    biz_msg: String::new(),
                    mute_until: None,
                });
            }
            return Err(UpstreamError::Malformed(format!(
                "bad JSON from {path} (http {status}): {e}"
            )));
        }
    };
    if status != 200 {
        if env.code == 0 && env.data.biz_code == 0 && status >= 500 {
            return Err(UpstreamError::HttpStatus {
                status,
                snippet: env.msg.clone(),
            });
        }
        return Err(UpstreamError::Biz {
            http_status: status,
            code: env.code,
            biz_code: env.data.biz_code,
            msg: env.msg,
            biz_msg: env.data.biz_msg,
            mute_until: parse_mute_until(env.data.biz_data.as_ref()),
        });
    }
    Ok(env)
}

/// Validate an envelope for success (Go `checkEnv`).
pub fn check_env(env: &Envelope) -> Result<(), UpstreamError> {
    if env.code != 0 || env.data.biz_code != 0 {
        return Err(UpstreamError::Biz {
            http_status: 200,
            code: env.code,
            biz_code: env.data.biz_code,
            msg: env.msg.clone(),
            biz_msg: env.data.biz_msg.clone(),
            mute_until: parse_mute_until(env.data.biz_data.as_ref()),
        });
    }
    Ok(())
}

/// Extract the bearer token from a successful login envelope.
#[allow(dead_code)] // pool/manager consume next
pub fn login_token(env: &Envelope) -> Result<String, UpstreamError> {
    let token = env
        .data
        .biz_data
        .as_ref()
        .and_then(|d| d.get("user"))
        .and_then(|u| u.get("token"))
        .and_then(|t| t.as_str())
        .unwrap_or("");
    if token.is_empty() {
        return Err(UpstreamError::Malformed(
            "login response missing token".into(),
        ));
    }
    Ok(token.to_string())
}

/// Extract the session id from a create_session envelope (Go shape:
/// `chat_session.id` preferred, top-level `id` fallback).
#[allow(dead_code)] // pool/manager consume next
pub fn session_id(env: &Envelope) -> Result<String, UpstreamError> {
    let d = env.data.biz_data.as_ref();
    let chat_session = d
        .and_then(|d| d.get("chat_session"))
        .and_then(|c| c.get("id"));
    let top = d.and_then(|d| d.get("id"));
    let id = chat_session.or(top).and_then(|i| i.as_str()).unwrap_or("");
    if id.is_empty() {
        return Err(UpstreamError::Malformed(
            "create_session response missing id".into(),
        ));
    }
    Ok(id.to_string())
}

/// Classify a completion response by Content-Type: SSE (proceed) vs JSON
/// (error envelope at HTTP 200) — Go's post-2xx branch.
#[allow(dead_code)] // pool/manager consume next
pub fn completion_content_type_is_sse(content_type: &str) -> bool {
    content_type.contains("text/event-stream")
}

/// Session drawer entry (Go `SessionInfo`).
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct SessionInfo {
    pub id: String,
    #[serde(default)]
    pub pinned: bool,
    #[serde(default)]
    pub updated_at: f64,
}

/// One fetch_page biz_data page (Go `sessionPage`).
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct SessionPage {
    #[serde(default, rename = "chat_sessions")]
    pub sessions: Vec<SessionInfo>,
    #[serde(default)]
    pub has_more: bool,
}

#[allow(dead_code)] // pool/manager consume next
pub fn parse_session_page(env: &Envelope) -> Result<SessionPage, UpstreamError> {
    check_env(env)?;
    serde_json::from_value(env.data.biz_data.clone().unwrap_or_default())
        .map_err(|e| UpstreamError::Malformed(format!("bad fetch_page biz_data: {e}")))
}

/// Extract the uploaded file id from an upload envelope (`id`, fallback
/// `file_id`).
#[allow(dead_code)] // pool/manager consume next
pub fn upload_file_id(env: &Envelope) -> Result<String, UpstreamError> {
    let d = env.data.biz_data.as_ref();
    let id = d
        .and_then(|d| d.get("id"))
        .or_else(|| d.and_then(|d| d.get("file_id")))
        .and_then(|i| i.as_str())
        .unwrap_or("");
    if id.is_empty() {
        return Err(UpstreamError::Malformed(
            "upload response missing id".into(),
        ));
    }
    Ok(id.to_string())
}

/// Parse the fetch_files reply for one file's status (Go `fileStatus`).
#[allow(dead_code)] // pool/manager consume next
pub fn file_status(env: &Envelope, file_id: &str) -> Result<String, UpstreamError> {
    check_env(env)?;
    let files = env
        .data
        .biz_data
        .as_ref()
        .and_then(|d| d.get("files"))
        .and_then(|f| f.as_array());
    for f in files.into_iter().flatten() {
        if f.get("id").and_then(|i| i.as_str()) == Some(file_id) {
            return Ok(f
                .get("status")
                .and_then(|s| s.as_str())
                .unwrap_or_default()
                .to_string());
        }
    }
    Err(UpstreamError::Malformed(format!(
        "file {file_id} missing from fetch_files"
    )))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn biz(biz_code: i64, biz_msg: &str) -> UpstreamError {
        UpstreamError::Biz {
            http_status: 200,
            code: 0,
            biz_code,
            msg: String::new(),
            biz_msg: biz_msg.into(),
            mute_until: None,
        }
    }

    #[test]
    fn ban_kind_classification() {
        assert_eq!(biz(10, "banned").ban_kind(), BanState::Banned);
        assert_eq!(biz(5, "muted").ban_kind(), BanState::Muted);
        assert_eq!(biz(11, "risk").ban_kind(), BanState::RiskDevice);
        assert_eq!(biz(1, "other").ban_kind(), BanState::None);
        assert_eq!(
            UpstreamError::Transport("x".into()).ban_kind(),
            BanState::None
        );
    }

    #[test]
    fn auth_failure_classification() {
        let mk_status = |status: u16| UpstreamError::Biz {
            http_status: status,
            code: 0,
            biz_code: 0,
            msg: String::new(),
            biz_msg: String::new(),
            mute_until: None,
        };
        assert!(mk_status(401).is_auth_failure());
        assert!(mk_status(403).is_auth_failure());
        assert!(!mk_status(500).is_auth_failure());
        let mk_code = |code: i64, biz_code: i64| UpstreamError::Biz {
            http_status: 200,
            code,
            biz_code,
            msg: String::new(),
            biz_msg: String::new(),
            mute_until: None,
        };
        assert!(mk_code(40_001, 0).is_auth_failure());
        assert!(mk_code(0, 40_003).is_auth_failure());
        assert!(!mk_code(40_004, 0).is_auth_failure());
        // Keyword probing joins msg + biz_msg, lowercased, exact-phrase
        // substrings (Go's list: token, unauthorized, expired, not login,
        // login required, invalid jwt). "please login first" matches
        // nothing — parity quirk, not a bug to fix.
        assert!(biz(0, "token expired").is_auth_failure());
        assert!(biz(0, "LOGIN REQUIRED").is_auth_failure());
        assert!(!biz(0, "please login first").is_auth_failure());
        assert!(!biz(0, "insufficient credits").is_auth_failure());
        // Ban states are never auth failures (no relogin can fix them).
        assert!(!biz(5, "token expired").is_auth_failure());
        assert!(!biz(10, "unauthorized").is_auth_failure());
        // Non-biz errors are never auth failures.
        assert!(!UpstreamError::Transport("token".into()).is_auth_failure());
    }

    #[test]
    fn retryable_classification() {
        // Go: Biz never; HTTPStatus >= 500 yes; everything else yes.
        assert!(UpstreamError::Transport("reset".into()).is_retryable());
        assert!(UpstreamError::HttpStatus {
            status: 502,
            snippet: String::new()
        }
        .is_retryable());
        assert!(!UpstreamError::HttpStatus {
            status: 400,
            snippet: String::new()
        }
        .is_retryable());
        assert!(!biz(5, "muted").is_retryable());
        assert!(UpstreamError::StreamIdle { idle_secs: 180 }.is_retryable());
        assert!(UpstreamError::Malformed("bad".into()).is_retryable());
    }

    #[test]
    fn biz_error_display_tiers() {
        // Go BizError.Error(): biz_msg, then msg, then http.
        let e = biz(5, "muted upstream");
        assert_eq!(e.to_string(), "upstream: biz_code 5: muted upstream");
        let e = UpstreamError::Biz {
            http_status: 200,
            code: 500,
            biz_code: 0,
            msg: "outer boom".into(),
            biz_msg: String::new(),
            mute_until: None,
        };
        assert_eq!(e.to_string(), "upstream: code 500: outer boom");
        let e = UpstreamError::Biz {
            http_status: 401,
            code: 0,
            biz_code: 0,
            msg: String::new(),
            biz_msg: String::new(),
            mute_until: None,
        };
        assert_eq!(e.to_string(), "upstream: http 401");
    }

    #[test]
    fn mute_until_probe_sites() {
        let mk = |json: &str| serde_json::from_str::<serde_json::Value>(json).unwrap();
        // Top-level, unix seconds number.
        let v = mk(r#"{"mute_until": 1750000000}"#);
        let t = parse_mute_until(Some(&v)).unwrap();
        assert_eq!(timeutil::unix_secs(t), 1_750_000_000);
        // chat.mute_until, string unix seconds.
        let v = mk(r#"{"chat": {"mute_until": "1750000000"}}"#);
        assert_eq!(
            parse_mute_until(Some(&v)).map(timeutil::unix_secs),
            Some(1_750_000_000)
        );
        // user.chat.mute_until, RFC3339.
        let v = mk(r#"{"user": {"chat": {"mute_until": "2026-10-13T00:00:00Z"}}}"#);
        assert_eq!(
            parse_mute_until(Some(&v)).map(timeutil::unix_secs),
            timeutil::parse_rfc3339("2026-10-13T00:00:00Z").map(timeutil::unix_secs)
        );
        // First site wins when several are present.
        let v = mk(r#"{"mute_until": 1750000000, "chat": {"mute_until": 999}}"#);
        assert_eq!(
            parse_mute_until(Some(&v)).map(timeutil::unix_secs),
            Some(1_750_000_000)
        );
        // Null / garbage / absent.
        let v = mk(r#"{"mute_until": null}"#);
        assert!(parse_mute_until(Some(&v)).is_none());
        let v = mk(r#"{"chat": {"mute_until": "garbage"}}"#);
        assert!(parse_mute_until(Some(&v)).is_none());
        assert!(parse_mute_until(None).is_none());
    }

    #[test]
    fn envelope_decode_paths() {
        // 200 success.
        let env = decode_envelope(200, "/x", br#"{"code":0,"msg":""}"#).unwrap();
        assert!(check_env(&env).is_ok());
        // 200 outer failure → Biz with code.
        let env = decode_envelope(200, "/x", br#"{"code":500,"msg":"boom"}"#).unwrap();
        let err = check_env(&env).unwrap_err();
        assert!(matches!(err, UpstreamError::Biz { code: 500, .. }));
        // Non-200 with envelope → Biz carrying the http status.
        let err = decode_envelope(403, "/x", br#"{"code":40001,"msg":"token"}"#).unwrap_err();
        assert!(matches!(
            err,
            UpstreamError::Biz {
                http_status: 403,
                ..
            }
        ));
        assert!(err.is_auth_failure());
        // Non-200 5xx with clean envelope → HttpStatus (retryable).
        let err = decode_envelope(502, "/x", br#"{"code":0,"msg":""}"#).unwrap_err();
        assert!(matches!(err, UpstreamError::HttpStatus { status: 502, .. }));
        // Bad JSON + 5xx → HttpStatus with snippet.
        let err = decode_envelope(500, "/x", b"<html>gateway</html>").unwrap_err();
        assert!(matches!(err, UpstreamError::HttpStatus { status: 500, .. }));
        // Bad JSON + 401 → Biz (auth-retryable, relogin path).
        let err = decode_envelope(401, "/x", b"").unwrap_err();
        assert!(matches!(
            err,
            UpstreamError::Biz {
                http_status: 401,
                ..
            }
        ));
        assert!(err.is_auth_failure());
        // Bad JSON + other → Malformed.
        let err = decode_envelope(202, "/x", b"").unwrap_err();
        assert!(matches!(err, UpstreamError::Malformed(_)));
    }

    #[test]
    fn login_and_session_extraction() {
        let env: Envelope =
            serde_json::from_str(r#"{"code":0,"data":{"biz_data":{"user":{"token":"tok-1"}}}}"#)
                .unwrap();
        assert_eq!(login_token(&env).unwrap(), "tok-1");
        let missing: Envelope =
            serde_json::from_str(r#"{"code":0,"data":{"biz_data":{}}}"#).unwrap();
        assert!(login_token(&missing).is_err());
        // Session: chat_session.id preferred, top-level id fallback.
        let env: Envelope = serde_json::from_str(
            r#"{"code":0,"data":{"biz_data":{"id":"top","chat_session":{"id":"nested"}}}}"#,
        )
        .unwrap();
        assert_eq!(session_id(&env).unwrap(), "nested");
        let env: Envelope =
            serde_json::from_str(r#"{"code":0,"data":{"biz_data":{"id":"top"}}}"#).unwrap();
        assert_eq!(session_id(&env).unwrap(), "top");
        let env: Envelope = serde_json::from_str(r#"{"code":0,"data":{"biz_data":{}}}"#).unwrap();
        assert!(session_id(&env).is_err());
    }

    #[test]
    fn sse_content_type_discriminator() {
        assert!(completion_content_type_is_sse(
            "text/event-stream; charset=utf-8"
        ));
        assert!(completion_content_type_is_sse("text/event-stream"));
        assert!(!completion_content_type_is_sse("application/json"));
        assert!(!completion_content_type_is_sse(""));
    }

    #[test]
    fn session_page_and_file_parsing() {
        let env: Envelope = serde_json::from_str(
            r#"{"code":0,"data":{"biz_data":{"chat_sessions":[{"id":"a","pinned":true,"updated_at":1.5},{"id":"b","updated_at":2.0}],"has_more":false}}}"#,
        )
        .unwrap();
        let page = parse_session_page(&env).unwrap();
        assert_eq!(page.sessions.len(), 2);
        assert!(page.sessions[0].pinned);
        assert_eq!(page.sessions[1].id, "b");
        // Upload id extraction.
        let env: Envelope =
            serde_json::from_str(r#"{"code":0,"data":{"biz_data":{"id":"fid"}}}"#).unwrap();
        assert_eq!(upload_file_id(&env).unwrap(), "fid");
        let env: Envelope =
            serde_json::from_str(r#"{"code":0,"data":{"biz_data":{"file_id":"alt"}}}"#).unwrap();
        assert_eq!(upload_file_id(&env).unwrap(), "alt");
        // File status lookup.
        let env: Envelope = serde_json::from_str(
            r#"{"code":0,"data":{"biz_data":{"files":[{"id":"f1","status":"SUCCESS"}]}}}"#,
        )
        .unwrap();
        assert_eq!(file_status(&env, "f1").unwrap(), "SUCCESS");
        assert!(file_status(&env, "f2").is_err());
    }
}
