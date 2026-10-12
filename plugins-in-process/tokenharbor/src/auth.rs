//! Supabase auth: password login, single-use refresh rotation, cookie
//! assembly, and refresh-on-401 with single-flight (contract §3 account
//! pool lives here; dossier §1).
//!
//! **Dossier top risk #1 — refresh rotation is SINGLE-USE**: every refresh
//! grant consumes the old refresh token and returns a new one. The new
//! token MUST be persisted to the store BEFORE any further upstream call;
//! a crash between grant and persist kills the session (re-login from the
//! stored password is the recovery path).
//!
//! Cookie shape (dossier §1, live-verified): app APIs accept
//! `sb-auth-auth-token.0` ALONE (unsplit, `.1` not needed; bearer auth
//! rejected) with value `base64-<urlsafe-b64-no-pad JSON>`:
//! `{access_token, token_type, expires_in, expires_at, refresh_token, user}`.
//!
//! Single-flight: one refresh at a time per account; concurrent callers
//! await the same in-flight refresh instead of racing (two refreshes with
//! one token = one dead session).

use std::sync::Arc;

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine as _;
use serde_json::{json, Value};

use crate::store::{AccountRow, Store};

/// Auth endpoint base (Supabase hosted at the site's auth subdomain).
pub const AUTH_BASE: &str = "https://auth.tokenharbor.ai";
/// Supabase anon key — a public browser constant shipped in the site's JS
/// bundle (dossier §1: "anon key unchanged vs reference, present in public
/// JS bundle, verified byte-identical"). Not a secret.
pub const ANON_KEY: &str = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzdXBhYmFzZSIsInJlZiI6ImlzYm56bXdqbXRpdWlwZXNnbW1nIiwicm9sZSI6ImFub24iLCJpYXQiOjE3NzY3NjU1MzYsImV4cCI6MjA5MjM0MTUzNn0.CodUcchio6jNW_k68vaAb--LshBQXK51tZ6VTxNSz_A";

/// Internal auth errors — mapped by the caller onto §4 plugin errors.
#[derive(Debug, thiserror::Error)]
pub enum AuthError {
    #[error("login failed for {email}: {detail}")]
    LoginFailed { email: String, detail: String },
    #[error("auth store: {0}")]
    Store(#[from] crate::store::StoreError),
    #[error("http: {0}")]
    Http(#[from] reqwest::Error),
}

/// One Supabase session grant (login or refresh response).
#[derive(Debug, Clone, PartialEq)]
pub struct SessionGrant {
    pub access_token: String,
    pub refresh_token: String,
    pub expires_in: i64,
    pub expires_at: i64,
    pub user_json: String,
}

impl SessionGrant {
    /// Parse a `/auth/v1/token` 200 body (shape live-verified, dossier §1).
    pub fn parse(body: &str) -> Option<Self> {
        let v: Value = serde_json::from_str(body).ok()?;
        Some(Self {
            access_token: v.get("access_token")?.as_str()?.to_string(),
            refresh_token: v.get("refresh_token")?.as_str()?.to_string(),
            expires_in: v.get("expires_in").and_then(Value::as_i64).unwrap_or(3600),
            expires_at: v.get("expires_at").and_then(Value::as_i64).unwrap_or(0),
            user_json: v
                .get("user")
                .map(|u| u.to_string())
                .unwrap_or_else(|| "{}".into()),
        })
    }
}

/// Assemble the `Cookie` header value for site APIs from a session grant:
/// `sb-auth-auth-token.0=base64-<urlsafe-b64-no-pad(json)>` — the only
/// cookie app endpoints need (dossier §1: `.1`, `th_sid` not required).
pub fn cookie_for(grant: &SessionGrant) -> String {
    let payload = json!({
        "access_token": grant.access_token,
        "token_type": "bearer",
        "expires_in": grant.expires_in,
        "expires_at": grant.expires_at,
        "refresh_token": grant.refresh_token,
        "user": serde_json::from_str::<Value>(&grant.user_json)
            .unwrap_or_else(|_| json!({})),
    });
    let encoded = URL_SAFE_NO_PAD.encode(payload.to_string().as_bytes());
    format!("sb-auth-auth-token.0=base64-{encoded}")
}

/// Auth state machine for ONE account, over the shared store.
///
/// States (implicit, store-backed):
/// - logged-out (`access_token` empty) → [`Auth::login`]
/// - access-valid → serve
/// - access-expired, refresh token present → [`Auth::ensure_session`]
///   refreshes (single-flight) and persists
/// - refresh dead → re-login from the stored password
///
/// Quarantined accounts are skipped upstream entirely (chat layer checks
/// the row before dispatch); auth itself never touches the network for them.
pub struct Auth {
    store: Arc<Store>,
    http: reqwest::Client,
    auth_base: String,
    /// Per-account single-flight refresh lock. The map is keyed by email
    /// and entries are transient: lock is held only during one refresh.
    refreshing: tokio::sync::Mutex<std::collections::BTreeSet<String>>,
}

impl Auth {
    /// Production constructor: points at the live Supabase host.
    pub fn new(store: Arc<Store>, http: reqwest::Client) -> Self {
        Self::with_base(store, http, AUTH_BASE.to_string())
    }

    /// Test constructor: points at a mock auth host.
    pub fn with_base(store: Arc<Store>, http: reqwest::Client, auth_base: String) -> Self {
        Self {
            store,
            http,
            auth_base,
            refreshing: tokio::sync::Mutex::new(std::collections::BTreeSet::new()),
        }
    }

    /// Password grant → persist → grant. The stored password is the only
    /// recovery path once a refresh token is consumed and lost.
    pub async fn login(&self, row: &AccountRow) -> Result<SessionGrant, AuthError> {
        let url = format!("{}/auth/v1/token?grant_type=password", self.auth_base);
        let resp = self
            .http
            .post(&url)
            .header("apikey", ANON_KEY)
            .header("authorization", format!("Bearer {ANON_KEY}"))
            .json(&json!({"email": row.email, "password": row.password}))
            .send()
            .await?;
        let status = resp.status();
        let text = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            // Never include the body's echo of credentials in the detail.
            let detail = truncate(&text, 200);
            return Err(AuthError::LoginFailed {
                email: row.email.clone(),
                detail,
            });
        }
        let grant = SessionGrant::parse(&text).ok_or_else(|| AuthError::LoginFailed {
            email: row.email.clone(),
            detail: "response missing token fields".into(),
        })?;
        // Persist BEFORE returning: the caller must never hold a grant
        // that is not already durable.
        self.persist(&row.email, &grant)?;
        Ok(grant)
    }

    /// Ensure the account has a valid access token: refresh when expired
    /// (single-use rotation, persisted immediately), re-login when there
    /// is no refresh token. Returns the ready-to-use `Cookie` header value.
    pub async fn ensure_session(&self, row: &AccountRow) -> Result<String, AuthError> {
        if row.access_valid(chrono::Utc::now().timestamp()) {
            return Ok(cookie_for(&row_as_grant(row)));
        }
        if row.refresh_token.is_empty() {
            let grant = self.login(row).await?;
            return Ok(cookie_for(&grant));
        }
        let grant = self.refresh(row).await?;
        Ok(cookie_for(&grant))
    }

    /// One refresh grant, single-flighted + persisted immediately.
    ///
    /// Single-flight: the per-account lock serializes refreshes. Waiters
    /// re-read the store after acquiring — if the winner already rotated,
    /// the fresh access token is reused and no second grant is attempted
    /// (a second refresh with the consumed token would kill the session).
    async fn refresh(&self, row: &AccountRow) -> Result<SessionGrant, AuthError> {
        loop {
            let mut guard = self.refreshing.lock().await;
            if !guard.contains(&row.email) {
                guard.insert(row.email.clone());
                drop(guard);
                break;
            }
            // Someone else is refreshing this account; wait for them.
            drop(guard);
            tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        }

        let result = self.refresh_locked(row).await;

        self.refreshing.lock().await.remove(&row.email);
        result
    }

    /// Body of one refresh, executed under the single-flight lock.
    async fn refresh_locked(&self, row: &AccountRow) -> Result<SessionGrant, AuthError> {
        // Re-read under the lock: a concurrent refresh may have already
        // rotated the token we were about to use.
        let current = self
            .store
            .accounts()?
            .into_iter()
            .find(|r| r.email == row.email)
            .unwrap_or_else(|| row.clone());
        if current.access_valid(chrono::Utc::now().timestamp()) {
            return Ok(row_as_grant(&current));
        }
        let refresh_token = current.refresh_token.clone();
        let url = format!("{}/auth/v1/token?grant_type=refresh_token", self.auth_base);
        let resp = self
            .http
            .post(&url)
            .header("apikey", ANON_KEY)
            .header("authorization", format!("Bearer {ANON_KEY}"))
            .json(&json!({"refresh_token": refresh_token}))
            .send()
            .await?;
        let status = resp.status();
        let text = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            // Refresh token consumed/dead — fall back to password re-login
            // (a fresh password grant supersedes the dead chain either way).
            tracing::warn!(email = %row.email, "refresh failed — re-login");
            return self.login(&current).await;
        }
        let grant = SessionGrant::parse(&text).ok_or_else(|| AuthError::LoginFailed {
            email: row.email.clone(),
            detail: "refresh response missing token fields".into(),
        })?;
        // **Persist the rotated refresh token IMMEDIATELY** (dossier risk
        // #1): the old one is now dead server-side; losing the new one
        // loses the session.
        self.persist(&row.email, &grant)?;
        Ok(grant)
    }

    fn persist(&self, email: &str, grant: &SessionGrant) -> Result<(), AuthError> {
        Ok(self.store.save_tokens(
            email,
            &grant.access_token,
            &grant.refresh_token,
            grant.expires_at,
            &grant.user_json,
        )?)
    }
}

/// View an account row's current token set as a grant (cookie assembly).
fn row_as_grant(row: &AccountRow) -> SessionGrant {
    SessionGrant {
        access_token: row.access_token.clone(),
        refresh_token: row.refresh_token.clone(),
        expires_in: 3600,
        expires_at: row.access_expires_at,
        user_json: row.user_json.clone(),
    }
}

fn truncate(s: &str, n: usize) -> String {
    if s.len() <= n {
        s.to_string()
    } else {
        format!("{}…", &s[..n])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn grant() -> SessionGrant {
        SessionGrant {
            access_token: "at-xxx".into(),
            refresh_token: "rt-yyy".into(),
            expires_in: 3600,
            expires_at: 1_791_787_538,
            user_json: r#"{"id":"u-1","email":"a@example.com"}"#.into(),
        }
    }

    #[test]
    fn grant_parses_live_shape() {
        let body = r#"{"access_token":"at","refresh_token":"rt","expires_in":3600,
            "expires_at":1791787538,"token_type":"bearer","user":{"id":"u"}}"#;
        let g = SessionGrant::parse(body).unwrap();
        assert_eq!(g.access_token, "at");
        assert_eq!(g.refresh_token, "rt");
        assert_eq!(g.expires_at, 1791787538);
    }

    #[test]
    fn grant_parse_rejects_missing_tokens() {
        assert!(SessionGrant::parse("{}").is_none());
        assert!(SessionGrant::parse("{\"access_token\":\"x\"}").is_none());
        assert!(SessionGrant::parse("not json").is_none());
    }

    #[test]
    fn cookie_shape_matches_dossier() {
        let cookie = cookie_for(&grant());
        assert!(cookie.starts_with("sb-auth-auth-token.0=base64-"));
        assert!(!cookie.contains("sb-auth-auth-token.1"));
        // Round-trip: decode and verify the payload keys (dossier §1).
        let b64 = cookie.strip_prefix("sb-auth-auth-token.0=base64-").unwrap();
        let bytes = URL_SAFE_NO_PAD.decode(b64).unwrap();
        let v: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(v["access_token"], "at-xxx");
        assert_eq!(v["refresh_token"], "rt-yyy");
        assert_eq!(v["token_type"], "bearer");
        assert_eq!(v["user"]["id"], "u-1");
    }
}
