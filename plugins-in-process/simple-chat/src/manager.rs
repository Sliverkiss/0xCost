//! Account manager — port of Go `client.go` `AccountManager`: token
//! lifecycle (lazy login, one relogin on auth failure), the monotonic
//! ban/mute state machine, and the one-shot app-launch startup sequence.
//!
//! Go guards the whole manager with one mutex held across network calls;
//! the port splits it: a tokio login gate serializes login/relogin
//! (concurrent `token()` callers wait, then find the cached token) and a
//! sync state lock covers the fast ban/token fields read under pool
//! locks. Observable semantics are identical.

use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use tokio::sync::Mutex as AsyncMutex;

use crate::account::BanState;
use crate::body::CompletionRequest;
use crate::client::{Client, WatchedStream};
use crate::wire::UpstreamError;

/// Local grace added to a future upstream mute window (Go
/// `localMuteDeadline`'s +1h). A constant, so `mark_ban` and the pool's
/// `note_error` recompute Go's mutable `BizError.localMutePark`
/// identically.
pub(crate) const MUTE_GRACE: Duration = Duration::from_secs(3600);

/// Fresh-login callback: receives every successful login/relogin's token
/// (the store's write-through moment — docs-spec-memory-first.md).
pub type LoginHook = Arc<dyn Fn(&str) + Send + Sync>;

/// Go `localMuteDeadline`: +1h grace when the upstream window is still
/// future, else the fallback from now.
pub(crate) fn local_mute_deadline(
    until: Option<SystemTime>,
    fallback: Duration,
    now: SystemTime,
) -> SystemTime {
    match until {
        Some(t) if t > now => t + MUTE_GRACE,
        _ => now + fallback,
    }
}

/// The manager's fast state — read under pool locks, written by request
/// paths. `pub(crate)` fields: the pool's `note_error` applies Go's exact
/// transition ladder under this lock.
#[derive(Debug)]
pub(crate) struct MgrState {
    pub(crate) token: String,
    pub(crate) ban: BanState,
    /// Set when the manager observed a permanent ban before the lease
    /// could write its durable park record.
    pub(crate) ban_park_pending: bool,
    pub(crate) ban_msg: String,
    pub(crate) park_until: Option<SystemTime>,
    /// Transient per-identity parallel-limit throttle.
    pub(crate) cooldown_until: Option<SystemTime>,
    /// Guards the one-shot app-launch startup sequence (apk-behavior.md
    /// §8 D2): fires on the empty→token transition, once per process.
    pub(crate) startup_fired: bool,
}

impl Default for MgrState {
    fn default() -> Self {
        MgrState {
            token: String::new(),
            ban: BanState::None,
            ban_park_pending: false,
            ban_msg: String::new(),
            park_until: None,
            cooldown_until: None,
            startup_fired: false,
        }
    }
}

/// Per-account token/ban lifecycle (Go `AccountManager`) — the seam the
/// pool leases out.
pub struct AccountManager {
    client: Arc<Client>,
    state: Mutex<MgrState>,
    /// Serializes login/relogin network work (Go holds `am.mu` across the
    /// `Login` call).
    login_gate: AsyncMutex<()>,
    mute_park_default: Duration,
    risk_cooldown: Duration,
    on_login: Mutex<Option<LoginHook>>,
}

impl AccountManager {
    pub fn new(client: Arc<Client>, mute_park_default: Duration, risk_cooldown: Duration) -> Self {
        AccountManager {
            client,
            state: Mutex::new(MgrState::default()),
            login_gate: AsyncMutex::new(()),
            mute_park_default,
            risk_cooldown,
            on_login: Mutex::new(None),
        }
    }

    /// Wire the login write-through hook (the pool builds the account
    /// before the shared runtime state exists, so the hook is attached
    /// post-construction).
    pub(crate) fn set_on_login(&self, hook: LoginHook) {
        *self.on_login.lock().unwrap() = Some(hook);
    }

    pub(crate) fn lock_state(&self) -> std::sync::MutexGuard<'_, MgrState> {
        self.state.lock().unwrap()
    }

    /// The current bearer token, logging in if none yet (Go `Token`).
    pub async fn token(&self) -> Result<String, UpstreamError> {
        let _gate = self.login_gate.lock().await;
        {
            let mut st = self.state.lock().unwrap();
            // Expired mute/risk park: the natural unpark (Go clears on
            // every Token call).
            if matches!(st.ban, BanState::Muted | BanState::RiskDevice) {
                if let Some(until) = st.park_until {
                    if SystemTime::now() >= until {
                        st.ban = BanState::None;
                        st.ban_msg.clear();
                        st.park_until = None;
                    }
                }
            }
            if st.ban != BanState::None {
                let msg = st.ban_msg.clone();
                return Err(UpstreamError::Malformed(format!(
                    "upstream: account unavailable: {msg}"
                )));
            }
            if !st.token.is_empty() {
                return Ok(st.token.clone());
            }
        }
        let tok = match self.client.login().await {
            Ok(t) => t,
            Err(e) => {
                self.mark_ban(&e);
                return Err(e);
            }
        };
        let fire_startup = {
            let mut st = self.state.lock().unwrap();
            st.token = tok.clone();
            let first = !st.startup_fired;
            st.startup_fired = true;
            first
        };
        if let Some(hook) = self.on_login.lock().unwrap().clone() {
            hook(&tok);
        }
        if fire_startup {
            self.fire_startup_sequence(&tok);
        }
        Ok(tok)
    }

    /// Mimic the app's launch traffic around first login (apk-behavior.md
    /// §8 D2): GET users/current, then GET chat_session/fetch_page.
    /// Best-effort: background task, never blocks or gates the request,
    /// no retries, failures only logged.
    fn fire_startup_sequence(&self, tok: &str) {
        let client = Arc::clone(&self.client);
        let tok = tok.to_string();
        tokio::spawn(async move {
            let seq = async {
                client.users_current(&tok).await?;
                client.fetch_session_page(&tok).await
            };
            match tokio::time::timeout(Duration::from_secs(10), seq).await {
                Ok(Ok(())) => {}
                _ => tracing::debug!("simple-chat: startup sequence failed (ignored)"),
            }
        });
    }

    /// Record a ban/mute state from an upstream error (Go `markBan`) —
    /// the monotonic state machine. Never shortens a known window; an
    /// existing unknown-recovery quarantine is only superseded by a
    /// permanent ban.
    pub fn mark_ban(&self, err: &UpstreamError) {
        let kind = err.ban_kind();
        let mut st = self.state.lock().unwrap();
        if st.ban == BanState::Banned {
            return;
        }
        if matches!(st.ban, BanState::Muted | BanState::RiskDevice)
            && st.park_until.is_none()
            && kind != BanState::Banned
        {
            return;
        }
        let now = SystemTime::now();
        let mute_until = if let UpstreamError::Biz { mute_until, .. } = err {
            *mute_until
        } else {
            None
        };
        match kind {
            BanState::Banned => {
                st.ban = BanState::Banned;
                st.ban_park_pending = true;
                st.park_until = None;
                st.ban_msg = format!("account banned: {err}");
            }
            BanState::Muted => {
                // Future upstream window: +1h local grace (Go localMutePark).
                if let Some(mu) = mute_until.filter(|t| *t > now) {
                    let park = mu + MUTE_GRACE;
                    if st.ban != BanState::None && st.park_until.is_some_and(|p| p > park) {
                        return;
                    }
                    st.ban = BanState::Muted;
                    st.park_until = Some(park);
                    st.ban_msg = format!("account muted: {err}");
                    return;
                }
                // A duplicate fallback report must not slide the local
                // deadline (a fallback is relative to observation time).
                if st.ban == BanState::Muted && st.park_until.is_some_and(|p| p > now) {
                    return;
                }
                let until = local_mute_deadline(mute_until, self.mute_park_default, now);
                if st.ban != BanState::None && st.park_until.is_some_and(|p| p > until) {
                    return;
                }
                st.ban = BanState::Muted;
                st.park_until = Some(until);
                st.ban_msg = format!("account muted: {err}");
            }
            BanState::RiskDevice => {
                let until = now + self.risk_cooldown;
                if st.ban != BanState::None && st.park_until.is_some_and(|p| p > until) {
                    return;
                }
                st.ban = BanState::RiskDevice;
                st.park_until = Some(until);
                st.ban_msg = format!("risk device detected: {err}");
            }
            BanState::None => {}
        }
    }

    /// Drop the cached token and log in again (Go `relogin`). No ban
    /// marking here — callers decide (Go parity: `Completion` marks the
    /// relogin error, `CreateSession` does not).
    pub async fn relogin(&self) -> Result<String, UpstreamError> {
        let _gate = self.login_gate.lock().await;
        let tok = self.client.login().await?;
        self.state.lock().unwrap().token = tok.clone();
        if let Some(hook) = self.on_login.lock().unwrap().clone() {
            hook(&tok);
        }
        Ok(tok)
    }

    /// Create a chat session, lazily logging in and retrying once with a
    /// fresh token on auth failure (Go `CreateSession`).
    pub async fn create_session(&self) -> Result<String, UpstreamError> {
        let tok = self.token().await?;
        let mut result = self.client.create_session(&tok).await;
        if let Err(e) = &result {
            if e.is_auth_failure() {
                let tok = match self.relogin().await {
                    Ok(t) => t,
                    Err(aerr) => return Err(aerr),
                };
                result = self.client.create_session(&tok).await;
            }
        }
        if let Err(e) = &result {
            self.mark_ban(e);
        }
        result
    }

    /// Run the completion stream with the account's token (PoW included),
    /// retrying once with a fresh token on an auth failure during the
    /// PoW pre-flight (Go `Completion`).
    pub async fn completion(
        &self,
        req: &CompletionRequest,
    ) -> Result<WatchedStream, UpstreamError> {
        let tok = self.token().await?;
        let mut result = self.client.completion(&tok, req).await;
        if let Err(e) = &result {
            if e.is_auth_failure() {
                let tok = match self.relogin().await {
                    Ok(t) => t,
                    Err(aerr) => {
                        self.mark_ban(&aerr);
                        return Err(aerr);
                    }
                };
                result = self.client.completion(&tok, req).await;
            }
        }
        if let Err(e) = &result {
            self.mark_ban(e);
        }
        result
    }

    /// Human-readable account state (Go `Status`).
    pub fn status(&self) -> &'static str {
        let st = self.state.lock().unwrap();
        match st.ban {
            BanState::Banned => "banned",
            BanState::Muted => "muted",
            BanState::RiskDevice => "risk device",
            BanState::None => {
                if st.token.is_empty() {
                    "no token"
                } else {
                    "ready"
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::wire::Config;

    /// A manager whose client points at a dead local port: any login
    /// attempt fails fast with a transport error, offline.
    fn mgr() -> AccountManager {
        let client = Arc::new(
            Client::new(Config {
                base_url: "http://127.0.0.1:1".into(),
                mobile: "13800001111".into(),
                password: "p".into(),
                ..Default::default()
            })
            .unwrap(),
        );
        AccountManager::new(
            client,
            Duration::from_secs(7 * 24 * 3600),
            Duration::from_secs(600),
        )
    }

    fn mute_err(mute_until: Option<SystemTime>) -> UpstreamError {
        UpstreamError::Biz {
            http_status: 200,
            code: 0,
            biz_code: 5,
            msg: String::new(),
            biz_msg: "muted upstream".into(),
            mute_until,
        }
    }

    fn banned_err() -> UpstreamError {
        UpstreamError::Biz {
            http_status: 200,
            code: 0,
            biz_code: 10,
            msg: String::new(),
            biz_msg: "banned upstream".into(),
            mute_until: None,
        }
    }

    fn risk_err() -> UpstreamError {
        UpstreamError::Biz {
            http_status: 200,
            code: 0,
            biz_code: 11,
            msg: String::new(),
            biz_msg: "risk".into(),
            mute_until: None,
        }
    }

    fn later(secs: u64) -> SystemTime {
        SystemTime::now() + Duration::from_secs(secs)
    }

    #[test]
    fn mark_ban_mute_future_window_gets_grace() {
        let m = mgr();
        m.mark_ban(&mute_err(Some(later(3600))));
        let st = m.lock_state();
        assert_eq!(st.ban, BanState::Muted);
        let park = st.park_until.unwrap();
        // +1h grace over the upstream window (Go localMutePark).
        assert!(park > later(3600) - Duration::from_secs(5));
        assert!(park <= later(3600) + MUTE_GRACE + Duration::from_secs(5));
    }

    #[test]
    fn mark_ban_mute_fallback_from_now() {
        let m = mgr();
        m.mark_ban(&mute_err(None));
        let st = m.lock_state();
        // No upstream window: 7d fallback from now.
        assert!(st.park_until.unwrap() > later(6 * 24 * 3600));
    }

    #[test]
    fn mark_ban_never_shortens() {
        let m = mgr();
        m.mark_ban(&mute_err(Some(later(5 * 24 * 3600))));
        let first = m.lock_state().park_until.unwrap();
        // A late in-flight error with a shorter window must not slide it.
        m.mark_ban(&mute_err(Some(later(600))));
        assert_eq!(m.lock_state().park_until, Some(first));
    }

    #[test]
    fn mark_ban_duplicate_fallback_does_not_slide() {
        let m = mgr();
        // First report with no window: fallback = now+7d.
        m.mark_ban(&mute_err(None));
        let first = m.lock_state().park_until.unwrap();
        // Second report, also windowless: recomputing would extend by 7d.
        m.mark_ban(&mute_err(None));
        assert_eq!(m.lock_state().park_until, Some(first));
    }

    #[test]
    fn mark_ban_banned_is_permanent() {
        let m = mgr();
        m.mark_ban(&banned_err());
        assert_eq!(m.lock_state().ban, BanState::Banned);
        assert_eq!(m.lock_state().park_until, None);
        assert!(m.lock_state().ban_park_pending);
        // Later mute reports cannot soften it.
        m.mark_ban(&mute_err(Some(later(3600))));
        assert_eq!(m.lock_state().ban, BanState::Banned);
    }

    #[test]
    fn mark_ban_unknown_recovery_quarantined() {
        let m = mgr();
        // Restored muted with no deadline: only a ban may supersede.
        {
            let mut st = m.lock_state();
            st.ban = BanState::Muted;
            st.park_until = None;
        }
        m.mark_ban(&mute_err(Some(later(3600))));
        {
            let st = m.lock_state();
            assert_eq!(st.ban, BanState::Muted);
            assert_eq!(st.park_until, None, "must not invent a deadline");
        }
        m.mark_ban(&banned_err());
        assert_eq!(m.lock_state().ban, BanState::Banned);
    }

    #[test]
    fn mark_ban_risk_cooldown() {
        let m = mgr();
        m.mark_ban(&risk_err());
        let st = m.lock_state();
        assert_eq!(st.ban, BanState::RiskDevice);
        assert!(st.park_until.unwrap() <= later(601));
    }

    #[test]
    fn mark_ban_ignores_non_ban_errors() {
        let m = mgr();
        m.mark_ban(&UpstreamError::Transport("reset".into()));
        assert_eq!(m.lock_state().ban, BanState::None);
        m.mark_ban(&UpstreamError::Malformed("bad".into()));
        assert_eq!(m.lock_state().ban, BanState::None);
    }

    #[tokio::test]
    async fn token_unavailable_when_banned() {
        let m = mgr();
        m.mark_ban(&banned_err());
        let err = m.token().await.expect_err("banned account");
        assert!(err.to_string().contains("account unavailable"));
    }

    #[tokio::test]
    async fn token_clears_expired_park_then_logs_in() {
        let m = mgr();
        // A mute park that has already elapsed: token() must clear it and
        // proceed to a (failing, offline) login instead of refusing.
        {
            let mut st = m.lock_state();
            st.ban = BanState::Muted;
            st.park_until = Some(SystemTime::now() - Duration::from_secs(1));
        }
        let err = m.token().await.expect_err("login against dead port");
        // Transport failure (not "account unavailable") — the park was
        // cleared and login attempted.
        assert!(matches!(err, UpstreamError::Transport(_)));
        let st = m.lock_state();
        assert_eq!(st.ban, BanState::None);
        assert!(st.park_until.is_none());
    }

    #[test]
    fn status_string() {
        let m = mgr();
        assert_eq!(m.status(), "no token");
        m.lock_state().token = "tok".into();
        assert_eq!(m.status(), "ready");
        m.mark_ban(&banned_err());
        assert_eq!(m.status(), "banned");
    }
}
