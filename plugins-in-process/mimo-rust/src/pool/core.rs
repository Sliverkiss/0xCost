//! Guarded pool state, the inner shared handle, and the lease (one
//! in-flight slot). Release is idempotent; the reaper force-releases
//! silent leases through their expiry hook (ADR 0036).

use std::collections::HashMap;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use super::account::Account;
use super::pool_state;
use super::types::{Clock, FailureClass, Ladder, ProbeFn, RenewFn, RiskEvent};

/// Guarded pool state (one mutex, SDD §3: pool ops are microseconds).
/// In-flight counts are atomics so Release stays lock-free-ish, exactly like
/// Go's buffered-channel semaphore.
pub(super) struct PoolState {
    pub(super) accounts: Vec<Account>,
    pub(super) by_id: HashMap<String, usize>,
    /// Round-robin rotation within the selection band.
    pub(super) cursor: usize,
    pub(super) max_inflight: usize,
    pub(super) queue_wait: Duration,
    pub(super) ladders: HashMap<FailureClass, Ladder>,
    pub(super) risk_window: Duration,
    pub(super) risk_events: Vec<RiskEvent>,
    pub(super) warm_penalty: f64,
    pub(super) now: Clock,
    /// Live leases (reaper tracking). The pool and the request share one
    /// `Arc<Lease>` — Go tracks a `*Lease` pointer it does not own; this is
    /// the ownership-safe form of that.
    pub(super) live: Vec<LeaseHandle>,
    pub(super) expired_slots: u64,
}

/// Unshared, immutable-after-construction fields.
pub(super) struct PoolInner {
    pub(super) state: Mutex<PoolState>,
    pub(super) notify: tokio::sync::Notify,
    pub(super) slot_ttl: Duration,
    pub(super) probe: Option<ProbeFn>,
    pub(super) renew: Option<RenewFn>,
}

/// Shared lease handle (see [`Lease`]).
pub type LeaseHandle = Arc<Lease>;

/// A held in-flight slot on one account.
///
/// A lease is capacity for ONE request, never stickiness: it names the
/// account the request is talking to and occupies one of its in-flight
/// slots (ADR 0019). `touch` moves the expiry with every upstream byte
/// (ADR 0036) — a slot is reaped for silence, not for taking long.
pub struct Lease {
    pub(super) pool: Option<Arc<PoolInner>>,
    /// Account id (the lease names its account).
    pub id: String,
    /// Egress label the account's traffic must leave through.
    pub egress: String,
    pub(super) issued: Instant,
    /// This slot's silence window (zero = never reaped).
    pub(super) ttl: Duration,
    /// Wall clock (ns since epoch) of the last upstream byte; 0 = never.
    pub(super) last_activity_ns: AtomicI64,
    /// Idempotent-release guard.
    pub(super) released: std::sync::atomic::AtomicBool,
    /// On-expire cancel hook (abort the in-flight upstream call), armed by
    /// the chat pipeline right before it starts the upstream call.
    pub(super) on_expire: Mutex<Option<Box<dyn FnOnce() + Send + Sync>>>,
}

impl Lease {
    /// Records upstream activity on this lease, moving its expiry. The chat
    /// pipeline calls it on every upstream byte, not per parsed piece.
    pub fn touch(&self) {
        self.last_activity_ns.store(now_ns(), Ordering::Release);
    }

    /// When the slot was claimed (reporting: how long capacity was held).
    pub fn issued_at(&self) -> Instant {
        self.issued
    }

    /// How long this lease has been without upstream activity (a fresh
    /// lease counts from its claim).
    pub fn silent_for(&self, now: Instant) -> Duration {
        let last = self.last_activity_ns.load(Ordering::Acquire);
        if last == 0 {
            return now.duration_since(self.issued);
        }
        Duration::from_nanos((now_ns() - last).max(0) as u64)
    }

    /// This slot's silence window (zero = never reaped). The reaper reads
    /// it from the lease, so the lease is self-contained (Go: `l.ttl`).
    pub fn ttl(&self) -> Duration {
        self.ttl
    }

    /// Registers the cancel func the reaper calls when this lease outlives
    /// its TTL. Call right before starting the upstream call it should
    /// abort.
    pub fn arm_expiry(&self, cancel: Box<dyn FnOnce() + Send + Sync>) {
        *self.on_expire.lock().unwrap() = Some(cancel);
    }

    /// Frees the in-flight slot and wakes one Acquire waiter. Idempotent:
    /// every path may release without double-counting capacity.
    pub fn release(&self) {
        if self.released.swap(true, Ordering::AcqRel) {
            return;
        }
        *self.on_expire.lock().unwrap() = None;
        let Some(pool) = self.pool.as_ref() else {
            return;
        };
        let self_ptr = self as *const Lease as usize;
        let was_live;
        {
            let mut state = pool_state(pool);
            let before = state.live.len();
            // Identity-based removal: the lease's raw address is unique for
            // its lifetime (the Arc guarantees a stable address).
            state.live.retain(|l| Arc::as_ptr(l) as usize != self_ptr);
            was_live = state.live.len() < before;
            if let Some(&i) = state.by_id.get(&self.id) {
                state.accounts[i].inflight.fetch_sub(1, Ordering::AcqRel);
            }
        }
        if was_live {
            pool.notify.notify_waiters();
        }
    }
}

pub(super) fn now_ns() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
}

pub(super) fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}
