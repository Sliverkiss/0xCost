//! Live seeding for SDK construction (R1-W2a): the Upstash account row
//! carries a *live session* (cookie/ph/pass_token) that the Go env
//! seeding family (`MIMO_ACCOUNT_n_*`, store.rs `seed_from_env`) cannot
//! express — it covers phone/password/egress only. This module maps the
//! session-bearing row onto the store columns after env seeding.
//!
//! The row never persists to any file: the operator's script fetches it
//! via Upstash REST and exports `MIMO_ACCOUNT_1_*` +
//! `MIMO_RUST_SESSION_1_*` env vars (scripts/live-seed-mimo.sh); the
//! host constructs the plugin with both families in the environment.
//! Only `MIMO_ACCOUNT_1_*` existing signals "a live account is injected
//! — register the plugin" (mirrors the tokenharbor gate: no env, no
//! registration, no probe).

use crate::store;

/// Apply the `MIMO_RUST_SESSION_n_*` env family onto the store after the
/// normal account seeding: user_id/pass_token/cookie/ph/device_json map
/// one-to-one onto the account row's columns (Go `Account` record).
/// A partial session still lands — the COALESCE/NULLIF update keeps
/// existing values when a field is absent (same discipline as
/// `Store::apply_session`). Returns the ids a session was applied to.
pub fn seed_session_from_env(
    store: &store::Store,
    vars: &dyn Fn(&str) -> Option<String>,
) -> rusqlite::Result<Vec<String>> {
    let mut applied = Vec::new();
    let mut n = 1;
    loop {
        // n tracks the MIMO_ACCOUNT_n family (sessions pair with accounts).
        let id = match vars(&format!("MIMO_ACCOUNT_{n}_ID")) {
            Some(id) => id,
            None => match vars(&format!("MIMO_ACCOUNT_{n}_PHONE")) {
                // The account family may omit explicit ids (env-{n} default).
                Some(_) => format!("env-{n}"),
                None => break,
            },
        };
        if apply_session_vars(store, &id, n, vars)? {
            applied.push(id);
        }
        n += 1;
    }
    Ok(applied)
}

/// One account's session vars → store columns. Returns whether anything
/// was written (all fields absent → no SQL touched).
fn apply_session_vars(
    store: &store::Store,
    id: &str,
    n: usize,
    vars: &dyn Fn(&str) -> Option<String>,
) -> rusqlite::Result<bool> {
    let get = |k: &str| vars(&format!("MIMO_RUST_SESSION_{n}_{k}"));
    let user_id = get("USER_ID");
    let pass_token = get("PASS_TOKEN");
    let cookie = get("COOKIE");
    let ph = get("PH");
    let device_json = get("DEVICE");
    if user_id.is_none()
        && pass_token.is_none()
        && cookie.is_none()
        && ph.is_none()
        && device_json.is_none()
    {
        return Ok(false);
    }
    // Store::apply_session's COALESCE/NULLIF update keeps existing
    // values when a field is absent, and stamps session_fetched_at.
    store.apply_session(
        id,
        user_id.as_deref(),
        pass_token.as_deref(),
        cookie.as_deref(),
        ph.as_deref(),
        device_json.as_deref(),
    )?;
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::{Store, StoredAccount};

    fn account(id: &str) -> StoredAccount {
        StoredAccount {
            id: id.into(),
            phone: Some("18800000001".into()),
            password: Some("pw".into()),
            enabled: true,
            egress: "direct".into(),
            created_at: 0,
            updated_at: 0,
            ..Default::default()
        }
    }

    /// Redacted-shape fixture mirroring the Upstash row (task R1-W2a):
    /// synthetic values only.
    fn upstash_row_env(n: usize, with_session: bool) -> Vec<(String, String)> {
        let mut v = vec![
            (format!("MIMO_ACCOUNT_{n}_ID"), "18800000001".into()),
            (format!("MIMO_ACCOUNT_{n}_PHONE"), "18800000001".into()),
            (format!("MIMO_ACCOUNT_{n}_PASSWORD"), "fixture-pw".into()),
        ];
        if with_session {
            v.extend([
                (
                    format!("MIMO_RUST_SESSION_{n}_USER_ID"),
                    "2650000000".into(),
                ),
                (
                    format!("MIMO_RUST_SESSION_{n}_PASS_TOKEN"),
                    "V1:fixture-token".into(),
                ),
                (
                    format!("MIMO_RUST_SESSION_{n}_COOKIE"),
                    "cUserId=c; userId=2650000000; serviceToken=st".into(),
                ),
                (format!("MIMO_RUST_SESSION_{n}_PH"), "ph==".into()),
                (
                    format!("MIMO_RUST_SESSION_{n}_DEVICE"),
                    r#"{"deviceId":"wb-fixture","pass_ua":"web"}"#.into(),
                ),
            ]);
        }
        v
    }

    fn vars_of(pairs: &[(String, String)]) -> impl Fn(&str) -> Option<String> + '_ {
        move |k: &str| {
            pairs
                .iter()
                .find(|(key, _)| key == k)
                .map(|(_, v)| v.clone())
        }
    }

    #[test]
    fn full_row_maps_all_columns() {
        let store = Store::open_in_memory().unwrap();
        store.upsert_account(&account("18800000001")).unwrap();
        let pairs = upstash_row_env(1, true);
        let applied = seed_session_from_env(&store, &vars_of(&pairs)).unwrap();
        assert_eq!(applied, vec!["18800000001"]);
        let a = &store.load_accounts().unwrap()[0];
        assert_eq!(a.user_id.as_deref(), Some("2650000000"));
        assert_eq!(a.pass_token.as_deref(), Some("V1:fixture-token"));
        assert!(a.cookie.as_deref().unwrap().contains("serviceToken=st"));
        assert_eq!(a.ph.as_deref(), Some("ph=="));
        assert_eq!(
            a.device_json.as_deref(),
            Some(r#"{"deviceId":"wb-fixture","pass_ua":"web"}"#)
        );
        assert!(a.session_fetched_at.unwrap() > 1_700_000_000);
    }

    #[test]
    fn row_without_session_writes_nothing() {
        let store = Store::open_in_memory().unwrap();
        store.upsert_account(&account("18800000001")).unwrap();
        let pairs = upstash_row_env(1, false);
        let applied = seed_session_from_env(&store, &vars_of(&pairs)).unwrap();
        assert!(applied.is_empty());
        let a = &store.load_accounts().unwrap()[0];
        assert!(a.cookie.is_none());
        assert!(a.pass_token.is_none());
        // No session write happened: the account row is untouched.
        assert_eq!(a.session_fetched_at, None);
    }

    #[test]
    fn partial_session_keeps_stored_values() {
        // Only pass_token+user_id present (renewal creds, no cookie):
        // the other columns must not be wiped.
        let store = Store::open_in_memory().unwrap();
        let mut acct = account("18800000001");
        acct.cookie = Some("existing-cookie".into());
        acct.ph = Some("existing-ph".into());
        store.upsert_account(&acct).unwrap();
        let pairs = vec![
            ("MIMO_ACCOUNT_1_ID".to_string(), "18800000001".into()),
            ("MIMO_ACCOUNT_1_PHONE".to_string(), "18800000001".into()),
            (
                "MIMO_RUST_SESSION_1_USER_ID".to_string(),
                "2650000000".into(),
            ),
            (
                "MIMO_RUST_SESSION_1_PASS_TOKEN".to_string(),
                "V1:fixture-token".into(),
            ),
        ];
        seed_session_from_env(&store, &vars_of(&pairs)).unwrap();
        let a = &store.load_accounts().unwrap()[0];
        assert_eq!(a.cookie.as_deref(), Some("existing-cookie"));
        assert_eq!(a.ph.as_deref(), Some("existing-ph"));
        assert_eq!(a.user_id.as_deref(), Some("2650000000"));
    }

    #[test]
    fn env_n_default_id_is_matched() {
        // No explicit ID var: the env-{n} default id still receives the
        // session (mirrors seed_from_env's id defaulting).
        let store = Store::open_in_memory().unwrap();
        store.upsert_account(&account("env-1")).unwrap();
        let pairs = vec![
            ("MIMO_ACCOUNT_1_PHONE".to_string(), "18800000001".into()),
            ("MIMO_RUST_SESSION_1_PH".to_string(), "ph==".into()),
        ];
        seed_session_from_env(&store, &vars_of(&pairs)).unwrap();
        let a = &store.load_accounts().unwrap()[0];
        assert_eq!(a.ph.as_deref(), Some("ph=="));
    }
}
