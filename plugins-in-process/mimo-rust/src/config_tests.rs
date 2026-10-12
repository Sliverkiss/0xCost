//! config tests: the env contract resolution (defaults vs overrides,
//! boolean parsing, egress discovery).

use std::collections::HashMap;
use std::time::Duration;

use super::Config;

fn env<'a>(pairs: &'a [(&'a str, &'a str)]) -> impl Fn(&str) -> Option<String> + 'a {
    let m: HashMap<String, String> = pairs
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
    move |k| m.get(k).cloned()
}

#[test]
fn defaults_match_go_parity() {
    let c = Config::from_env(&|_| None);
    assert_eq!(c.db_path, std::path::PathBuf::from("mimo-rust.db"));
    assert_eq!(c.base, "https://aistudio.xiaomimimo.com");
    assert_eq!(c.default_model, "mimo-v2.6-pro");
    assert!(c.thinking_default_on);
    // Login behavior: OTP auto off, consent auto on (Go defaults).
    assert!(c.login.disable_otp_auto);
    assert!(!c.login.disable_cross_region_agree);
    // Pool defaults come from PoolOptions::default().
    assert_eq!(c.pool.max_inflight, 2);
    assert_eq!(c.pool.queue_wait, Duration::from_secs(15));
    assert_eq!(c.pool.slot_ttl, Duration::from_secs(120));
    assert!(c.egress.is_empty());
}

#[test]
fn overrides_apply() {
    let c = Config::from_env(&env(&[
        ("MIMO_RUST_DB_PATH", "/tmp/x.db"),
        ("MIMO_RUST_UPSTREAM_BASE", "http://mock:1"),
        ("MIMO_RUST_DEFAULT_MODEL", "mimo-v2.6-flash"),
        ("MIMO_RUST_USER_AGENT", "UA"),
        ("MIMO_RUST_SSE_IDLE_TIMEOUT_S", "5"),
        ("MIMO_RUST_MAX_INFLIGHT_PER_ACCOUNT", "4"),
        ("MIMO_RUST_QUEUE_WAIT_S", "7"),
        ("MIMO_RUST_COOLDOWN_429_S", "30"),
        ("MIMO_RUST_LEASE_TTL_S", "0"),
    ]));
    assert_eq!(c.db_path, std::path::PathBuf::from("/tmp/x.db"));
    assert_eq!(c.base, "http://mock:1");
    assert_eq!(c.default_model, "mimo-v2.6-flash");
    assert_eq!(c.login.user_agent.as_deref(), Some("UA"));
    assert_eq!(c.login.idle_timeout, Some(Duration::from_secs(5)));
    assert_eq!(c.pool.max_inflight, 4);
    assert_eq!(c.pool.queue_wait, Duration::from_secs(7));
    assert_eq!(c.pool.cooldown_429, Duration::from_secs(30));
    assert_eq!(c.pool.slot_ttl, Duration::from_secs(0));
}

#[test]
fn boolean_parsing_is_forgiving_but_explicit() {
    // "true"/"1" (either case) → true; "false"/"0" → false; junk/absent
    // → default.
    let c = Config::from_env(&env(&[
        ("MIMO_RUST_OTP_AUTO", "true"),
        ("MIMO_RUST_CROSS_REGION_AGREE", "0"),
        ("MIMO_RUST_THINKING_DEFAULT_ON", "junk"),
    ]));
    assert!(!c.login.disable_otp_auto); // OTP auto on
    assert!(c.login.disable_cross_region_agree); // consent off
    assert!(c.thinking_default_on); // junk → default (true)
}

#[test]
fn egress_labels_are_declared_not_guessed() {
    // Labels come from MIMO_RUST_EGRESS_LABELS; each label's proxy from
    // MIMO_RUST_EGRESS_<LABEL>. A bad URL for one label drops only that
    // label (the client falls back to direct for it).
    let c = Config::from_env(&env(&[
        ("MIMO_RUST_EGRESS_LABELS", "jp, sg ,"),
        ("MIMO_RUST_EGRESS_JP", "http://proxy.jp:8080"),
        ("MIMO_RUST_EGRESS_SG", "not a url"),
    ]));
    assert_eq!(c.egress.len(), 1);
    assert_eq!(
        c.egress.get("jp").unwrap().as_str(),
        "http://proxy.jp:8080/"
    );
    // The login client gets the same map (per-egress proxied clients).
    assert_eq!(c.login.egress.len(), 1);
}
