//! TDD pool vector replay (Go docs/TDD.md §3.3/§3.10/§3.11).

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use super::*;

/// Fake clock: shared `AtomicI64`, so the closure is Send+Sync.
struct FakeClock(Arc<std::sync::atomic::AtomicI64>);
impl FakeClock {
    fn new(start_ms: i64) -> Self {
        Self(Arc::new(std::sync::atomic::AtomicI64::new(start_ms)))
    }
    fn clock(&self) -> Clock {
        let shared = self.0.clone();
        Arc::new(move || shared.load(Ordering::SeqCst))
    }
    fn set(&self, ms: i64) {
        self.0.store(ms, Ordering::SeqCst);
    }
}

fn recs(ids: &[&str]) -> Vec<AccountRecord> {
    ids.iter()
        .map(|id| AccountRecord {
            id: id.to_string(),
            ..Default::default()
        })
        .collect()
}

fn pool_with(ids: &[&str], clock: &FakeClock) -> Pool {
    Pool::new(
        &recs(ids),
        Options {
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    )
}

#[test]
fn pool_001_round_robin_fairness() {
    // 3 available accounts A,B,C; 30 sequential acquire/release → 10 each.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a", "b", "c"], &clock);
    let mut counts = HashMap::new();
    for _ in 0..30 {
        let l = p.try_acquire(&[]).unwrap();
        *counts.entry(l.id.clone()).or_insert(0) += 1;
        l.release();
    }
    assert_eq!(counts["a"], 10);
    assert_eq!(counts["b"], 10);
    assert_eq!(counts["c"], 10);
    // Strict rotation: cursor advances through the band in stable account
    // order (Go starts at cursor 1 — any cyclic order satisfies the vector).
    let order: Vec<String> = (0..3)
        .map(|_| {
            let l = p.try_acquire(&[]).unwrap();
            let id = l.id.clone();
            l.release();
            id
        })
        .collect();
    assert_ne!(order[0], order[1]);
    assert_ne!(order[1], order[2]);
    assert_ne!(order[2], order[0]);
}

#[test]
fn pool_002_cooldown_on_429() {
    // report upstream 429 → state=cooldown, until=now+60s, excluded.
    let clock = FakeClock::new(1_000_000);
    let p = pool_with(&["a"], &clock);
    p.report_429("a");
    assert_eq!(p.state_of("a"), State::Cooldown);
    assert_eq!(p.cooldown_until_of("a"), 1_000_000 + 60_000);
    assert!(matches!(p.try_acquire(&[]), Err(PoolError::Parked { .. })));
}

#[test]
fn pool_003_backoff_ladder_429() {
    // Consecutive 429s (clock advances past each expiry): 60s, 120s,
    // 240s, 480s, 600s, 600s… cap 10 min.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    let expected = [60_000, 120_000, 240_000, 480_000, 600_000, 600_000];
    let mut now = 0i64;
    for &want in &expected {
        p.report_429("a");
        assert_eq!(
            p.cooldown_until_of("a") - now,
            want,
            "ladder step at now={now}"
        );
        now += want + 1; // advance past the cooldown (fake clock)
        clock.set(now);
    }
}

#[test]
fn pool_004_success_resets_ladder() {
    // Cooldown expires, probe OK, one success, then a 429 → 60s again.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    // Run the ladder up 4 steps.
    for i in 0..4 {
        p.report_429("a");
        clock.set(i * 1_000_000 + 1_000_000);
    }
    clock.set(100_000_000);
    p.enable("a"); // operator/probe recovered it
    p.report_success("a");
    p.report_429("a");
    assert_eq!(p.cooldown_until_of("a") - 100_000_000, 60_000);
}

#[test]
fn pool_005_5xx_cooldown_ladder() {
    // report 502 → cooldown from 30s, same doubling ladder.
    let clock = FakeClock::new(5_000);
    let p = pool_with(&["a"], &clock);
    p.report_5xx("a");
    assert_eq!(p.cooldown_until_of("a") - 5_000, 30_000);
    clock.set(40_000);
    p.report_5xx("a");
    assert_eq!(p.cooldown_until_of("a") - 40_000, 60_000);
}

#[test]
fn pool_006_recovery_probe_gate() {
    // Cooldown expired: selection attempt before probe → still
    // excluded; after OK probe → available.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a", "b"], &clock);
    p.report_429("a");
    clock.set(61_000); // expiry passed
                       // Expiry alone is NOT selectable (probe gate first).
    let l = p.try_acquire(&[]).unwrap();
    assert_eq!(l.id, "b"); // "a" still excluded
    l.release();
    // Probe OK via sweep with a healthy probe.
    let p2 = Pool::new(
        &recs(&["a"]),
        Options {
            now: Some(clock.clock()),
            ..Default::default()
        },
        Some(Arc::new(|_| ProbeResult::Ok)),
        None,
    );
    p2.report_429("a");
    clock.set(61_000);
    p2.probe_sweep();
    clock.set(200_000); // re-armed cooldown also expired
    assert_eq!(p2.probe_sweep(), 1);
    assert_eq!(p2.state_of("a"), State::Available);
    let l2 = p2.try_acquire(&[]).unwrap();
    assert_eq!(l2.id, "a");
    l2.release();
}

#[test]
fn pool_007_probe_auth_expired_renew_or_disable() {
    // Probe 401 → exactly one renewal; success → available, failure → disabled.
    let clock = FakeClock::new(0);
    let opts = Options {
        now: Some(clock.clock()),
        ..Default::default()
    };
    let renew_calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let rc = renew_calls.clone();

    // Success path.
    let p = Pool::new(
        &recs(&["a"]),
        opts.clone(),
        Some(Arc::new(|_| ProbeResult::AuthExpired)),
        Some(Arc::new(move |id| {
            rc.fetch_add(1, Ordering::SeqCst);
            id == "a"
        })),
    );
    p.report_429("a");
    clock.set(61_000);
    assert_eq!(p.probe_sweep(), 1);
    assert_eq!(renew_calls.load(Ordering::SeqCst), 1); // exactly one
    assert_eq!(p.state_of("a"), State::Available);

    // Failure path.
    let rc2 = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let rc2c = rc2.clone();
    let p = Pool::new(
        &recs(&["a"]),
        opts,
        Some(Arc::new(|_| ProbeResult::AuthExpired)),
        Some(Arc::new(move |_| {
            rc2c.fetch_add(1, Ordering::SeqCst);
            false
        })),
    );
    p.report_429("a");
    clock.set(200_000);
    assert_eq!(p.probe_sweep(), 0);
    assert_eq!(renew_calls2(&rc2), 1);
    assert_eq!(p.state_of("a"), State::Disabled);

    fn renew_calls2(c: &Arc<std::sync::atomic::AtomicUsize>) -> usize {
        c.load(Ordering::SeqCst)
    }
}

#[test]
fn pool_008_all_cooling_fail_closed() {
    // All 3 cooling, nearest expiry now+42s → Parked(42s).
    let clock = FakeClock::new(0);
    let p = pool_with(&["a", "b", "c"], &clock);
    p.report_429("a"); // +60s
                       // Custom: b at +42s, c at +90s via direct failure with a short
                       // window — use cooldown persistence instead (report at a moved
                       // clock then wind back is not possible; construct directly).
    let recs = vec![
        AccountRecord {
            id: "a".into(),
            state: Some(State::Cooldown),
            cooldown_until_ms: Some(60_000),
            ..Default::default()
        },
        AccountRecord {
            id: "b".into(),
            state: Some(State::Cooldown),
            cooldown_until_ms: Some(42_000),
            ..Default::default()
        },
        AccountRecord {
            id: "c".into(),
            state: Some(State::Cooldown),
            cooldown_until_ms: Some(90_000),
            ..Default::default()
        },
    ];
    let p = Pool::new(
        &recs,
        Options {
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    );
    match p.try_acquire(&[]) {
        Err(PoolError::Parked { retry_after_ms }) => assert_eq!(retry_after_ms, 42_000),
        other => panic!("expected parked, got {}", other.is_err()),
    }
    // Retry-After clamps to ≥1s.
    assert_eq!(
        PoolError::Parked {
            retry_after_ms: 42_000
        }
        .retry_after_seconds(),
        Some(42)
    );
}

#[tokio::test]
async fn pool_009_010_semaphore_and_queue_wait() {
    // 1 account, max_inflight=2, queue_wait=150ms: 3rd acquire waits,
    // then fails pool_busy with retry-after present (POOL-009/010).
    let clock = FakeClock::new(0);
    let p = Pool::new(
        &recs(&["a"]),
        Options {
            max_inflight: 2,
            queue_wait: Duration::from_millis(150),
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    );
    let l1 = p.try_acquire(&[]).unwrap();
    let l2 = p.try_acquire(&[]).unwrap();
    assert_eq!(p.in_flight(), 2);
    let p3 = p.clone();
    let start = Instant::now();
    let third = tokio::spawn(async move { p3.acquire(&[]).await });
    let result = third.await.unwrap();
    assert!(matches!(result, Err(PoolError::QueueTimeout { .. })));
    assert!(start.elapsed() >= Duration::from_millis(140));
    if let Err(PoolError::QueueTimeout { retry_after_ms }) = &result {
        assert!(*retry_after_ms > 0, "Retry-After present");
    } else {
        panic!("expected QueueTimeout");
    }
    // Release path: waiter proceeds when a slot frees (no leak,
    // POOL-016 spirit).
    l1.release();
    l2.release();
    assert_eq!(p.in_flight(), 0);
    assert!(p.try_acquire(&[]).is_ok());

    // Idempotent release: no double-counting.
    l1.release(); // second call is a no-op
    assert_eq!(p.in_flight(), 1);
}

#[test]
fn pool_011_empty_or_all_disabled() {
    let clock = FakeClock::new(0);
    let p = pool_with(&[], &clock);
    assert!(matches!(p.try_acquire(&[]), Err(PoolError::NoAccounts)));
    let recs = vec![AccountRecord {
        id: "a".into(),
        state: Some(State::Disabled),
        ..Default::default()
    }];
    let p = Pool::new(
        &recs,
        Options {
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    );
    assert!(matches!(p.try_acquire(&[]), Err(PoolError::NoAccounts)));
}

#[test]
fn pool_012_risk_control_disable() {
    // 451/461 → disabled with reason; ReportDisabled(permanent=true)
    // records permanence; permanent accounts never probed.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    let n = p.report_risk_control(
        "a",
        "account restricted",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    assert_eq!(n, 1);
    assert_eq!(p.state_of("a"), State::Disabled);
    assert!(!p.permanent_of("a")); // RISK-002
                                   // Dead passToken is permanent (RISK-003).
    p.report_disabled("a", "passToken dead", true);
    assert!(p.permanent_of("a"));
}

#[test]
fn pool_013_cooldown_persists_across_restart() {
    // Persisted state → new pool restores cooldown_until.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_429("a");
    p.report_429("a"); // 120s
    let (state, until, reason) = p.runtime_state("a");
    assert_eq!(state, State::Cooldown);
    assert_eq!(until, 120_000);
    assert!(reason.is_empty());
    let recs = vec![AccountRecord {
        id: "a".into(),
        state: Some(state),
        cooldown_until_ms: Some(until),
        disabled_reason: reason,
        ..Default::default()
    }];
    let p2 = Pool::new(
        &recs,
        Options {
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    );
    assert_eq!(p2.state_of("a"), State::Cooldown);
    assert_eq!(p2.cooldown_until_of("a"), 120_000);
}

#[test]
fn pool_015_selection_prefers_idle() {
    // TDD vector: A warm (2/2 inflight, low EWMA), B idle (higher EWMA)
    // → Acquire picks B. Fill A's slots (steer with exclude_ids) first.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a", "b"], &clock);
    // A: warm — two leases at full capacity, low EWMA.
    let la1 = p.try_acquire(&["b"]).unwrap();
    assert_eq!(la1.id, "a");
    let la2 = p.try_acquire(&["b"]).unwrap();
    assert_eq!(la2.id, "a"); // A now 2/2
    p.record_first_byte("a", Duration::from_millis(10)); // low EWMA
                                                         // B: idle but high EWMA; A full: only B is a candidate.
    p.record_first_byte("b", Duration::from_millis(2000)); // 2s EWMA
    let l3 = p.try_acquire(&[]).unwrap();
    assert_eq!(l3.id, "b");
    // A never oversubscribed across the whole sequence.
    assert_eq!(p.in_flight(), 3);
    l3.release();
    la2.release();
    la1.release();
}

#[tokio::test]
async fn pool_016_cancel_releases_slot() {
    // Cancel a waiting acquire: no slot leaks; repeat concurrently.
    let clock = FakeClock::new(0);
    let p = Pool::new(
        &recs(&["a"]),
        Options {
            max_inflight: 1,
            queue_wait: Duration::from_secs(5),
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    );
    let holder = p.try_acquire(&[]).unwrap();
    for _ in 0..100 {
        let p2 = p.clone();
        // 20ms: cancelled, queue-wait 5s not reached.
        let _ = tokio::time::timeout(Duration::from_millis(20), p2.acquire(&[])).await;
    }
    holder.release();
    assert_eq!(p.in_flight(), 0);
    assert!(p.try_acquire(&[]).is_ok());
}

// ---- POOLATTR-* (attribution) ----

#[test]
fn poolattr_001_transport_has_own_ladder() {
    // 3 consecutive 5xx (5xx ladder at 120s) → ReportTransport → 15s.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    for _ in 0..3 {
        p.report_5xx("a");
    }
    let before = p.cooldown_until_of("a");
    clock.set(before + 1);
    p.report_transport("a");
    assert_eq!(p.cooldown_until_of("a") - (before + 1), 15_000);
    let f = p.failures_of("a");
    assert_eq!(f[&FailureClass::Transport], 1);
    assert_eq!(f[&FailureClass::Upstream5xx], 3);
}

#[test]
fn poolattr_002_counters_per_class() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_5xx("a");
    clock.set(1000);
    p.report_transport("a");
    clock.set(2000);
    p.report_429("a");
    let f = p.failures_of("a");
    for c in FailureClass::ALL {
        assert_eq!(f[&c], 1, "{:?} counter", c);
    }
}

#[test]
fn poolattr_005_existing_ladders_unchanged() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    // 429: 60→120→240 (clock set to i, so until - i == step).
    for (i, &want) in [60_000, 120_000, 240_000].iter().enumerate() {
        clock.set(i as i64);
        p.report_429("a");
        assert_eq!(p.cooldown_until_of("a") - i as i64, want);
    }
    // 5xx: 30→60→120.
    let clock2 = FakeClock::new(0);
    let p2 = pool_with(&["a"], &clock2);
    for (i, &want) in [30_000, 60_000, 120_000].iter().enumerate() {
        clock2.set(i as i64);
        p2.report_5xx("a");
        assert_eq!(p2.cooldown_until_of("a") - i as i64, want);
    }
}

#[test]
fn poolattr_006_transport_ladder_own_cap() {
    // Six transports: 15→30→60→120→240→300, capped at 5m.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    let expected = [15_000, 30_000, 60_000, 120_000, 240_000, 300_000];
    let mut now = 0i64;
    for (i, &want) in expected.iter().enumerate() {
        p.report_transport("a");
        assert_eq!(p.cooldown_until_of("a") - now, want, "step {i}");
        now += 1;
        clock.set(now);
        now += want;
        clock.set(now);
    }
}

#[test]
fn poolattr_008_success_clears_every_class() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_5xx("a");
    clock.set(1000);
    p.report_transport("a");
    clock.set(2000);
    p.report_429("a");
    p.report_success("a");
    for c in FailureClass::ALL {
        assert_eq!(p.failures_of("a")[&c], 0);
    }
}

#[test]
fn poolattr_009_reset_failures_keeps_cooldown() {
    // Renewal path: counters cleared, cooldown_until unchanged.
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_429("a");
    p.report_429("a");
    let until = p.cooldown_until_of("a");
    p.reset_failures("a");
    assert_eq!(p.cooldown_until_of("a"), until);
    for c in FailureClass::ALL {
        assert_eq!(p.failures_of("a")[&c], 0);
    }
}

#[test]
fn poolattr_010_cooldown_records_its_cause() {
    // Class + status + timestamp recorded; latest cause wins.
    let clock = FakeClock::new(1000);
    let p = pool_with(&["a"], &clock);
    p.report_5xx("a"); // status 0
    let (class, status, since) = p.cooldown_info_of("a").unwrap();
    assert_eq!(class, FailureClass::Upstream5xx);
    assert_eq!(status, 0);
    assert_eq!(since, 1000);
    clock.set(5000);
    p.report_429("a"); // status 429
    let (class, status, since) = p.cooldown_info_of("a").unwrap();
    assert_eq!(class, FailureClass::Throttle429);
    assert_eq!(status, 429);
    assert_eq!(since, 5000);
}

#[test]
fn poolattr_011_ladder_ignores_upstream_retry_after() {
    // report_failure(429) has no duration input at all: cooldown is
    // exactly the ladder's 60s. (The API surface accepts no upstream
    // duration — this test pins that by construction.)
    let clock = FakeClock::new(7);
    let p = pool_with(&["a"], &clock);
    p.report_failure("a", FailureClass::Throttle429, 429);
    assert_eq!(p.cooldown_until_of("a") - 7, 60_000);
}

// ---- RISK-* ----

#[test]
fn risk_001_461_recorded_not_just_acted_on() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_risk_control(
        "a",
        "restricted",
        RiskEvent {
            model: Some("mimo-v2.6-pro".into()),
            channel: Some("chat".into()),
            status: 461,
            body_digest: Some("digest123".into()),
            ..Default::default()
        },
    );
    assert_eq!(p.state_of("a"), State::Disabled);
    let evs = p.risk_events();
    assert_eq!(evs.len(), 1);
    assert_eq!(evs[0].status, 461);
    assert_eq!(evs[0].model.as_deref(), Some("mimo-v2.6-pro"));
    assert_eq!(evs[0].body_digest.as_deref(), Some("digest123"));
    assert_eq!(evs[0].egress, "direct");
}

#[test]
fn risk_002_461_not_permanent() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_risk_control(
        "a",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    assert!(!p.permanent_of("a"));
}

#[test]
fn risk_004_same_egress_correlation() {
    // Two accounts labelled hk1 both hit 461 inside the window →
    // count 2; another-egress account not counted; old signals stop
    // correlating but stay recorded.
    let clock = FakeClock::new(0);
    let recs = vec![
        AccountRecord {
            id: "a".into(),
            egress: "hk1".into(),
            ..Default::default()
        },
        AccountRecord {
            id: "b".into(),
            egress: "hk1".into(),
            ..Default::default()
        },
        AccountRecord {
            id: "c".into(),
            egress: "sg1".into(),
            ..Default::default()
        },
    ];
    let p = Pool::new(
        &recs,
        Options {
            now: Some(clock.clock()),
            ..Default::default()
        },
        None,
        None,
    );
    p.report_risk_control(
        "a",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    let n = p.report_risk_control(
        "b",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    assert_eq!(n, 2);
    let n = p.report_risk_control(
        "c",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    assert_eq!(n, 1); // sg1: only c
                      // Signals older than the window (5m) stop correlating.
    clock.set(400_000); // 6m40s later
    let n = p.report_risk_control(
        "a",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    assert_eq!(n, 1); // only the fresh "a" signal
                      // But all events stay recorded (evidence).
    assert_eq!(p.risk_events().len(), 4);
}

#[test]
fn risk_005_451_same_path() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_risk_control(
        "a",
        "restricted",
        RiskEvent {
            status: 451,
            ..Default::default()
        },
    );
    assert_eq!(p.state_of("a"), State::Disabled);
    assert!(!p.permanent_of("a"));
    assert_eq!(p.risk_events().len(), 1);
}

#[test]
fn risk_006_operator_undo_keeps_evidence() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    p.report_risk_control(
        "a",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    p.enable("a");
    assert_eq!(p.state_of("a"), State::Available);
    assert!(p.try_acquire(&[]).is_ok());
    assert_eq!(p.risk_events().len(), 1); // evidence stays
}

#[test]
fn risk_007_unknown_account_still_records() {
    let clock = FakeClock::new(0);
    let p = pool_with(&["a"], &clock);
    let n = p.report_risk_control(
        "ghost",
        "r",
        RiskEvent {
            status: 461,
            ..Default::default()
        },
    );
    assert_eq!(n, 1);
    assert_eq!(p.risk_events().len(), 1);
    assert_eq!(p.risk_events()[0].egress, "direct");
}

// ---- Reaper / lease semantics (ADR 0019/0036) ----

#[test]
fn lease_touch_moves_expiry() {
    // A touched lease is never reaped; a silent one is.
    let clock = FakeClock::new(0);
    let p = Pool::new(
        &recs(&["a"]),
        Options {
            now: Some(clock.clock()),
            slot_ttl: Duration::from_millis(50),
            ..Default::default()
        },
        None,
        None,
    );
    let l = p.try_acquire(&[]).unwrap();
    std::thread::sleep(Duration::from_millis(70));
    assert!(l.silent_for(Instant::now()) > l.ttl());
    l.touch(); // upstream byte: silent_for resets
    assert!(l.silent_for(Instant::now()) < l.ttl());
    l.release();
}

#[test]
fn reaper_releases_silent_slot() {
    let clock = FakeClock::new(0);
    let p = Pool::new(
        &recs(&["a"]),
        Options {
            now: Some(clock.clock()),
            slot_ttl: Duration::from_millis(30),
            ..Default::default()
        },
        None,
        None,
    );
    let l = p.try_acquire(&[]).unwrap();
    let armed = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let armed_c = armed.clone();
    l.arm_expiry(Box::new(move || {
        armed_c.store(true, Ordering::SeqCst);
    }));
    assert_eq!(p.in_flight(), 1);
    std::thread::sleep(Duration::from_millis(60));
    assert_eq!(p.reap_expired_slots(), 1);
    assert!(armed.load(Ordering::SeqCst));
    assert_eq!(p.in_flight(), 0);
    // Lease already released: idempotent.
    l.release();
    assert_eq!(p.in_flight(), 0);
}

#[test]
fn ewma_first_byte_selection() {
    // Slow first byte → higher EWMA → B preferred (gap beyond band slack).
    let clock = FakeClock::new(0);
    let p = pool_with(&["a", "b"], &clock);
    for _ in 0..10 {
        p.record_first_byte("a", Duration::from_secs(20)); // ewma → 20s
        p.record_first_byte("b", Duration::from_millis(50)); // ewma → 0.05
    }
    // b's score 0.05 vs a's 20: far outside the band — b wins every time.
    for _ in 0..6 {
        let l = p.try_acquire(&[]).unwrap();
        assert_eq!(l.id, "b");
        l.release();
    }
}
