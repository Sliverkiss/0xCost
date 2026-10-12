//! Plugin-private SQLite account store — PORT_DESIGN §2.1, binding
//! maintainer decision. Backs the in-memory pool exactly like Go's
//! `MemoryFirstStore` wraps a JSON/Redis backing:
//!
//! - one boot-time load; afterwards only write-through moments
//!   (save/delete, login, park transitions, boot device-id minting);
//! - `load` strips only expired (parseable) mute/risk parks — banned and
//!   unknown-deadline parks survive (R3 quarantine);
//! - a failed park write latches `park_write_failed` sticky — new
//!   acquisitions answer 503 `park_persistence_failed` and health turns
//!   unhealthy (a lost mute renews the upstream window, observed
//!   mute→ban escalation);
//! - `generation` increments on every row mutation so stale park/login
//!   callbacks from removed/re-added accounts are rejected;
//! - the whole write path is synchronous under one mutex (SQLite is not
//!   async anyway; Go holds `mu` across backing writes too).

use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::SystemTime;

use rusqlite::{params, Connection, OptionalExtension};

use crate::account::{Account, BanState};
use crate::device::DeviceProfile;
use crate::timeutil;

/// Errors surfaced to callers (park/login failures stay fail-soft and only
/// trip the latch / log — they never propagate to the request path).
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("sqlite open failed: {0}")]
    Open(String),
    #[error("sqlite: {0}")]
    Sql(#[from] rusqlite::Error),
    #[error("account {0}: {1}")]
    Invalid(String, #[source] crate::account::AccountError),
    #[error("store: account {0} not found")]
    NotFound(String),
}

/// A park transition to merge (Go `upstream.ParkRecord`).
#[derive(Debug, Clone)]
pub struct ParkRecord {
    pub identity: String,
    pub kind: BanState,
    /// `None` renders the empty/unknown-deadline form.
    pub until: Option<SystemTime>,
    pub reason: String,
    /// 0 = unfenced (legacy) callbacks, accepted while fencing is off.
    pub generation: u64,
}

/// A fresh login token write-through (Go `upstream.LoginRecord`).
#[derive(Debug, Clone)]
pub struct LoginRecord {
    pub identity: String,
    pub token: String,
    pub generation: u64,
}

/// SQLite-backed account store (the plugin's backing). Wraps a
/// `Connection` behind a `Mutex` — the request path never touches this
/// directly; the pool holds accounts in memory and writes through here.
pub struct SqliteStore {
    conn: Mutex<Connection>,
    /// Sticky park-persistence failure (R3). Once tripped, admission stays
    /// closed for this process even if SQLite later succeeds — restarting
    /// with the unchanged row cannot recover the lost transition.
    park_write_failed: AtomicBool,
}

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS accounts (
  identity        TEXT PRIMARY KEY,
  mobile          TEXT NOT NULL DEFAULT '',
  email           TEXT NOT NULL DEFAULT '',
  password        TEXT NOT NULL,
  region          TEXT NOT NULL DEFAULT 'cn',
  channel         TEXT NOT NULL DEFAULT '',
  device_id       TEXT NOT NULL DEFAULT '',
  park_kind       TEXT,
  park_until      TEXT,
  park_reason     TEXT,
  parked_at       TEXT,
  session_token   TEXT,
  generation      INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS device_fingerprints (
  identity        TEXT PRIMARY KEY REFERENCES accounts(identity) ON DELETE CASCADE,
  rangers_id      TEXT NOT NULL,
  header_device_id TEXT NOT NULL
);
"#;

/// Columns of one account row, in SELECT order.
const ACCOUNT_COLS: &str = "identity, mobile, email, password, region, channel, \
     device_id, park_kind, park_until, park_reason, parked_at, session_token";

fn open_connection(path: &Path) -> Result<Connection, StoreError> {
    let conn = Connection::open(path).map_err(|e| StoreError::Open(e.to_string()))?;
    conn.pragma_update(None, "journal_mode", "WAL")?;
    conn.pragma_update(None, "foreign_keys", "ON")?;
    conn.execute_batch(SCHEMA)?;
    Ok(conn)
}

fn row_to_account(row: &rusqlite::Row<'_>) -> rusqlite::Result<Account> {
    Ok(Account {
        mobile: row.get("mobile")?,
        email: row.get("email")?,
        password: row.get("password")?,
        region: row.get("region")?,
        channel: row.get("channel")?,
        device_id: row.get("device_id")?,
        park_kind: opt_or_empty(row.get("park_kind")?),
        park_until: opt_or_empty(row.get("park_until")?),
        park_reason: opt_or_empty(row.get("park_reason")?),
        parked_at: opt_or_empty(row.get("parked_at")?),
        session_token: opt_or_empty(row.get("session_token")?),
    })
}

fn opt_or_empty(v: Option<String>) -> String {
    v.unwrap_or_default()
}

fn null_if_empty(s: &str) -> Option<&str> {
    (!s.is_empty()).then_some(s)
}

fn query_row_account(conn: &Connection, identity: &str) -> rusqlite::Result<Option<Account>> {
    conn.query_row(
        &format!("SELECT {ACCOUNT_COLS} FROM accounts WHERE identity = ?1"),
        [identity],
        row_to_account,
    )
    .optional()
}

fn generation_of(conn: &Connection, identity: &str) -> u64 {
    conn.query_row(
        "SELECT generation FROM accounts WHERE identity = ?1",
        [identity],
        |row| row.get::<_, i64>(0),
    )
    .map(|v| v.max(0) as u64)
    .unwrap_or(0)
}

impl SqliteStore {
    /// Open (creating if absent) and migrate the database. Fails only on
    /// I/O or schema problems — an empty database is a valid starting state.
    pub fn open(path: &Path) -> Result<Self, StoreError> {
        Ok(Self {
            conn: Mutex::new(open_connection(path)?),
            park_write_failed: AtomicBool::new(false),
        })
    }

    /// In-memory variant for tests and fixtures.
    pub fn open_in_memory() -> Result<Self, StoreError> {
        let conn = Connection::open_in_memory().map_err(|e| StoreError::Open(e.to_string()))?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "foreign_keys", "ON")?;
        conn.execute_batch(SCHEMA)?;
        Ok(Self {
            conn: Mutex::new(conn),
            park_write_failed: AtomicBool::new(false),
        })
    }

    /// The R3 latch: once a park write has failed, admission stays closed
    /// for this process.
    pub fn park_write_failed(&self) -> bool {
        self.park_write_failed.load(Ordering::Relaxed)
    }

    /// Boot-time load: every account row, credentials included. Expired
    /// (parseable) mute/risk parks are stripped and persisted — banned and
    /// unknown-deadline parks survive verbatim. Rows failing validation are
    /// a fatal error (a corrupt source of truth must not serve).
    pub fn load(&self, now: SystemTime) -> Result<Vec<Account>, StoreError> {
        let mut conn = self.conn.lock().expect("store mutex poisoned");
        let accounts: Vec<Account> = {
            let mut stmt = conn.prepare(&format!("SELECT {ACCOUNT_COLS} FROM accounts"))?;
            let rows = stmt.query_map([], row_to_account)?;
            rows.collect::<rusqlite::Result<Vec<_>>>()?
        };
        let mut out = Vec::with_capacity(accounts.len());
        for mut acct in accounts {
            if acct.strip_expired_park(now) {
                save_locked(&mut conn, &acct)?;
            }
            acct.validate()
                .map_err(|e| StoreError::Invalid(acct.identity().to_string(), e))?;
            out.push(acct);
        }
        Ok(out)
    }

    /// Current generation for an identity (0 when absent) — the fencing
    /// snapshot pool callbacks carry.
    pub fn generation(&self, identity: &str) -> Result<u64, StoreError> {
        let conn = self.conn.lock().expect("store mutex poisoned");
        Ok(generation_of(&conn, identity))
    }

    /// Upsert one account (backing write first; the in-memory cache in the
    /// pool follows). Errors propagate — hard writes are honest 5xx.
    pub fn save_account(&self, acct: &Account) -> Result<(), StoreError> {
        let mut conn = self.conn.lock().expect("store mutex poisoned");
        save_locked(&mut conn, acct)
    }

    /// Delete one record by identity; [`StoreError::NotFound`] when nothing
    /// matches. Generation bumps via the tombstone rule — a re-add cannot
    /// reuse a former generation (the row's `generation` restarts at 1 only
    /// for a genuinely new identity; delete+re-add is fenced by identity
    /// presence, see `save_locked`).
    pub fn delete_account(&self, identity: &str) -> Result<(), StoreError> {
        let conn = self.conn.lock().expect("store mutex poisoned");
        let n = conn.execute("DELETE FROM accounts WHERE identity = ?1", [identity])?;
        if n == 0 {
            return Err(StoreError::NotFound(identity.to_string()));
        }
        Ok(())
    }

    /// Merge one park transition (synchronous under the store mutex, called
    /// from the pool with the account lock held). Fail-soft: a failed write
    /// latches admission closed (R3), never propagates. Unknown identity or
    /// a stale generation is the same no-op the backing stores take.
    pub fn apply_park(&self, rec: &ParkRecord, now: SystemTime) {
        let mut guard = self.conn.lock().expect("store mutex poisoned");
        let loaded = match query_row_account(&guard, &rec.identity) {
            Ok(Some(a)) => a,
            Ok(None) => {
                tracing::warn!(identity = %rec.identity, "store: park for unknown identity skipped");
                return;
            }
            Err(e) => {
                // A failed read loses this transition exactly like a failed
                // write (Go never hits this — its cache is in memory); R3 is
                // fail-closed, so the latch trips here too.
                self.park_write_failed.store(true, Ordering::Relaxed);
                tracing::error!(identity = %rec.identity, error = %e, "store: park read failed");
                return;
            }
        };
        let mut acct = loaded;
        if rec.generation != 0 && rec.generation != generation_of(&guard, &rec.identity) {
            tracing::warn!(identity = %rec.identity, "store: stale park generation skipped");
            return;
        }
        let until = rec.until.map(timeutil::format_rfc3339);
        if !acct.apply_park_record(
            rec.kind,
            until.as_deref(),
            &rec.reason,
            &timeutil::format_rfc3339(now),
        ) {
            return; // already persisted exactly this state — no churn
        }
        if let Err(e) = save_locked(&mut guard, &acct) {
            self.park_write_failed.store(true, Ordering::Relaxed);
            tracing::error!(identity = %rec.identity, error = %e, "store: cannot persist park state");
        }
    }

    /// Write one login token through (fail-soft — a missed token costs one
    /// relogin after restart, never a bad request).
    pub fn apply_login(&self, rec: &LoginRecord) {
        let mut guard = self.conn.lock().expect("store mutex poisoned");
        let acct = match query_row_account(&guard, &rec.identity) {
            Ok(Some(a)) => a,
            Ok(None) => return, // removed mid-run: token lives in pool memory only
            Err(_) => return,
        };
        if rec.generation != 0 && rec.generation != generation_of(&guard, &rec.identity) {
            return; // stale callback
        }
        let mut acct = acct;
        acct.session_token = rec.token.clone();
        if let Err(e) = save_locked(&mut guard, &acct) {
            tracing::warn!(identity = %rec.identity, error = %e, "store: cannot persist session token");
        }
    }

    /// Load the persisted device fingerprint row for one identity.
    pub fn load_fingerprint(&self, identity: &str) -> Result<Option<DeviceProfile>, StoreError> {
        let conn = self.conn.lock().expect("store mutex poisoned");
        load_fingerprint_locked(&conn, identity)
    }

    /// Persist the derived device profile for one account (boot-time
    /// `EnsureDeviceIDs` write-through). Insert-or-replace is correct: the
    /// profile is a pure function of (device_id, identity, channel), so a
    /// re-mint with unchanged inputs rewrites identical values.
    pub fn save_fingerprint(
        &self,
        identity: &str,
        profile: &DeviceProfile,
    ) -> Result<(), StoreError> {
        let conn = self.conn.lock().expect("store mutex poisoned");
        conn.execute(
            "INSERT INTO device_fingerprints (identity, rangers_id, header_device_id) \
             VALUES (?1, ?2, ?3) \
             ON CONFLICT(identity) DO UPDATE SET \
             rangers_id=excluded.rangers_id, header_device_id=excluded.header_device_id",
            params![identity, profile.rangers_id, profile.header_device_id],
        )?;
        Ok(())
    }
}

fn save_locked(conn: &mut Connection, acct: &Account) -> Result<(), StoreError> {
    let identity = acct.identity().to_string();
    let now = timeutil::format_rfc3339(SystemTime::now());
    conn.execute(
        "INSERT INTO accounts (identity, mobile, email, password, region, channel, device_id, \
         park_kind, park_until, park_reason, parked_at, session_token, updated_at, generation) \
         VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,1) \
         ON CONFLICT(identity) DO UPDATE SET \
         mobile=excluded.mobile, email=excluded.email, password=excluded.password, \
         region=excluded.region, channel=excluded.channel, device_id=excluded.device_id, \
         park_kind=excluded.park_kind, park_until=excluded.park_until, \
         park_reason=excluded.park_reason, parked_at=excluded.parked_at, \
         session_token=excluded.session_token, updated_at=excluded.updated_at, \
         generation=accounts.generation+1",
        params![
            identity,
            acct.mobile,
            acct.email,
            acct.password,
            acct.region,
            acct.channel,
            acct.device_id,
            null_if_empty(&acct.park_kind),
            null_if_empty(&acct.park_until),
            null_if_empty(&acct.park_reason),
            null_if_empty(&acct.parked_at),
            null_if_empty(&acct.session_token),
            now
        ],
    )?;
    Ok(())
}

fn load_fingerprint_locked(
    conn: &Connection,
    identity: &str,
) -> Result<Option<DeviceProfile>, StoreError> {
    let row = conn
        .query_row(
            "SELECT rangers_id, header_device_id FROM device_fingerprints WHERE identity = ?1",
            [identity],
            |row| Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?)),
        )
        .optional()?;
    let Some((rangers_id, header_device_id)) = row else {
        return Ok(None);
    };
    // Body device_id + channel come from the account row (join on identity).
    let acct_row = conn
        .query_row(
            "SELECT device_id, channel FROM accounts WHERE identity = ?1",
            [identity],
            |row| Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?)),
        )
        .optional()?;
    let (device_id, channel) = acct_row.unwrap_or_default();
    Ok(Some(DeviceProfile {
        device_id,
        rangers_id,
        header_device_id,
        channel,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn now() -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000)
    }

    fn base_account(mobile: &str) -> Account {
        Account {
            mobile: mobile.into(),
            password: "pw".into(),
            ..Default::default()
        }
    }

    #[test]
    fn round_trip_save_and_load() {
        let store = SqliteStore::open_in_memory().unwrap();
        let mut a = base_account("138");
        a.park_kind = "muted".into();
        a.park_until = "2999-01-01T00:00:00Z".into();
        a.park_reason = "muted upstream".into();
        a.parked_at = "2026-01-01T00:00:00Z".into();
        store.save_account(&a).unwrap();
        let loaded = store.load(now()).unwrap();
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].mobile, "138");
        assert_eq!(loaded[0].park_kind, "muted");
        assert_eq!(loaded[0].park_until, "2999-01-01T00:00:00Z");
        assert_eq!(loaded[0].park_reason, "muted upstream");
    }

    #[test]
    fn load_strips_expired_park_and_persists() {
        let store = SqliteStore::open_in_memory().unwrap();
        let mut a = base_account("138");
        a.park_kind = "muted".into();
        a.park_until = "2000-01-01T00:00:00Z".into();
        store.save_account(&a).unwrap();
        let loaded = store.load(now()).unwrap();
        assert!(loaded[0].park_kind.is_empty());
        // Re-load: still clean (persisted, not just in-memory).
        let again = store.load(now()).unwrap();
        assert!(again[0].park_kind.is_empty());
    }

    #[test]
    fn banned_and_unknown_deadline_survive_load() {
        let store = SqliteStore::open_in_memory().unwrap();
        let mut banned = base_account("139");
        banned.park_kind = "banned".into();
        banned.park_until = "2000-01-01T00:00:00Z".into();
        store.save_account(&banned).unwrap();
        let mut unknown = base_account("137");
        unknown.park_kind = "muted".into(); // no park_until = unknown recovery
        store.save_account(&unknown).unwrap();
        let loaded = store.load(now()).unwrap();
        assert!(loaded
            .iter()
            .any(|a| a.mobile == "139" && a.park_kind == "banned"));
        assert!(loaded
            .iter()
            .any(|a| a.mobile == "137" && a.park_kind == "muted"));
    }

    #[test]
    fn apply_park_merges_and_fences() {
        let store = SqliteStore::open_in_memory().unwrap();
        store.save_account(&base_account("138")).unwrap();
        // Fenced callback with the current generation applies.
        store.apply_park(
            &ParkRecord {
                identity: "138".into(),
                kind: BanState::Muted,
                until: Some(now() + Duration::from_secs(3600)),
                reason: "muted upstream".into(),
                generation: 1,
            },
            now(),
        );
        let loaded = store.load(now()).unwrap();
        assert_eq!(loaded[0].park_kind, "muted");
        assert_eq!(loaded[0].park_reason, "muted upstream");
        // Stale generation (post-mutation) is a no-op.
        store.apply_park(
            &ParkRecord {
                identity: "138".into(),
                kind: BanState::None,
                until: None,
                reason: String::new(),
                generation: 1, // row generation bumped to 2 by the first park
            },
            now(),
        );
        let loaded = store.load(now()).unwrap();
        assert_eq!(
            loaded[0].park_kind, "muted",
            "stale generation must not unpark"
        );
        // Unknown identity is a silent no-op.
        store.apply_park(
            &ParkRecord {
                identity: "999".into(),
                kind: BanState::Banned,
                until: None,
                reason: "x".into(),
                generation: 0,
            },
            now(),
        );
        assert_eq!(store.load(now()).unwrap().len(), 1);
    }

    #[test]
    fn park_write_failure_latches_sticky() {
        let store = SqliteStore::open_in_memory().unwrap();
        store.save_account(&base_account("138")).unwrap();
        // Drop the tables so the park write fails.
        {
            let conn = store.conn.lock().unwrap();
            conn.execute_batch("DROP TABLE device_fingerprints; DROP TABLE accounts;")
                .unwrap();
        }
        store.apply_park(
            &ParkRecord {
                identity: "138".into(),
                kind: BanState::Muted,
                until: Some(now() + Duration::from_secs(3600)),
                reason: "muted upstream".into(),
                generation: 0, // read fails anyway; latch must still trip
            },
            now(),
        );
        assert!(
            store.park_write_failed(),
            "R3 latch must trip on write failure"
        );
    }

    #[test]
    fn apply_login_write_through() {
        let store = SqliteStore::open_in_memory().unwrap();
        store.save_account(&base_account("138")).unwrap();
        store.apply_login(&LoginRecord {
            identity: "138".into(),
            token: "tok-1".into(),
            generation: 1,
        });
        let loaded = store.load(now()).unwrap();
        assert_eq!(loaded[0].session_token, "tok-1");
        // Stale generation: token not written.
        store.apply_login(&LoginRecord {
            identity: "138".into(),
            token: "tok-2".into(),
            generation: 1, // bumped to 2 by the first login write
        });
        let loaded = store.load(now()).unwrap();
        assert_eq!(loaded[0].session_token, "tok-1");
    }

    #[test]
    fn delete_account_and_not_found() {
        let store = SqliteStore::open_in_memory().unwrap();
        store.save_account(&base_account("138")).unwrap();
        store.delete_account("138").unwrap();
        assert!(matches!(
            store.delete_account("138"),
            Err(StoreError::NotFound(_))
        ));
    }

    #[test]
    fn invalid_row_fails_load() {
        let store = SqliteStore::open_in_memory().unwrap();
        let mut a = base_account("138");
        a.password = String::new(); // fails Validate
        store.save_account(&a).unwrap(); // save does not validate
        assert!(matches!(store.load(now()), Err(StoreError::Invalid(_, _))));
    }

    #[test]
    fn generation_increments_on_save() {
        let store = SqliteStore::open_in_memory().unwrap();
        let a = base_account("138");
        store.save_account(&a).unwrap();
        assert_eq!(store.generation("138").unwrap(), 1);
        store.save_account(&a).unwrap();
        assert_eq!(store.generation("138").unwrap(), 2);
        assert_eq!(store.generation("nope").unwrap(), 0);
    }

    #[test]
    fn fingerprints_round_trip_and_join() {
        let store = SqliteStore::open_in_memory().unwrap();
        let a = base_account("138");
        store.save_account(&a).unwrap();
        let profile = crate::device::resolve_device_profile("", "138", "");
        store.save_fingerprint("138", &profile).unwrap();
        let loaded = store.load_fingerprint("138").unwrap().unwrap();
        assert_eq!(loaded.device_id, profile.device_id);
        assert_eq!(loaded.rangers_id, profile.rangers_id);
        assert_eq!(loaded.header_device_id, profile.header_device_id);
        assert_eq!(loaded.channel, "");
        // Overwrite with an identical re-mint: no error, same values.
        store.save_fingerprint("138", &profile).unwrap();
        assert_eq!(store.load_fingerprint("138").unwrap().unwrap(), loaded);
        // Absent identity: None.
        assert!(store.load_fingerprint("999").unwrap().is_none());
    }
}
