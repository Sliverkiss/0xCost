//! Public pool vocabulary: health states, failure classes and their
//! ladders, the pool error surface, injected clock and probe/renew hooks,
//! and options. Mirrors Go `internal/pool` type declarations (SDD §4).

use std::sync::Arc;
use std::time::Duration;

/// An account's health state (SDD §4.1).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum State {
    #[default]
    Available,
    Cooldown,
    Disabled,
}

impl State {
    pub const fn as_str(self) -> &'static str {
        match self {
            State::Available => "available",
            State::Cooldown => "cooldown",
            State::Disabled => "disabled",
        }
    }
}

/// Failure class: each class owns its own counter and its own ladder — a
/// transport failure must never advance the 5xx ladder (SDD §4.5.1).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum FailureClass {
    /// Connection error, timeout, SSE idle-stall, EOF mid-body — failures
    /// with no HTTP response (network- or egress-shaped, not the account's
    /// fault).
    Transport,
    /// The upstream answered 5xx (or a non-SSE 200 body).
    Upstream5xx,
    /// The upstream answered 429.
    Throttle429,
}

impl FailureClass {
    /// The ladder classes in a stable order (the admin view renders every
    /// class, present or not).
    pub const ALL: [FailureClass; 3] = [
        FailureClass::Transport,
        FailureClass::Upstream5xx,
        FailureClass::Throttle429,
    ];

    pub const fn as_str(self) -> &'static str {
        match self {
            FailureClass::Transport => "transport",
            FailureClass::Upstream5xx => "upstream-5xx",
            FailureClass::Throttle429 => "throttle-429",
        }
    }
}

/// Default egress label for unlabelled accounts (SDD §4.8).
pub const DEFAULT_EGRESS: &str = "direct";

/// Risk-control signal history cap: evidence is never time-erased (RISK-006),
/// only cap-bounded (keeps a long-running gateway's memory flat).
pub const RISK_EVENT_CAP: usize = 200;

/// One risk-control signal (451/461). The response body is never stored —
/// only a digest — and no credential appears here (INV-4).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RiskEvent {
    pub account_id: String,
    pub egress: String,
    pub model: Option<String>,
    pub channel: Option<String>,
    pub status: u16,
    pub body_digest: Option<String>,
    pub reason: Option<String>,
    /// Milliseconds since the Unix epoch.
    pub at_ms: i64,
}

/// Ladder (base + cap) for one failure class (SDD §4.5.1).
#[derive(Debug, Clone, Copy)]
pub struct Ladder {
    pub base: Duration,
    pub cap: Duration,
}

/// Pool error surface. `Parked`/`Busy` carry a retry-after for the API layer;
/// `NoAccounts` is terminal-503.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PoolError {
    /// Pool empty or all disabled → 503 no_accounts.
    NoAccounts,
    /// All candidate accounts cooling → 429 pool_parked, retry-after derived
    /// from the nearest cooldown expiry.
    Parked { retry_after_ms: u64 },
    /// All in-flight slots busy beyond the queue wait → 429 pool_busy.
    QueueTimeout { retry_after_ms: u64 },
    /// The caller's wait was cancelled.
    Cancelled,
}

impl PoolError {
    /// Retry-After seconds, clamped to at least 1s (the header form).
    pub fn retry_after_seconds(&self) -> Option<u64> {
        let ms = match self {
            PoolError::Parked { retry_after_ms } | PoolError::QueueTimeout { retry_after_ms } => {
                *retry_after_ms
            }
            _ => return None,
        };
        Some((ms / 1000).max(1))
    }
}

impl std::fmt::Display for PoolError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PoolError::NoAccounts => write!(
                f,
                "no accounts available: pool is empty or all accounts are disabled"
            ),
            PoolError::Parked { retry_after_ms } => write!(
                f,
                "all accounts are cooling down; nearest recovery in {}s",
                (*retry_after_ms / 1000).max(1)
            ),
            PoolError::QueueTimeout { retry_after_ms } => write!(
                f,
                "all account slots busy; retry after {}s",
                (*retry_after_ms / 1000).max(1)
            ),
            PoolError::Cancelled => write!(f, "acquire cancelled"),
        }
    }
}

/// Injected clock: cooldown math uses the fake clock, queue waiting uses
/// wall-clock instants (exactly as in Go: the deadline is `time.Now`, the
/// ladder math is `p.now()`).
pub type Clock = Arc<dyn Fn() -> i64 + Send + Sync>; // ms since epoch

/// Recovery probe outcome (SDD §4.5): healthy / auth-expired (one STS
/// renewal first) / sick (stay cooling).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProbeResult {
    Ok,
    AuthExpired,
    Sick,
}

/// Probe/renew hooks (the chat pipeline wires the real ones; tests inject
/// deterministic ones).
pub type ProbeFn = Arc<dyn Fn(&str) -> ProbeResult + Send + Sync>;
pub type RenewFn = Arc<dyn Fn(&str) -> bool + Send + Sync>; // true = renewed

/// Pool options; zero/None fields take SDD defaults.
#[derive(Clone)]
pub struct Options {
    /// Per-account in-flight capacity (default 2, SDD §4.4).
    pub max_inflight: usize,
    /// Acquire wait bound (default 15s).
    pub queue_wait: Duration,
    /// First cooldown on 429 (default 60s).
    pub cooldown_429: Duration,
    /// First cooldown on 5xx (default 30s).
    pub cooldown_5xx: Duration,
    /// First cooldown on transport failures (default 15s).
    pub cooldown_transport: Duration,
    /// Ladder cap for 429/5xx (default 10m).
    pub cooldown_cap: Duration,
    /// Transport ladder cap (default 5m) — escalates less far.
    pub cooldown_transport_cap: Duration,
    /// Window for cross-account risk correlation on one egress (default 5m).
    pub risk_window: Duration,
    /// Selection penalty for accounts with in-flight work (default 0.5).
    pub warm_penalty: f64,
    /// Injected clock (ms since epoch); default: system time.
    pub now: Option<Clock>,
    /// Slot TTL: silence window after which the reaper force-releases
    /// (default 120s). Zero disables the reaper.
    pub slot_ttl: Duration,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            max_inflight: 2,
            queue_wait: Duration::from_secs(15),
            cooldown_429: Duration::from_secs(60),
            cooldown_5xx: Duration::from_secs(30),
            cooldown_transport: Duration::from_secs(15),
            cooldown_cap: Duration::from_secs(600),
            cooldown_transport_cap: Duration::from_secs(300),
            risk_window: Duration::from_secs(300),
            warm_penalty: 0.5,
            now: None,
            slot_ttl: Duration::from_secs(120),
        }
    }
}
