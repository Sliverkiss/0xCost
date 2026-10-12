//! One pool member: the persisted record it loads from and the runtime
//! state it carries (counters, cooldown cause, EWMA).

use std::collections::HashMap;
use std::sync::atomic::AtomicUsize;

use super::types::{FailureClass, State, DEFAULT_EGRESS};

/// One pool member, as loaded from the store (persisted runtime state
/// honored so cooldowns and bans survive restart, SDD §7.5, TDD POOL-013).
#[derive(Debug, Clone, Default)]
pub struct AccountRecord {
    pub id: String,
    /// Egress label; DEFAULT_EGRESS when empty (SDD §4.8).
    pub egress: String,
    /// Persisted state: available/cooldown/disabled.
    pub state: Option<State>,
    /// Cooldown deadline, ms since epoch (honored on load).
    pub cooldown_until_ms: Option<i64>,
    /// Persisted disabled reason.
    pub disabled_reason: String,
}

pub(super) struct Account {
    pub(super) id: String,
    pub(super) egress: String,
    pub(super) state: State,
    pub(super) cooldown_until_ms: i64,
    pub(super) disabled: bool,
    /// Dead passToken: never auto-recovers (RISK-003).
    pub(super) permanent: bool,
    pub(super) reason: String,
    /// Consecutive failures per class; counters never escalate each other
    /// (REQ-2/REQ-6).
    pub(super) failures: HashMap<FailureClass, u32>,
    /// Why the account is cooling, not just until when (REQ-5).
    pub(super) cooldown_class: Option<FailureClass>,
    pub(super) cooldown_status: u16,
    pub(super) cooldown_since_ms: i64,
    /// Last failure of any kind, cooldown or not (REQ-12).
    pub(super) last_error_class: Option<FailureClass>,
    pub(super) last_error_status: u16,
    pub(super) last_error_at_ms: i64,
    pub(super) inflight: AtomicUsize,
    /// EWMA latency in seconds, alpha 0.2 (SDD §4.4).
    pub(super) ewma_latency: f64,
}

impl Account {
    pub(super) fn new(rec: &AccountRecord) -> Self {
        Self {
            id: rec.id.clone(),
            egress: if rec.egress.is_empty() {
                DEFAULT_EGRESS.to_string()
            } else {
                rec.egress.clone()
            },
            state: State::Available,
            cooldown_until_ms: 0,
            disabled: false,
            permanent: false,
            reason: rec.disabled_reason.clone(),
            failures: HashMap::new(),
            cooldown_class: None,
            cooldown_status: 0,
            cooldown_since_ms: 0,
            last_error_class: None,
            last_error_status: 0,
            last_error_at_ms: 0,
            inflight: AtomicUsize::new(0),
            ewma_latency: 0.0,
        }
    }
}
