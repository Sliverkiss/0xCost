//! Seed import — PC-41: parse Upstash REST GET replies into [`Account`]
//! records and upsert them into the SQLite store. Read-only by design:
//! the only upstream verbs are GET `/get/simple-chat:accounts:index`
//! (SMEMBERS) and GET `/get/simple-chat:accounts:<identity>` per member;
//! there is no code path that SETs or DELs anything in Redis.
//!
//! The wire forms live here (not in the CLI) so fixture tests run offline;
//! the `sc-seed` binary is a thin wrapper. Live mode requires
//! `SC_LIVE_UPSTASH=1` and never runs in gates.

use std::time::SystemTime;

use serde::Deserialize;

use crate::account::Account;
use crate::store::{ParkRecord, SqliteStore, StoreError};

/// Upstash REST GET reply: `{"result": <value>}` where `<value>` is the
/// decoded string, a JSON array of strings (SMEMBERS), or `null`.
#[derive(Debug, Deserialize)]
struct UpstashReply<T> {
    result: Option<T>,
}

/// Decode the REST GET reply for one account key: `{"result": "<json>"}` or
/// `{"result": null}` (key absent — index ahead of data, skip).
pub fn parse_account_reply(body: &str) -> Result<Option<Account>, SeedError> {
    let reply: UpstashReply<String> = serde_json::from_str(body)?;
    let Some(raw) = reply.result else {
        return Ok(None);
    };
    let acct: Account = serde_json::from_str(&raw)?;
    Ok(Some(acct))
}

/// Decode the REST GET reply for the index set (SMEMBERS).
pub fn parse_index_reply(body: &str) -> Result<Vec<String>, SeedError> {
    let reply: UpstashReply<Vec<String>> = serde_json::from_str(body)?;
    Ok(reply.result.unwrap_or_default())
}

/// Import errors: malformed replies or invalid account payloads. A record
/// that fails Go's validation is skipped with a reason (one bad row must
/// not take the pool down — `redis.go` Load), and reported in the summary.
#[derive(Debug, thiserror::Error)]
pub enum SeedError {
    #[error("invalid Upstash reply: {0}")]
    BadReply(#[from] serde_json::Error),
    #[error("sqlite: {0}")]
    Store(#[from] StoreError),
}

/// Outcome of one import pass (reported to the operator).
#[derive(Debug, Default, PartialEq)]
pub struct ImportSummary {
    pub imported: usize,
    pub skipped: Vec<(String, String)>, // (identity/ key, reason)
    pub parked: usize,                  // records that arrived with a live park
}

/// Upsert fixture- or live-fetched records into the store. Each record is
/// validated (Go `Account.Validate` semantics: identity, password, region,
/// channel, web⇒device_id); invalid rows are skipped with the reason, not
/// fatal. Park state rides the record verbatim — the final Upstash export
/// (dossier §2.3 step 4) is the reconciliation moment.
pub fn import_records(
    store: &SqliteStore,
    records: Vec<Account>,
    now: SystemTime,
) -> Result<ImportSummary, SeedError> {
    let mut summary = ImportSummary::default();
    for acct in records {
        let identity = acct.identity().to_string();
        if acct.identity().is_empty() {
            summary.skipped.push(("".into(), "no identity".into()));
            continue;
        }
        if let Err(e) = acct.validate() {
            summary.skipped.push((identity.clone(), e.to_string()));
            continue;
        }
        if !acct.park_kind.is_empty() {
            summary.parked += 1;
        }
        store.save_account(&acct)?;
        summary.imported += 1;
    }
    let _ = now; // park reconciliation happens at pool boot, not here
    Ok(summary)
}

/// The R1 concurrency warning printed on import (dossier §2.3): the plugin
/// assumes EXCLUSIVE ownership of these accounts; the Go binary must be
/// stopped before cutover, and this import does not stop it.
pub const CONCURRENCY_WARNING: &str = "\
WARNING: exclusive ownership (R1). This plugin assumes it is the ONLY process \
operating these upstream accounts. Importing while the Go binary still serves \
will fork park state (its Upstash writes vs. this SQLite) — divergence is \
unrecoverable. Cutover order (dossier §2.3): stop Go, re-export park state, \
then start the plugin.";

/// Park-record conversion for the §2.3 step-4 reconciliation (re-apply the
/// final Upstash park state onto SQLite rows): a persisted record's park
/// fields become one unfenced transition, preserving unknown-deadline
/// (kind muted/risk with empty/unparseable until ⇒ `None`).
pub fn park_record_from_account(acct: &Account) -> Option<ParkRecord> {
    let kind = crate::account::BanState::parse(&acct.park_kind);
    if kind == crate::account::BanState::None {
        return None;
    }
    let until = crate::timeutil::parse_rfc3339(&acct.park_until);
    Some(ParkRecord {
        identity: acct.identity().to_string(),
        kind,
        until,
        reason: acct.park_reason.clone(),
        generation: 0, // unfenced: the operator drives this reconciliation
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn now() -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000)
    }

    #[test]
    fn parses_account_reply_go_shaped() {
        // PC-41 fixture: a real Upstash GET reply wrapping a Go Account JSON.
        let body = r#"{"result":"{\"mobile\":\"13800001111\",\"email\":\"\",\"password\":\"hunter2\",\"region\":\"\",\"channel\":\"web\",\"device_id\":\"B-shumei+abc\",\"park_kind\":\"muted\",\"park_until\":\"2026-10-13T00:00:00Z\",\"park_reason\":\"biz 10005\"}"}"#;
        let acct = parse_account_reply(body).unwrap().unwrap();
        assert_eq!(acct.mobile, "13800001111");
        assert_eq!(acct.channel, "web");
        assert_eq!(acct.park_kind, "muted");
        assert_eq!(acct.park_until, "2026-10-13T00:00:00Z");
        // Absent key: null result.
        assert!(parse_account_reply("{\"result\":null}").unwrap().is_none());
    }

    #[test]
    fn parses_index_reply() {
        let body = r#"{"result":["13800001111","a@b.io"]}"#;
        assert_eq!(
            parse_index_reply(body).unwrap(),
            vec!["13800001111", "a@b.io"]
        );
        assert!(parse_index_reply("{\"result\":null}").unwrap().is_empty());
        assert!(parse_index_reply("{}").is_ok());
    }

    #[test]
    fn import_round_trips_into_sqlite_with_park_intact() {
        // PC-41 [U]: every member parses and round-trips into SQLite with
        // park state intact.
        let store = SqliteStore::open_in_memory().unwrap();
        let raw = [
            r#"{"mobile":"138","password":"p1"}"#,
            r#"{"mobile":"139","password":"p2","park_kind":"banned"}"#,
            r#"{"email":"a@b.io","password":"p3","channel":"web","device_id":"B-x"}"#,
            // Invalid rows: skipped, never fatal.
            r#"{"mobile":"136"}"#,                        // no password
            r#"{"mobile":"","email":"","password":"p"}"#, // no identity
            r#"{"mobile":"135","password":"p","region":"mars"}"#,
        ];
        let records: Vec<Account> = raw
            .iter()
            .map(|r| serde_json::from_str(r).unwrap())
            .collect();
        let summary = import_records(&store, records, now()).unwrap();
        assert_eq!(summary.imported, 3);
        assert_eq!(summary.parked, 1);
        assert_eq!(summary.skipped.len(), 3);
        let loaded = store.load(now()).unwrap();
        assert_eq!(loaded.len(), 3);
        let banned = loaded.iter().find(|a| a.mobile == "139").unwrap();
        assert_eq!(banned.park_kind, "banned");
    }

    #[test]
    fn live_park_reconciliation_converts_records() {
        let store = SqliteStore::open_in_memory().unwrap();
        let acct: Account = serde_json::from_str(
            r#"{"mobile":"138","password":"p","park_kind":"muted","park_until":"2999-01-01T00:00:00Z","park_reason":"r"}"#,
        )
        .unwrap();
        import_records(&store, vec![acct.clone()], now()).unwrap();
        // §2.3 step 4: re-apply park state — the record's park is already on
        // the row; converting it yields the same state (idempotent).
        let rec = park_record_from_account(&acct).unwrap();
        store.apply_park(&rec, now());
        let loaded = store.load(now()).unwrap();
        assert_eq!(loaded[0].park_kind, "muted");
        assert_eq!(loaded[0].park_until, "2999-01-01T00:00:00Z");
        // Unknown-deadline park: until = None (quarantine preserved).
        let unknown: Account = serde_json::from_str(
            r#"{"mobile":"137","password":"p","park_kind":"muted","park_reason":"r"}"#,
        )
        .unwrap();
        let rec = park_record_from_account(&unknown).unwrap();
        assert_eq!(rec.until, None);
        // Unparked record: no transition.
        let clean: Account = serde_json::from_str(r#"{"mobile":"136","password":"p"}"#).unwrap();
        assert!(park_record_from_account(&clean).is_none());
    }

    #[test]
    fn concurrency_warning_is_printed_text() {
        assert!(CONCURRENCY_WARNING.to_lowercase().contains("exclusive"));
        assert!(CONCURRENCY_WARNING.contains("stop Go"));
    }
}
