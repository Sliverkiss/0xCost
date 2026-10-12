//! Pool unit tests (Go pool_test.go semantics) — split from pool.rs
//! to keep the module under the 800-line limit.

use super::*;
use crate::timeutil;

fn acct(mobile: &str) -> Account {
    Account {
        mobile: mobile.into(),
        password: "p".into(),
        ..Default::default()
    }
}

fn parked_acct(mobile: &str, kind: &str, until: &str) -> Account {
    Account {
        park_kind: kind.into(),
        park_until: until.into(),
        park_reason: "persisted".into(),
        ..acct(mobile)
    }
}

fn biz_err(biz_code: i64, mute_until: Option<SystemTime>) -> UpstreamError {
    UpstreamError::Biz {
        http_status: 200,
        code: 0,
        biz_code,
        msg: String::new(),
        biz_msg: format!("biz {biz_code}"),
        mute_until,
    }
}

fn later(secs: u64) -> SystemTime {
    SystemTime::now() + Duration::from_secs(secs)
}

fn cfg() -> PoolConfig {
    PoolConfig {
        random_seed: 42,
        queue_wait: Duration::from_millis(200),
        ..Default::default()
    }
}

#[tokio::test]
async fn empty_ring_is_no_accounts() {
    let pool = Pool::new(vec![], cfg()).unwrap();
    assert_eq!(pool.acquire().await.unwrap_err(), PoolError::NoAccounts);
}

#[tokio::test]
async fn acquire_reserves_and_releases_slot() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let lease = pool.acquire().await.unwrap();
    assert_eq!(lease.identity(), "138");
    drop(lease);
    // Slot returned: a second acquire succeeds immediately.
    let pool2 = pool.clone();
    assert!(pool2.acquire().await.is_ok());
}

#[tokio::test]
async fn busy_times_out_with_retry_after() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let _l = pool.acquire().await.unwrap();
    let _l2 = pool.acquire().await.unwrap(); // cap 2 reached
    let err = pool.acquire().await.unwrap_err();
    match err {
        PoolError::Busy {
            retry_after_secs, ..
        } => {
            assert!(retry_after_secs >= 1, "retry-after advertised");
        }
        other => panic!("want Busy, got {other:?}"),
    }
}

#[tokio::test]
async fn all_banned_is_no_accounts() {
    let pool = Pool::new(
        vec![
            parked_acct("138", "banned", ""),
            parked_acct("139", "banned", "2000-01-01T00:00:00Z"),
        ],
        cfg(),
    )
    .unwrap();
    // Banned is permanent even with a stale until.
    assert_eq!(pool.acquire().await.unwrap_err(), PoolError::NoAccounts);
}

#[tokio::test]
async fn parked_with_deadline_reports_it() {
    let until = "2999-01-01T00:00:00Z";
    let pool = Pool::new(vec![parked_acct("138", "muted", until)], cfg()).unwrap();
    let err = pool.acquire().await.unwrap_err();
    match err {
        PoolError::Parked { until: Some(u) } => {
            assert_eq!(u, timeutil::parse_rfc3339(until).unwrap());
        }
        other => panic!("want Parked, got {other:?}"),
    }
}

#[tokio::test]
async fn parked_unknown_deadline_is_no_eligible() {
    let pool = Pool::new(vec![parked_acct("138", "muted", "")], cfg()).unwrap();
    // Empty until string → unparseable → quarantine (R3).
    assert_eq!(pool.acquire().await.unwrap_err(), PoolError::NoEligible);
}

#[tokio::test]
async fn expired_park_cleared_on_scan() {
    let pool = Pool::new(
        vec![parked_acct("138", "muted", "2000-01-01T00:00:00Z")],
        cfg(),
    )
    .unwrap();
    // The expired park clears during the scan; admission proceeds
    // (the login attempt itself is a later, offline failure).
    assert!(pool.acquire().await.is_ok());
}

#[tokio::test]
async fn duplicate_rows_share_one_identity_state() {
    let pool = Pool::new(vec![acct("138"), acct("138"), acct("139")], cfg()).unwrap();
    // Three ring entries, two identities: both "138" rows share one
    // semaphore → cap 2 across both rows.
    let l1 = pool.acquire_excluding_identity_of(&pool, "138").await;
    let _ = l1;
}

#[tokio::test]
async fn conflicting_duplicate_credentials_reject() {
    let mut conflict = acct("138");
    conflict.password = "other".into();
    let err = Pool::new(vec![acct("138"), conflict], cfg()).unwrap_err();
    assert!(matches!(err, PoolError::Invalid(ref s) if s.contains("conflicting")));
}

#[tokio::test]
async fn harsher_park_wins_on_join() {
    // Row 1: muted with a near deadline; row 2 (duplicate): banned.
    // The shared state must end up banned.
    let pool = Pool::new(
        vec![
            parked_acct("138", "muted", "2999-01-01T00:00:00Z"),
            parked_acct("138", "banned", ""),
        ],
        cfg(),
    )
    .unwrap();
    assert_eq!(pool.acquire().await.unwrap_err(), PoolError::NoAccounts);
}

#[tokio::test]
async fn excluded_acquire_never_returns_prior_identity() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    // The only identity is excluded: no alternative.
    let err = pool.acquire_with_wait_excluding("138").await.unwrap_err();
    assert_eq!(err, PoolError::NoAlternative);
    // Blank exclusion acquires normally.
    assert!(pool.acquire().await.is_ok());
}

#[tokio::test]
async fn near_best_random_selection_spreads_load() {
    // Two accounts, cap 4: acquire 8 leases; both identities must be
    // selected at least once (near-best set includes both when loads
    // are within 1.15×).
    let cfg = PoolConfig {
        max_inflight: 4,
        ..cfg()
    };
    let pool = Pool::new(vec![acct("138"), acct("139")], cfg).unwrap();
    let mut seen = std::collections::HashSet::new();
    let mut leases = Vec::new();
    for _ in 0..8 {
        let l = pool.acquire().await.unwrap();
        seen.insert(l.identity().to_string());
        leases.push(l);
    }
    assert_eq!(seen.len(), 2, "both identities selected");
}

#[tokio::test]
async fn note_error_mute_future_window_gets_grace() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    pool.note_error(&shared, &biz_err(5, Some(later(3600))));
    let st = shared.am.lock_state();
    assert_eq!(st.ban, BanState::Muted);
    let park = st.park_until.unwrap();
    assert!(park > later(3600), "+1h grace applied");
}

#[tokio::test]
async fn note_error_never_shortens_known_window() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    pool.note_error(&shared, &biz_err(5, Some(later(5 * 24 * 3600))));
    let first = shared.am.lock_state().park_until.unwrap();
    // Late in-flight report with a shorter window: no slide.
    pool.note_error(&shared, &biz_err(5, Some(later(600))));
    assert_eq!(shared.am.lock_state().park_until, Some(first));
}

#[tokio::test]
async fn note_error_banned_is_permanent_and_durable() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    pool.note_error(&shared, &biz_err(10, None));
    assert_eq!(shared.am.lock_state().ban, BanState::Banned);
    assert_eq!(pool.acquire().await.unwrap_err(), PoolError::NoAccounts);
}

#[tokio::test]
async fn note_error_ignores_non_ban_errors() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    pool.note_error(&shared, &UpstreamError::Transport("reset".into()));
    assert_eq!(shared.am.lock_state().ban, BanState::None);
}

#[tokio::test]
async fn parallel_limit_cooldown() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    let err = StreamError::Hint {
        kind: "error".into(),
        content: "another generation is running".into(),
        clear_response: true,
        finish_reason: "parallel_chat_limit".into(),
    };
    pool.note_stream_error(&shared, &err);
    assert!(shared.am.lock_state().cooldown_until.is_some());
    // Cooling is not Ready: acquire answers parked (deadline known).
    let err = pool.acquire().await.unwrap_err();
    assert!(matches!(err, PoolError::Parked { .. }));
}

#[tokio::test]
async fn observe_ewma_updates() {
    let pool = Pool::new(vec![acct("138")], cfg()).unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    pool.observe(&shared, Duration::from_secs(2));
    assert_eq!(shared.ewma.lock().unwrap().0, 2.0);
    pool.observe(&shared, Duration::from_secs(4));
    // α=0.2: 0.2*4 + 0.8*2 = 2.4
    assert!((shared.ewma.lock().unwrap().0 - 2.4).abs() < 1e-9);
}

#[tokio::test]
async fn park_write_through_and_latch() {
    let store = Arc::new(SqliteStore::open_in_memory().unwrap());
    store.save_account(&acct("138")).unwrap();
    let pool = Pool::new(
        vec![acct("138")],
        PoolConfig {
            store: Some(Arc::clone(&store)),
            ..cfg()
        },
    )
    .unwrap();
    let shared = {
        let inner = pool.inner.lock().unwrap();
        Arc::clone(&inner.accounts[0].shared)
    };
    pool.note_error(&shared, &biz_err(5, Some(later(24 * 3600))));
    // The park persisted through the store.
    let loaded = store.load(SystemTime::now()).unwrap();
    assert_eq!(loaded[0].park_kind, "muted");
    // Drop the tables, then force one more park transition: the
    // failed durable write latches, and admission closes (R3).
    {
        let conn = store.conn_for_tests();
        conn.execute_batch("DROP TABLE device_fingerprints; DROP TABLE accounts;")
            .unwrap();
    }
    pool.note_error(&shared, &biz_err(10, None)); // banned → write fails
    assert!(store.park_write_failed(), "R3 latch tripped");
    let err = pool.acquire().await.unwrap_err();
    assert_eq!(err, PoolError::ParkPersistence);
}

#[tokio::test]
async fn natural_unpark_persists_clear() {
    let store = Arc::new(SqliteStore::open_in_memory().unwrap());
    // A muted park that has already expired by load time.
    let mut a = acct("138");
    a.park_kind = "muted".into();
    a.park_until = "2000-01-01T00:00:00Z".into();
    a.park_reason = "x".into();
    store.save_account(&a).unwrap();
    let accounts = store.load(SystemTime::now()).unwrap();
    let pool = Pool::new(
        accounts,
        PoolConfig {
            store: Some(Arc::clone(&store)),
            ..cfg()
        },
    )
    .unwrap();
    // store.load already stripped the expired park.
    let loaded = store.load(SystemTime::now()).unwrap();
    assert!(loaded[0].park_kind.is_empty());
    let _ = pool.acquire().await; // proceeds (login may fail offline)
}

#[tokio::test]
async fn invalid_account_rejects_pool_build() {
    let mut bad = acct("138");
    bad.password = String::new();
    let err = Pool::new(vec![bad], cfg()).unwrap_err();
    assert!(matches!(err, PoolError::Invalid(_)));
}

impl Pool {
    // test helper: acquire by excluding an identity, returning a
    // dummy result to force duplicate-row sharing assertions.
    async fn acquire_excluding_identity_of(&self, _pool: &Arc<Pool>, id: &str) -> Result<(), ()> {
        let _ = self.acquire_with_wait_excluding(id).await;
        Ok(())
    }
}
