//! Account pool: states (available / cooldown / disabled), score-based
//! selection with round-robin fairness, the per-class cooldown ladder,
//! per-account in-flight slots, and the recovery probe gate. 1:1 port of
//! Go `internal/pool/pool.go` (SDD §4).
//!
//! Time is injected ([`Options::now`]) so the TDD vectors replay offline
//! with a fake clock; queue waiting is real-time bounded, exactly as in Go.
//!
//! - [`types`] — public pool vocabulary: states, failure classes, ladders,
//!   errors, options, probe/renew hooks
//! - [`account`] — one member's runtime record + loaded shape
//! - [`core`] — guarded pool state, the lease (slot) and the reaper seam
//! - this module — the `Pool` API over the above

mod account;
mod core;
mod types;

pub use account::AccountRecord;
pub use core::{Lease, LeaseHandle};
pub use types::{
    Clock, FailureClass, Ladder, Options, ProbeFn, ProbeResult, RenewFn, RiskEvent, State,
    DEFAULT_EGRESS,
};

pub use types::PoolError;

use std::collections::HashMap;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::Arc;
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};

use account::Account;
use core::{now_ms, now_ns, PoolInner, PoolState};
use types::RISK_EVENT_CAP;

#[cfg(test)]
mod tests;

pub struct Pool {
    inner: Arc<PoolInner>,
}

impl Clone for Pool {
    fn clone(&self) -> Self {
        Self {
            inner: Arc::clone(&self.inner),
        }
    }
}

impl Pool {
    /// Builds a pool from account records, applying defaults for unset
    /// options (Go `pool.New`). Persisted runtime state is honored so
    /// cooldowns and bans survive restart (SDD §7.5, TDD POOL-013).
    pub fn new(
        accts: &[AccountRecord],
        mut opts: Options,
        probe: Option<ProbeFn>,
        renew: Option<RenewFn>,
    ) -> Self {
        if opts.max_inflight == 0 {
            opts.max_inflight = 2;
        }
        if opts.queue_wait.is_zero() {
            opts.queue_wait = Duration::from_secs(15);
        }
        if opts.cooldown_429.is_zero() {
            opts.cooldown_429 = Duration::from_secs(60);
        }
        if opts.cooldown_5xx.is_zero() {
            opts.cooldown_5xx = Duration::from_secs(30);
        }
        if opts.cooldown_transport.is_zero() {
            opts.cooldown_transport = Duration::from_secs(15);
        }
        if opts.cooldown_cap.is_zero() {
            opts.cooldown_cap = Duration::from_secs(600);
        }
        if opts.cooldown_transport_cap.is_zero() {
            opts.cooldown_transport_cap = Duration::from_secs(300);
        }
        if opts.risk_window.is_zero() {
            opts.risk_window = Duration::from_secs(300);
        }
        if opts.warm_penalty <= 0.0 {
            opts.warm_penalty = 0.5;
        }
        let now: Clock = opts.now.take().unwrap_or_else(|| Arc::new(now_ms));
        let mut accounts = Vec::with_capacity(accts.len());
        let mut by_id = HashMap::new();
        for rec in accts {
            let mut a = Account::new(rec);
            match rec.state {
                Some(State::Disabled) => {
                    a.state = State::Disabled;
                    a.disabled = true;
                }
                Some(State::Cooldown) => {
                    a.state = State::Cooldown;
                    a.cooldown_until_ms = rec.cooldown_until_ms.unwrap_or(0);
                }
                _ => {}
            }
            by_id.insert(a.id.clone(), accounts.len());
            accounts.push(a);
        }
        let ladders = HashMap::from([
            (
                FailureClass::Transport,
                Ladder {
                    base: opts.cooldown_transport,
                    cap: opts.cooldown_transport_cap,
                },
            ),
            (
                FailureClass::Upstream5xx,
                Ladder {
                    base: opts.cooldown_5xx,
                    cap: opts.cooldown_cap,
                },
            ),
            (
                FailureClass::Throttle429,
                Ladder {
                    base: opts.cooldown_429,
                    cap: opts.cooldown_cap,
                },
            ),
        ]);
        Self {
            inner: Arc::new(PoolInner {
                state: Mutex::new(PoolState {
                    accounts,
                    by_id,
                    cursor: 0,
                    max_inflight: opts.max_inflight,
                    queue_wait: opts.queue_wait,
                    ladders,
                    risk_window: opts.risk_window,
                    risk_events: Vec::new(),
                    warm_penalty: opts.warm_penalty,
                    now,
                    live: Vec::new(),
                    expired_slots: 0,
                }),
                notify: tokio::sync::Notify::new(),
                slot_ttl: opts.slot_ttl,
                probe,
                renew,
            }),
        }
    }

    /// tryAcquireLocked: immediate selection. Busy is waitable; parked and
    /// no-accounts are terminal. exclude_ids are skipped entirely
    /// (retry-on-another-account, SDD §7.2).
    ///
    /// Score = (inflight/capacity)*100 + ewma_latency + warm_penalty;
    /// accounts within best*1.15+1 share the load evenly via round-robin
    /// (TDD POOL-001/015).
    pub fn try_acquire(&self, exclude_ids: &[&str]) -> Result<LeaseHandle, PoolError> {
        let inner = &self.inner;
        let mut state = pool_state(inner);
        let now = (state.now)();
        let excl: std::collections::HashSet<&str> = exclude_ids.iter().copied().collect();

        struct Scored {
            idx: usize,
            score: f64,
        }
        let mut candidates: Vec<Scored> = Vec::new();
        let mut nearest_ms: Option<i64> = None;
        let mut available_exists = false;
        let mut enabled = 0;
        for (i, a) in state.accounts.iter().enumerate() {
            if excl.contains(a.id.as_str()) || a.disabled {
                continue;
            }
            enabled += 1;
            if a.state != State::Available {
                // Cooldown expiry alone is NOT selectable: the probe gate
                // applies first (SDD §4.5, TDD POOL-006).
                nearest_ms = Some(match nearest_ms {
                    None => a.cooldown_until_ms,
                    Some(n) => a.cooldown_until_ms.min(n),
                });
                continue;
            }
            available_exists = true;
            let inflight = a.inflight.load(Ordering::Acquire);
            if inflight >= state.max_inflight {
                continue; // full: contributes to busy, not to candidates
            }
            let mut score = inflight as f64 / state.max_inflight as f64 * 100.0 + a.ewma_latency;
            if inflight > 0 {
                score += state.warm_penalty;
            }
            candidates.push(Scored { idx: i, score });
        }
        if enabled == 0 {
            return Err(PoolError::NoAccounts); // POOL-011
        }
        if candidates.is_empty() {
            if available_exists {
                return Err(PoolError::QueueTimeout { retry_after_ms: 0 });
            }
            let base_429 = state.ladders[&FailureClass::Throttle429].base.as_millis() as i64;
            let until = nearest_ms.unwrap_or(now + base_429);
            return Err(PoolError::Parked {
                retry_after_ms: (until - now).max(0) as u64,
            }); // POOL-008
        }

        // Band selection: best*1.15+1, stable order, round-robin inside the
        // band.
        candidates.sort_by(|x, y| {
            x.score
                .partial_cmp(&y.score)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        let threshold = candidates[0].score * 1.15 + 1.0;
        let end = candidates
            .iter()
            .position(|c| c.score > threshold)
            .unwrap_or(candidates.len());
        let band: Vec<usize> = candidates[..end].iter().map(|c| c.idx).collect();
        state.cursor += 1;
        let pick = state.cursor % band.len();

        // Claim under the pool lock so selection and slot-taking are atomic.
        let pick_idx = band[pick];
        let prev = state.accounts[pick_idx]
            .inflight
            .fetch_add(1, Ordering::AcqRel);
        if prev >= state.max_inflight {
            state.accounts[pick_idx]
                .inflight
                .fetch_sub(1, Ordering::AcqRel);
            return Err(PoolError::QueueTimeout { retry_after_ms: 0 });
        }
        Ok(register_lease(inner, &mut state, pick_idx))
    }

    /// Acquire with queue waiting: waits up to queue_wait for a free slot,
    /// then fails closed with QueueTimeout → 429 pool_busy (Go `Acquire`;
    /// TDD POOL-009/010).
    pub async fn acquire(&self, exclude_ids: &[&str]) -> Result<LeaseHandle, PoolError> {
        let queue_wait = pool_state(&self.inner).queue_wait;
        let deadline = Instant::now() + queue_wait;
        loop {
            match self.try_acquire(exclude_ids) {
                Ok(l) => return Ok(l),
                Err(PoolError::QueueTimeout { retry_after_ms: 0 }) => {}
                Err(e) => return Err(e),
            }
            let wait = deadline.saturating_duration_since(Instant::now());
            if wait.is_zero() {
                return Err(PoolError::QueueTimeout {
                    retry_after_ms: queue_wait.as_millis() as u64,
                });
            }
            let notified = self.inner.notify.notified();
            tokio::select! {
                _ = notified => {}
                _ = tokio::time::sleep(wait) => {}
            }
        }
    }

    /// Records first-byte latency into the EWMA selection term (SDD §4.4).
    pub fn record_first_byte(&self, account_id: &str, elapsed: Duration) {
        let mut state = pool_state(&self.inner);
        if let Some(&i) = state.by_id.get(account_id) {
            let a = &mut state.accounts[i];
            a.ewma_latency = 0.8 * a.ewma_latency + 0.2 * elapsed.as_secs_f64();
        }
    }

    /// ReportFailure: applies the ladder of that class and records the
    /// cause (POOLATTR-*). Unknown classes cool like a 5xx — fail closed
    /// rather than inventing a shorter penalty. The upstream's own
    /// Retry-After is never read (POOLATTR-011): ladders are the single
    /// source of cooldown timing.
    pub fn report_failure(&self, account_id: &str, class: FailureClass, status: u16) {
        let mut state = pool_state(&self.inner);
        let now = (state.now)();
        let Some(&i) = state.by_id.get(account_id) else {
            return;
        };
        if state.accounts[i].disabled {
            return;
        }
        let ladder = match state.ladders.get(&class) {
            Some(l) => *l,
            // Unknown class → 5xx ladder (fail closed).
            None => state.ladders[&FailureClass::Upstream5xx],
        };
        let count = state.accounts[i].failures.entry(class).or_insert(0);
        *count += 1;
        let mut d = ladder.base;
        for _ in 1..*count {
            d *= 2;
            if d >= ladder.cap || d.is_zero() {
                d = ladder.cap;
                break;
            }
        }
        let a = &mut state.accounts[i];
        a.state = State::Cooldown;
        a.cooldown_until_ms = now + d.as_millis() as i64;
        a.cooldown_class = Some(class);
        a.cooldown_status = status;
        a.cooldown_since_ms = now;
        a.last_error_class = Some(class);
        a.last_error_status = status;
        a.last_error_at_ms = now;
    }

    /// Transport-class failure (connection error, timeout, SSE idle-stall,
    /// EOF mid-body) on its own ladder (POOLATTR-001/003/004).
    pub fn report_transport(&self, account_id: &str) {
        self.report_failure(account_id, FailureClass::Transport, 0);
    }

    /// 429 → the rate-limit ladder: 60s doubling to the 10m cap (POOL-002/003).
    pub fn report_429(&self, account_id: &str) {
        self.report_failure(account_id, FailureClass::Throttle429, 429);
    }

    /// 5xx → the upstream-sick ladder: 30s doubling to the cap (POOL-005).
    pub fn report_5xx(&self, account_id: &str) {
        self.report_failure(account_id, FailureClass::Upstream5xx, 0);
    }

    /// ReportSuccess resets the cooldown ladder (TDD POOL-004; POOLATTR-008:
    /// clears every class, not just the one that last failed).
    pub fn report_success(&self, account_id: &str) {
        let mut state = pool_state(&self.inner);
        if let Some(&i) = state.by_id.get(account_id) {
            state.accounts[i].failures.clear();
        }
    }

    /// ResetFailures clears counters without touching the cooldown — the
    /// renewal path (POOLATTR-006/009: cooldowns are time-owned; releasing
    /// one here would hand an unhealthy account straight back).
    pub fn reset_failures(&self, account_id: &str) {
        let mut state = pool_state(&self.inner);
        if let Some(&i) = state.by_id.get(account_id) {
            state.accounts[i].failures.clear();
        }
    }

    /// Per-class failure counters, every known class present (stable shape
    /// for the admin view).
    pub fn failures_of(&self, account_id: &str) -> HashMap<FailureClass, u32> {
        let state = pool_state(&self.inner);
        let mut out: HashMap<FailureClass, u32> =
            FailureClass::ALL.iter().map(|&c| (c, 0)).collect();
        if let Some(&i) = state.by_id.get(account_id) {
            for (&c, &n) in &state.accounts[i].failures {
                out.insert(c, n);
            }
        }
        out
    }

    /// Why an account is cooling: (class, status, since_ms); None when it
    /// never cooled.
    pub fn cooldown_info_of(&self, account_id: &str) -> Option<(FailureClass, u16, i64)> {
        let state = pool_state(&self.inner);
        let &i = state.by_id.get(account_id)?;
        let a = &state.accounts[i];
        Some((a.cooldown_class?, a.cooldown_status, a.cooldown_since_ms))
    }

    /// ReportRiskControl: records the signal and disables the account —
    /// fail closed, because we cannot yet tell an account-scoped block from
    /// an egress- or model-scoped one (SDD §4.5.2).
    ///
    /// Deliberately NOT permanent (RISK-002): permanence is a claim about
    /// the upstream's intent and the form of 451/461 is unverified here;
    /// the account is recoverable through `enable`. Returns distinct
    /// same-egress accounts with a signal inside the correlation window
    /// (RISK-004).
    pub fn report_risk_control(&self, account_id: &str, reason: &str, ev: RiskEvent) -> usize {
        let mut state = pool_state(&self.inner);
        let now = (state.now)();
        let egress = match state.by_id.get(account_id) {
            Some(&i) => {
                let a = &mut state.accounts[i];
                a.disabled = true;
                a.permanent = false;
                a.reason = reason.to_string();
                a.state = State::Disabled;
                a.cooldown_until_ms = 0;
                a.last_error_class = None; // risk-control is not a ladder class
                a.last_error_status = ev.status;
                a.last_error_at_ms = now;
                a.egress.clone()
            }
            // Unknown account: still recorded (evidence about the upstream's
            // behaviour, RISK-007); default egress.
            None => DEFAULT_EGRESS.to_string(),
        };
        let mut event = ev;
        event.account_id = account_id.to_string();
        event.egress = egress.clone();
        event.reason = Some(reason.to_string());
        event.at_ms = now;
        state.risk_events.push(event);
        if state.risk_events.len() > RISK_EVENT_CAP {
            let drop_n = state.risk_events.len() - RISK_EVENT_CAP;
            state.risk_events.drain(..drop_n);
        }
        same_egress_count(&state, &egress, now)
    }

    /// Recorded risk-control history, oldest first (evidence is not erased,
    /// RISK-006).
    pub fn risk_events(&self) -> Vec<RiskEvent> {
        pool_state(&self.inner).risk_events.clone()
    }

    /// Disables an account with a reason. Permanent (461/dead passToken) is
    /// recorded as such (TDD POOL-012); permanent accounts are never probed.
    pub fn report_disabled(&self, account_id: &str, reason: &str, permanent: bool) {
        {
            let mut state = pool_state(&self.inner);
            if let Some(&i) = state.by_id.get(account_id) {
                let a = &mut state.accounts[i];
                a.state = State::Disabled;
                a.disabled = true;
                a.permanent = permanent;
                a.reason = reason.to_string();
            }
        }
        // Waiters must re-evaluate (may now park).
        self.inner.notify.notify_waiters();
    }

    /// Clears disabled/cooldown state and makes the account selectable
    /// again (operator action). Idempotent (RISK-006: evidence stays).
    pub fn enable(&self, account_id: &str) {
        {
            let mut state = pool_state(&self.inner);
            if let Some(&i) = state.by_id.get(account_id) {
                let a = &mut state.accounts[i];
                a.disabled = false;
                a.permanent = false;
                a.reason = String::new();
                a.failures.clear();
                a.cooldown_class = None;
                a.cooldown_status = 0;
                a.cooldown_since_ms = 0;
                a.cooldown_until_ms = 0;
                a.state = State::Available;
            }
        }
        // Waiters re-evaluate (may unpark).
        self.inner.notify.notify_waiters();
    }

    /// ProbeSweep: recovery probes over cooldown-expired accounts (TDD
    /// POOL-006/007): OK → available; auth-expired → exactly one renewal
    /// (success → available, failure → disabled); sick → stay cooling,
    /// re-armed at the 5xx base. Disabled accounts are never probed
    /// (POOL-012). Returns the recovered count.
    pub fn probe_sweep(&self) -> usize {
        let inner = &self.inner;
        let due: Vec<usize> = {
            let state = pool_state(inner);
            let now = (state.now)();
            state
                .accounts
                .iter()
                .enumerate()
                .filter(|(_, a)| {
                    !a.disabled && a.state == State::Cooldown && now > a.cooldown_until_ms
                })
                .map(|(i, _)| i)
                .collect()
        };
        let mut recovered = 0;
        for i in due {
            let id = pool_state(inner).accounts[i].id.clone();
            let mut result = ProbeResult::Ok;
            if let Some(probe) = inner.probe.as_ref() {
                result = probe(&id);
            }
            let mut state = pool_state(inner);
            if state.accounts[i].disabled || state.accounts[i].state != State::Cooldown {
                continue; // state changed under us; leave it
            }
            match result {
                ProbeResult::Ok => {
                    state.accounts[i].state = State::Available;
                    state.accounts[i].failures.clear();
                    recovered += 1;
                }
                ProbeResult::AuthExpired => {
                    let Some(renew) = inner.renew.as_ref() else {
                        continue;
                    };
                    drop(state);
                    let renewed = renew(&id); // exactly one renewal attempt
                    let mut state = pool_state(inner);
                    if state.accounts[i].disabled || state.accounts[i].state != State::Cooldown {
                        continue;
                    }
                    if renewed {
                        state.accounts[i].state = State::Available;
                        state.accounts[i].failures.clear();
                        recovered += 1;
                    } else {
                        state.accounts[i].state = State::Disabled;
                        state.accounts[i].disabled = true;
                        state.accounts[i].reason = "session renewal failed".to_string();
                    }
                }
                ProbeResult::Sick => {
                    // Keep cooling at the 5xx base (the ladder continues on
                    // the next failure of that class).
                    let base = state.ladders[&FailureClass::Upstream5xx].base;
                    let now = (state.now)();
                    state.accounts[i].cooldown_until_ms = now + base.as_millis() as i64;
                }
            }
        }
        if recovered > 0 {
            inner.notify.notify_waiters();
        }
        recovered
    }

    /// Total in-flight slots held across all accounts (observability).
    pub fn in_flight(&self) -> usize {
        pool_state(&self.inner)
            .accounts
            .iter()
            .map(|a| a.inflight.load(Ordering::Acquire))
            .sum()
    }

    /// ReapExpiredSlots: force-releases every slot silent beyond its TTL
    /// and aborts the upstream call it was holding (ADR 0019/0036:
    /// silence, not age — the window moves with every `touch`). Returns
    /// how many slots were reclaimed.
    ///
    /// This is the backstop, not the mechanism: the request path itself
    /// bounds each upstream turn with a deadline. The reaper exists because
    /// the failure it prevents is silent — a wedged connection holding an
    /// account slot indefinitely.
    pub fn reap_expired_slots(&self) -> usize {
        let inner = &self.inner;
        if inner.slot_ttl.is_zero() {
            return 0;
        }
        let now = Instant::now();
        let expired: Vec<LeaseHandle> = {
            let state = pool_state(inner);
            state
                .live
                .iter()
                .filter(|l| l.ttl() > Duration::ZERO && l.silent_for(now) > l.ttl())
                .cloned()
                .collect()
        };
        if expired.is_empty() {
            return 0;
        }
        let mut count = 0;
        for lease in expired {
            if let Some(cancel) = lease.on_expire.lock().unwrap().take() {
                cancel(); // abort the upstream call this slot was holding
            }
            lease.release();
            count += 1;
        }
        pool_state(inner).expired_slots += count as u64;
        count
    }

    /// Operator snapshot (SDD §4.9): no credential material of any kind.
    pub fn snapshot(&self) -> Vec<AccountSnapshot> {
        let state = pool_state(&self.inner);
        state
            .accounts
            .iter()
            .map(|a| AccountSnapshot {
                id: a.id.clone(),
                egress: a.egress.clone(),
                state: a.state,
                permanent: a.permanent,
                reason: a.reason.clone(),
                in_flight: a.inflight.load(Ordering::Acquire),
                max_in_flight: state.max_inflight,
                ewma_latency_ms: (a.ewma_latency * 1000.0) as i64,
                cooldown_until_ms: a.cooldown_until_ms,
                cooldown_class: a.cooldown_class,
                cooldown_status: a.cooldown_status,
                cooldown_since_ms: a.cooldown_since_ms,
                failures: a
                    .failures
                    .iter()
                    .filter(|(_, &n)| n > 0)
                    .map(|(&c, &n)| (c, n))
                    .collect(),
                last_error_class: a.last_error_class,
                last_error_status: a.last_error_status,
                last_error_at_ms: a.last_error_at_ms,
            })
            .collect()
    }

    /// State counts (healthz, SDD §5.1).
    pub fn summarize(&self) -> Summary {
        let state = pool_state(&self.inner);
        let mut out = Summary {
            total: state.accounts.len(),
            ..Summary::default()
        };
        for a in &state.accounts {
            if a.disabled {
                out.disabled += 1;
            } else if a.state == State::Cooldown {
                out.cooldown += 1;
            } else {
                out.available += 1;
            }
        }
        out
    }

    /// All member ids (introspection/tests).
    pub fn ids(&self) -> Vec<String> {
        pool_state(&self.inner)
            .accounts
            .iter()
            .map(|a| a.id.clone())
            .collect()
    }

    /// Current state (State::Disabled for unknown ids).
    pub fn state_of(&self, account_id: &str) -> State {
        let state = pool_state(&self.inner);
        state
            .by_id
            .get(account_id)
            .map(|&i| state.accounts[i].state)
            .unwrap_or(State::Disabled)
    }

    /// Cooldown deadline, ms since epoch (tests).
    pub fn cooldown_until_of(&self, account_id: &str) -> i64 {
        let state = pool_state(&self.inner);
        state
            .by_id
            .get(account_id)
            .map(|&i| state.accounts[i].cooldown_until_ms)
            .unwrap_or(0)
    }

    /// Persisted-form state for the store (SDD §7.5): state, cooldown_until
    /// (ms since epoch), disabled_reason.
    pub fn runtime_state(&self, account_id: &str) -> (State, i64, String) {
        let state = pool_state(&self.inner);
        match state.by_id.get(account_id) {
            Some(&i) => {
                let a = &state.accounts[i];
                (a.state, a.cooldown_until_ms, a.reason.clone())
            }
            None => (State::Disabled, 0, String::new()),
        }
    }

    /// Whether the account's disable is permanent (dead passToken);
    /// risk-control disables are never permanent (RISK-002/003).
    pub fn permanent_of(&self, account_id: &str) -> bool {
        let state = pool_state(&self.inner);
        state
            .by_id
            .get(account_id)
            .is_some_and(|&i| state.accounts[i].permanent)
    }

    /// Egress label of an account ("" for unknown ids).
    pub fn egress_of(&self, account_id: &str) -> String {
        let state = pool_state(&self.inner);
        state
            .by_id
            .get(account_id)
            .map(|&i| state.accounts[i].egress.clone())
            .unwrap_or_default()
    }
}

/// Registers a lease on account `idx` and returns the shared handle (Go: the
/// pool tracks a `*Lease` pointer it does not own; here pool and caller
/// share one `Arc<Lease>`).
fn register_lease(inner: &Arc<PoolInner>, state: &mut PoolState, idx: usize) -> LeaseHandle {
    let a = &state.accounts[idx];
    let lease = Arc::new(Lease {
        pool: Some(inner.clone()),
        id: a.id.clone(),
        egress: a.egress.clone(),
        issued: Instant::now(),
        ttl: inner.slot_ttl,
        last_activity_ns: AtomicI64::new(now_ns()), // active from claim
        released: std::sync::atomic::AtomicBool::new(false),
        on_expire: Mutex::new(None),
    });
    if !inner.slot_ttl.is_zero() {
        state.live.push(lease.clone());
    }
    lease
}

/// Distinct accounts behind one egress with a signal inside the window.
fn same_egress_count(state: &PoolState, egress: &str, now: i64) -> usize {
    let window_ms = state.risk_window.as_millis() as i64;
    let mut seen: std::collections::HashSet<&str> = std::collections::HashSet::new();
    for ev in &state.risk_events {
        if ev.egress != egress {
            continue;
        }
        if now - ev.at_ms > window_ms {
            continue;
        }
        seen.insert(ev.account_id.as_str());
    }
    seen.len()
}

/// Locks the pool state (the one mutex).
fn pool_state(inner: &Arc<PoolInner>) -> MutexGuard<'_, PoolState> {
    inner.state.lock().unwrap_or_else(|e| e.into_inner())
}

/// Operator snapshot of one account (SDD §4.9).
#[derive(Debug, Clone)]
pub struct AccountSnapshot {
    pub id: String,
    pub egress: String,
    pub state: State,
    pub permanent: bool,
    pub reason: String,
    pub in_flight: usize,
    pub max_in_flight: usize,
    pub ewma_latency_ms: i64,
    pub cooldown_until_ms: i64,
    pub cooldown_class: Option<FailureClass>,
    pub cooldown_status: u16,
    pub cooldown_since_ms: i64,
    pub failures: Vec<(FailureClass, u32)>,
    pub last_error_class: Option<FailureClass>,
    pub last_error_status: u16,
    pub last_error_at_ms: i64,
}

/// healthz state counts.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Summary {
    pub total: usize,
    pub available: usize,
    pub cooldown: usize,
    pub disabled: usize,
}
