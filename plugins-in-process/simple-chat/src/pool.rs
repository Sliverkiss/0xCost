//! Account pool — port of Go `internal/upstream/pool.go`: scored selection
//! over the account ring, per-identity in-flight caps shared across
//! duplicate rows, and park-state transitions persisted through the
//! plugin's SQLite store (R3 fail-closed latch on admission).
//!
//! Locking shape (Go had one pool mutex + per-manager mutexes; the port
//! keeps the same serialization):
//! - `pool.inner` (std Mutex) guards the ring, PRNG, and EWMA samples —
//!   held only for synchronous state work, never across network calls;
//! - `AccountManager`'s state lock guards per-account fast fields; the
//!   pool takes pool→manager order in scans and releases the manager
//!   lock before re-taking the pool lock in `note_error`;
//! - in-flight caps are `tokio::sync::Semaphore`s (Go buffered channels).

use std::sync::{Arc, Mutex, OnceLock, Weak};
use std::time::{Duration, SystemTime};

use rand::rngs::StdRng;
use rand::{Rng, SeedableRng};
use tokio::sync::Semaphore;

use crate::account::{Account, BanState};
use crate::client::Client;
use crate::lease::{AccountStatus, Lease};
use crate::manager::{local_mute_deadline, AccountManager, MUTE_GRACE};
use crate::sse::StreamError;
use crate::store::{LoginRecord, ParkRecord, SqliteStore};
use crate::wire::{Config, UpstreamError};

/// Default in-flight cap per account when `max_inflight` is unset.
pub const DEFAULT_MAX_INFLIGHT: usize = 2;
/// Poll interval while waiting for a busy slot (Go's 20ms retry tick).
const QUEUE_POLL: Duration = Duration::from_millis(20);
const DEFAULT_MUTE_PARK: Duration = Duration::from_secs(7 * 24 * 3600);
const DEFAULT_RISK_COOLDOWN: Duration = Duration::from_secs(600);
const DEFAULT_PARALLEL_COOLDOWN: Duration = Duration::from_secs(1);
const DEFAULT_QUEUE_WAIT: Duration = Duration::from_secs(30);

/// Pool admission failures (Go's pool error family). Terminal unless
/// noted.
#[derive(Debug, Clone, PartialEq, thiserror::Error)]
pub enum PoolError {
    /// Every account is banned (permanent) — waiting cannot help.
    #[error("upstream: pool has no available accounts (all banned)")]
    NoAccounts,
    /// A retry cannot select a distinct ready account; terminate, never
    /// reuse the prior one.
    #[error("upstream: no eligible alternative account")]
    NoAlternative,
    /// All accounts busy; the pool waited `queue_wait` and gave up.
    #[error(
        "upstream: all accounts busy (waited {}s; retry after {}s)",
        waited_secs,
        retry_after_secs
    )]
    Busy {
        waited_secs: u64,
        retry_after_secs: u64,
    },
    /// No identity ready; the nearest known local unpark deadline is
    /// `until`. Not a capacity/QueueWait failure.
    #[error("upstream: all eligible accounts parked")]
    Parked { until: Option<SystemTime> },
    /// A parked pool without any known recovery deadline — never
    /// advertise a fabricated Retry-After.
    #[error("upstream: no eligible account with known recovery")]
    NoEligible,
    /// A park transition was not confirmed by the backing store; this
    /// process can no longer safely assign leases (R3 fail-closed).
    #[error("pool: park persistence failed; account admission closed")]
    ParkPersistence,
    /// Account validation/construction failure at pool build.
    #[error("{0}")]
    Invalid(String),
}

/// Pool configuration (Go `PoolConfig`; the store is wired directly — the
/// plugin always persists, a `None` store keeps memory-only semantics
/// for tests).
#[derive(Clone)]
pub struct PoolConfig {
    pub store: Option<Arc<SqliteStore>>,
    /// Overrides every client's base URL (tests inject a local server).
    pub base_url: String,
    pub max_inflight: usize,
    pub queue_wait: Duration,
    pub mute_park_default: Duration,
    pub risk_cooldown: Duration,
    pub parallel_limit_cooldown: Duration,
    /// Makes selection reproducible in tests; 0 seeds from the clock.
    pub random_seed: u64,
    /// Latency adaptation; 0 uses 0.2.
    pub ewma_alpha: f64,
}

impl Default for PoolConfig {
    fn default() -> Self {
        PoolConfig {
            store: None,
            base_url: String::new(),
            max_inflight: DEFAULT_MAX_INFLIGHT,
            queue_wait: DEFAULT_QUEUE_WAIT,
            mute_park_default: DEFAULT_MUTE_PARK,
            risk_cooldown: DEFAULT_RISK_COOLDOWN,
            parallel_limit_cooldown: DEFAULT_PARALLEL_COOLDOWN,
            random_seed: 0,
            ewma_alpha: 0.0,
        }
    }
}

/// Runtime state shared by every duplicate row of one physical identity:
/// client, manager, in-flight semaphore, EWMA, and duplicate-report
/// bookkeeping (Go's joined `poolAccount` pointers).
pub(crate) struct SharedAccountState {
    pub(crate) identity: String,
    pub(crate) client: Arc<Client>,
    pub(crate) am: Arc<AccountManager>,
    /// In-flight cap (Go `slots` channel): permits free = slots free.
    pub(crate) slots: Semaphore,
    /// EWMA latency in seconds and sample count (Go: `ewmaNanos`/
    /// `samples` under `p.mu`). Interior-mutable because observe() takes
    /// `&Arc<SharedAccountState>`.
    ewma: Mutex<(f64, u64)>,
    /// First NoteError persistence after `mark_ban` set a mute deadline
    /// without persisting it (duplicate-report suppression, Go
    /// `parkReported`). Own mutex: written while the manager lock is
    /// held but the pool lock is not.
    park_reported: Mutex<Option<SystemTime>>,
    /// Fenced generation snapshot from the store (0 = unfenced).
    generation: u64,
}

/// One ring entry: the account row plus its shared runtime state.
struct PoolEntry {
    account: Account,
    shared: Arc<SharedAccountState>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Health {
    Ready,
    Banned,
    Muted,
    Risk,
    Cooling,
}

struct PoolInner {
    accounts: Vec<PoolEntry>,
    rng: StdRng,
}

/// The account ring (Go `Pool`). Always constructed as `Arc<Pool>` —
/// leases borrow the pool through a weak self-reference.
pub struct Pool {
    weak: OnceLock<Weak<Pool>>,
    inner: Mutex<PoolInner>,
    cfg: PoolConfig,
}

impl std::fmt::Debug for Pool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Pool")
            .field("accounts", &self.inner.lock().map(|i| i.accounts.len()))
            .finish()
    }
}

impl Pool {
    /// Build the ring. An empty ring is valid: a fresh deployment boots
    /// with zero accounts and is seeded via the CLI; acquire on the empty
    /// ring answers [`PoolError::NoAccounts`].
    pub fn new(accounts: Vec<Account>, cfg: PoolConfig) -> Result<Arc<Pool>, PoolError> {
        let seed = if cfg.random_seed == 0 {
            SystemTime::now()
                .duration_since(SystemTime::UNIX_EPOCH)
                .map(|d| d.as_nanos() as u64)
                .unwrap_or(0)
        } else {
            cfg.random_seed
        };
        let pool = Arc::new(Pool {
            weak: OnceLock::new(),
            inner: Mutex::new(PoolInner {
                accounts: Vec::new(),
                rng: StdRng::seed_from_u64(seed),
            }),
            cfg,
        });
        pool.weak.set(Arc::downgrade(&pool)).ok();
        let mut parked = 0;
        for a in accounts {
            let fresh = pool.build_shared(&a)?;
            let shared = pool.join_identity(&a, fresh)?;
            if shared.am.lock_state().ban != BanState::None {
                parked += 1;
            }
            pool.inner
                .lock()
                .unwrap()
                .accounts
                .push(PoolEntry { account: a, shared });
        }
        if parked > 0 {
            tracing::info!(parked, "pool: accounts parked at startup");
        }
        Ok(pool)
    }

    fn self_arc(&self) -> Arc<Pool> {
        self.weak
            .get()
            .and_then(Weak::upgrade)
            .expect("pool outlives its leases")
    }

    /// Validate one account and assemble its runtime state (client,
    /// manager, semaphore, persisted park restore) — the single
    /// construction path (Go `buildAccount`).
    fn build_shared(&self, a: &Account) -> Result<Arc<SharedAccountState>, PoolError> {
        a.validate()
            .map_err(|e| PoolError::Invalid(format!("upstream: account {}: {e}", a.identity())))?;
        let client = Arc::new(
            Client::new(Config {
                base_url: self.cfg.base_url.clone(),
                mobile: a.mobile.clone(),
                email: a.email.clone(),
                password: a.password.clone(),
                // Per-account persistent device identity: explicit value
                // verbatim, else the deterministic mint. Never one shared
                // id across accounts — risk-service correlation vector.
                device_id: crate::device::resolve_device_id(&a.device_id, a.identity()),
                channel: a.channel.clone(),
            })
            .map_err(|e| PoolError::Invalid(e.to_string()))?,
        );
        let am = Arc::new(AccountManager::new(
            Arc::clone(&client),
            self.cfg.mute_park_default,
            self.cfg.risk_cooldown,
        ));
        let generation = self
            .cfg
            .store
            .as_ref()
            .and_then(|s| s.generation(a.identity()).ok())
            .unwrap_or(0);
        // Login write-through: every successful login/relogin persists the
        // fresh token. The hook's `active` flag stays true for the pool's
        // fixed ring (Go's RemoveAccount API is admin-surface-only, out of
        // scope for the plugin form) — kept because the facade may later
        // re-create pools per store reload.
        if let Some(store) = &self.cfg.store {
            let store = Arc::clone(store);
            let identity = a.identity().to_string();
            am.set_on_login(Arc::new(move |tok| {
                store.apply_login(&LoginRecord {
                    identity: identity.clone(),
                    token: tok.to_string(),
                    generation,
                });
            }));
        }
        let shared = Arc::new(SharedAccountState {
            identity: a.identity().to_string(),
            client,
            am,
            slots: Semaphore::new(self.cfg.max_inflight),
            ewma: Mutex::new((0.0, 0)),
            park_reported: Mutex::new(None),
            generation,
        });
        // Restore persisted park state (TASK_MUTE): an account
        // muted/banned before a restart must not re-enter rotation
        // "clean" — re-hitting the upstream renews the window (observed
        // 6h mute → 3-day ban escalation). Expired parks were already
        // stripped by the store load; banned persists forever.
        let kind = BanState::parse(&a.park_kind);
        if kind != BanState::None {
            let until = crate::timeutil::parse_rfc3339(&a.park_until);
            let mut st = shared.am.lock_state();
            if kind == BanState::Banned {
                st.ban = BanState::Banned;
                st.ban_msg = a.park_reason.clone();
                tracing::info!(
                    identity = %shared.identity,
                    "pool: account still BANNED (restored from store; manual revive = clear park fields)"
                );
            } else if until.is_none() || SystemTime::now() < until.unwrap() {
                st.ban = kind;
                st.park_until = until;
                st.ban_msg = a.park_reason.clone();
                match until {
                    None => tracing::warn!(
                        identity = %shared.identity,
                        "pool: account still parked with unknown recovery (restored from store)"
                    ),
                    Some(u) => tracing::info!(
                        identity = %shared.identity,
                        until = ?u,
                        "pool: account still parked (restored from store)"
                    ),
                }
            }
        }
        Ok(shared)
    }

    /// Share one runtime state per physical identity (Go `joinIdentity`):
    /// a duplicate row joins the existing shared state — conflicting
    /// credentials reject — and the harsher park state wins into the
    /// existing entry. Returns the shared state the new row will use.
    fn join_identity(
        &self,
        a: &Account,
        fresh: Arc<SharedAccountState>,
    ) -> Result<Arc<SharedAccountState>, PoolError> {
        let inner = self.inner.lock().unwrap();
        for existing in &inner.accounts {
            if existing.shared.identity != fresh.identity {
                continue;
            }
            // A shared client must never silently select one row's login
            // or wire fingerprint. Credential values stay out of errors.
            let same = existing.account.mobile == a.mobile
                && existing.account.email == a.email
                && existing.account.password == a.password
                && existing.account.normalized_region().ok() == a.normalized_region().ok()
                && crate::device::resolve_device_id(
                    &existing.account.device_id,
                    existing.account.identity(),
                ) == crate::device::resolve_device_id(&a.device_id, a.identity())
                && existing.account.channel == a.channel;
            if !same {
                return Err(PoolError::Invalid(format!(
                    "upstream: conflicting account configuration for identity {}",
                    fresh.identity
                )));
            }
            // Harsher park wins: banned > any; unknown recovery > known;
            // later window > earlier; equal windows prefer mute.
            {
                let mut ex = existing.shared.am.lock_state();
                let new = fresh.am.lock_state();
                let ban_wins = (new.ban == BanState::Banned
                    && (ex.ban != BanState::Banned || new.ban_msg > ex.ban_msg))
                    || (new.ban != BanState::None && ex.ban == BanState::None)
                    || (new.ban != BanState::None
                        && new.ban != BanState::Banned
                        && ex.ban != BanState::Banned
                        && (new.park_until.is_none() && ex.park_until.is_some()
                            || new.park_until.is_some_and(|n| {
                                n > ex.park_until.unwrap_or(SystemTime::UNIX_EPOCH)
                            })
                            || new.park_until == ex.park_until
                                && (new.ban == BanState::Muted && ex.ban == BanState::RiskDevice
                                    || new.ban == ex.ban && new.ban_msg > ex.ban_msg)));
                if ban_wins {
                    ex.ban = new.ban;
                    ex.park_until = new.park_until;
                    ex.ban_msg = new.ban_msg.clone();
                }
            }
            return Ok(Arc::clone(&existing.shared));
        }
        Ok(fresh)
    }

    /// Acquire scores ready identities with free capacity and reserves a
    /// slot (Go `Acquire`).
    pub async fn acquire(&self) -> Result<Lease, PoolError> {
        self.acquire_with_wait_excluding("").await.map(|(l, _)| l)
    }

    /// Acquire plus the time spent waiting for a free slot (Go
    /// `AcquireWithWait`).
    pub async fn acquire_with_wait(&self) -> Result<(Lease, Duration), PoolError> {
        self.acquire_with_wait_excluding("").await
    }

    /// Reserve a lease for a DIFFERENT physical identity during one retry
    /// (Go `AcquireWithWaitExcluding`): all duplicate entries of the
    /// prior identity are excluded; blank excludes nothing. Never falls
    /// back to the prior account or bypasses health/park checks.
    pub async fn acquire_with_wait_excluding(
        &self,
        prior_identity: &str,
    ) -> Result<(Lease, Duration), PoolError> {
        let started = SystemTime::now();
        let deadline = SystemTime::now() + self.cfg.queue_wait;
        loop {
            match self.try_acquire_excluding(prior_identity) {
                Ok(lease) => return Ok((lease, started.elapsed().unwrap_or_default())),
                Err(PoolError::Parked { until: Some(u) }) if SystemTime::now() >= u => {
                    // The earliest park expired while selection was
                    // blocked; re-scan before emitting a stale error.
                    continue;
                }
                Err(PoolError::Busy { .. }) => {
                    if SystemTime::now() > deadline {
                        return Err(PoolError::Busy {
                            waited_secs: self.cfg.queue_wait.as_secs(),
                            retry_after_secs: self.cfg.queue_wait.as_secs() + 1,
                        });
                    }
                    tokio::time::sleep(QUEUE_POLL).await;
                }
                Err(e) => return Err(e),
            }
        }
    }

    /// One non-blocking pass over the ring (Go `tryAcquireExcluding`).
    fn try_acquire_excluding(&self, excluded: &str) -> Result<Lease, PoolError> {
        let mut inner = self.inner.lock().unwrap();
        // R3: admission closes after a failed durable park write.
        if self.store_latch() {
            return Err(PoolError::ParkPersistence);
        }
        let n = inner.accounts.len();
        if n == 0 {
            // Empty ring: nothing can free a slot, waiting cannot help.
            return Err(PoolError::NoAccounts);
        }
        let now = SystemTime::now();
        let mut any_banned = false;
        let mut distinct_ready = false;
        let mut parked = false;
        let mut earliest: Option<SystemTime> = None;
        let mut unknown_recovery = false;
        let mut ready: Vec<Arc<SharedAccountState>> = Vec::with_capacity(n);
        for entry in &inner.accounts {
            if !excluded.is_empty() && entry.shared.identity == excluded {
                continue;
            }
            let h = self.health_now(&entry.shared, now);
            if h == Health::Banned {
                any_banned = true;
                continue;
            }
            if h != Health::Ready {
                // health_now reconciled expired parks; snapshot the
                // eligibility deadline while holding the manager lock.
                let until = {
                    let st = entry.shared.am.lock_state();
                    if h == Health::Cooling {
                        st.cooldown_until
                    } else {
                        st.park_until
                    }
                };
                parked = true;
                match until {
                    Some(u) if u > now => {
                        if earliest.is_none_or(|e| u < e) {
                            earliest = Some(u);
                        }
                    }
                    _ => unknown_recovery = true,
                }
                continue;
            }
            distinct_ready = true;
            if entry.shared.slots.available_permits() == 0 {
                continue; // busy at capacity
            }
            ready.push(Arc::clone(&entry.shared));
        }
        // health_now may naturally clear an expired park and
        // synchronously fail its durable write during this scan; recheck
        // the sticky store latch before reserving any slot.
        if self.store_latch() {
            return Err(PoolError::ParkPersistence);
        }
        if !ready.is_empty() {
            let best = ready
                .iter()
                .map(|s| self.account_score(s))
                .fold(f64::INFINITY, f64::min);
            let near: Vec<&Arc<SharedAccountState>> = ready
                .iter()
                .filter(|s| self.account_score(s) <= best * 1.15 + 1.0)
                .collect();
            let idx = inner.rng.gen_range(0..near.len());
            let shared = Arc::clone(near[idx]);
            // Capacity was checked under the pool lock and permits only
            // decrease while it is held — try_acquire cannot fail here.
            shared
                .slots
                .try_acquire()
                .map_err(|_| PoolError::Busy {
                    waited_secs: 0,
                    retry_after_secs: 0,
                })?
                .forget();
            return Ok(Lease {
                pool: self.self_arc(),
                shared,
            });
        }
        if !excluded.is_empty() && !distinct_ready {
            return Err(PoolError::NoAlternative);
        }
        // Terminal all-banned check while holding the pool lock (the ring
        // is mutable by hot administration in Go; our ring is fixed after
        // construction, but keep the scan — health transitions matter).
        let all_gone = any_banned
            && inner
                .accounts
                .iter()
                .all(|e| self.health_now(&e.shared, now) == Health::Banned);
        if self.store_latch() {
            return Err(PoolError::ParkPersistence);
        }
        if all_gone {
            return Err(PoolError::NoAccounts);
        }
        if !distinct_ready && parked {
            if earliest.is_none() || unknown_recovery {
                return Err(PoolError::NoEligible);
            }
            return Err(PoolError::Parked { until: earliest });
        }
        Err(PoolError::Busy {
            waited_secs: 0,
            retry_after_secs: 0,
        })
    }

    /// The account's effective health, clearing expired parks and
    /// persisting the natural unpark (Go `healthNow`). Caller holds the
    /// pool lock.
    fn health_now(&self, shared: &Arc<SharedAccountState>, now: SystemTime) -> Health {
        let mut st = shared.am.lock_state();
        if st.ban == BanState::Banned {
            return Health::Banned;
        }
        if let Some(until) = st.park_until {
            if now >= until {
                st.park_until = None;
                if matches!(st.ban, BanState::Muted | BanState::RiskDevice) {
                    st.ban = BanState::None;
                    // Natural unpark is a persisted transition: clear the
                    // parked fields so the next restart does not
                    // resurrect an expired park.
                    if let Some(store) = &self.cfg.store {
                        store.apply_park(
                            &ParkRecord {
                                identity: shared.identity.clone(),
                                kind: BanState::None,
                                until: None,
                                reason: String::new(),
                                generation: shared.generation,
                            },
                            now,
                        );
                    }
                }
            }
        }
        match st.ban {
            BanState::Banned => Health::Banned,
            BanState::Muted => Health::Muted,
            BanState::RiskDevice => Health::Risk,
            BanState::None => {
                if st.cooldown_until.is_some_and(|c| now < c) {
                    Health::Cooling
                } else {
                    Health::Ready
                }
            }
        }
    }

    /// Score one ready account (Go `accountScore`): lower is better.
    /// In-flight dominates; measured latency breaks ties; cold accounts
    /// get a neutral prior. Caller holds the pool lock.
    fn account_score(&self, shared: &Arc<SharedAccountState>) -> f64 {
        let capacity = self.cfg.max_inflight.max(1) as f64;
        let inflight = (capacity - shared.slots.available_permits() as f64).max(0.0);
        let load = inflight / capacity;
        let latency = 1.0 + shared.ewma.lock().unwrap().0;
        let warm_penalty = if shared.am.lock_state().token.is_empty() {
            0.05
        } else {
            0.0
        };
        load * 100.0 + latency + warm_penalty
    }

    /// Record one completed request latency (Go `Observe`). Crate-level:
    /// `Lease` is the public handle; the pool method exists for tests and
    /// the facade, which operate inside the crate boundary.
    pub(crate) fn observe(&self, shared: &Arc<SharedAccountState>, elapsed: Duration) {
        let _inner = self.inner.lock().unwrap();
        let alpha = if self.cfg.ewma_alpha <= 0.0 || self.cfg.ewma_alpha >= 1.0 {
            0.2
        } else {
            self.cfg.ewma_alpha
        };
        let sample = elapsed.as_secs_f64();
        let mut ewma = shared.ewma.lock().unwrap();
        if ewma.1 == 0 {
            ewma.0 = sample;
        } else {
            ewma.0 = alpha * sample + (1.0 - alpha) * ewma.0;
        }
        ewma.1 += 1;
    }

    /// Note an upstream error: a park transition persisted through the
    /// store (Go `Lease.NoteError`'s ban ladder). Crate-level: the
    /// public handle is [`Lease::note_error`].
    pub(crate) fn note_error(&self, shared: &Arc<SharedAccountState>, err: &UpstreamError) {
        let kind = err.ban_kind();
        if kind == BanState::None {
            return; // relogin-worthy auth failures etc. carry no park
        }
        {
            let mut st = shared.am.lock_state();
            // The manager may have observed this very ban while executing
            // the lease's upstream call (mark_ban ran first): its
            // in-memory transition still needs one durable write.
            if st.ban == BanState::Banned && !st.ban_park_pending {
                return;
            }
            if st.ban == BanState::Banned && kind != BanState::Banned {
                return;
            }
            // An existing unknown recovery time must not be replaced by a
            // freshly computed fallback from a late in-flight error.
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
                    st.ban_park_pending = false;
                    st.park_until = None;
                    st.ban_msg = format!("account banned: {err}");
                }
                BanState::Muted => {
                    // Future upstream window: the +1h grace Go's markBan
                    // stamped into localMutePark — recomputed identically
                    // here (Go mutates the shared BizError instead).
                    let until = match mute_until.filter(|t| *t > now) {
                        Some(mu) => mu + MUTE_GRACE,
                        None => {
                            // The manager parked this same biz-5 before
                            // note_error saw it; a fallback recomputed
                            // here would extend the deadline on every
                            // duplicate report. Keep the existing future
                            // window.
                            if st.ban == BanState::Muted && st.park_until.is_some_and(|p| p > now) {
                                st.park_until.unwrap()
                            } else {
                                local_mute_deadline(mute_until, self.cfg.mute_park_default, now)
                            }
                        }
                    };
                    // Never make a known mute eligible sooner.
                    if st.ban != BanState::None && st.park_until.is_some_and(|p| p > until) {
                        return;
                    }
                    // Equal windows still need their first durable write:
                    // suppress only later duplicates.
                    if st.ban == BanState::Muted
                        && st.park_until == Some(until)
                        && *shared.park_reported.lock().unwrap() == Some(until)
                    {
                        return;
                    }
                    st.ban = BanState::Muted;
                    st.park_until = Some(until);
                    st.ban_msg = format!("account muted: {err}");
                    *shared.park_reported.lock().unwrap() = Some(until);
                }
                BanState::RiskDevice => {
                    let until = now + self.cfg.risk_cooldown;
                    if st.ban != BanState::None && st.park_until.is_some_and(|p| p > until) {
                        return;
                    }
                    st.ban = BanState::RiskDevice;
                    st.park_until = Some(until);
                    st.ban_msg = format!("risk device detected: {err}");
                }
                BanState::None => unreachable!("kind checked above"),
            }
        }
        // Persist the park while this identity is still in the ring (a
        // removed lease may finish after a re-add; its manager is
        // intentionally detached — never overwrite the new record's
        // persisted health).
        let in_ring = self
            .inner
            .lock()
            .unwrap()
            .accounts
            .iter()
            .any(|e| Arc::ptr_eq(&e.shared, shared));
        if in_ring {
            if let Some(store) = &self.cfg.store {
                let st = shared.am.lock_state();
                store.apply_park(
                    &ParkRecord {
                        identity: shared.identity.clone(),
                        kind: st.ban,
                        until: st.park_until,
                        reason: st.ban_msg.clone(),
                        generation: shared.generation,
                    },
                    SystemTime::now(),
                );
            }
        }
    }

    /// Briefly suppress an identity after an explicit per-account
    /// parallel generation limit (Go's `IsParallelLimit` branch).
    /// Crate-level: the public handle is [`Lease::note_stream_error`].
    pub(crate) fn note_stream_error(&self, shared: &Arc<SharedAccountState>, err: &StreamError) {
        if err.is_parallel_limit() {
            let mut st = shared.am.lock_state();
            let until = SystemTime::now() + self.cfg.parallel_limit_cooldown;
            if st.cooldown_until.is_none_or(|c| until > c) {
                st.cooldown_until = Some(until);
            }
        }
        // Other mid-stream errors carry no ban classification (SSE hint
        // payloads have no biz codes).
    }

    /// Per-account states for the health endpoint (Go `Status`). Park
    /// fields never include tokens or credentials.
    pub fn snapshot(&self) -> Vec<AccountStatus> {
        let inner = self.inner.lock().unwrap();
        let now = SystemTime::now();
        let mut out = Vec::with_capacity(inner.accounts.len());
        for entry in &inner.accounts {
            let h = self.health_now(&entry.shared, now);
            let state = match h {
                Health::Ready => "ready",
                Health::Banned => "banned",
                Health::Muted => "muted",
                Health::Risk => "risk",
                Health::Cooling => "cooling",
            };
            let st = entry.shared.am.lock_state();
            out.push(AccountStatus {
                identity: entry.shared.identity.clone(),
                state: state.into(),
                inflight: self.cfg.max_inflight - entry.shared.slots.available_permits(),
                max_inflight: self.cfg.max_inflight,
                token_warm: !st.token.is_empty(),
            });
        }
        out
    }

    fn store_latch(&self) -> bool {
        self.cfg
            .store
            .as_ref()
            .is_some_and(|s| s.park_write_failed())
    }
}

#[cfg(test)]
#[path = "pool_tests.rs"]
mod tests;
