//! Plugin-private SQLite runtime store (PORT_DESIGN §2.2, maintainer
//! decision): the persistence of record for account rows and the state
//! audit trail. In-flight slots, leases, EWMA and selection state stay
//! in the in-memory pool; this store hydrates it at startup and receives
//! write-through updates on the same points Go persists.
//!
//! Synchronous `rusqlite` (bundled) behind a `Mutex<Connection>` — pool
//! ops are microseconds; single-writer + WAL matches the Go design's
//! one-mutex discipline. No credential is ever logged or exported.

use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use rusqlite::{params, Connection, OptionalExtension};

use crate::pool::{AccountRecord, FailureClass, State};

/// One stored account row (the Go `Account` JSON record, normalized).
/// Credentials (phone/password/pass_token/cookie) are never logged.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct StoredAccount {
    pub id: String,
    pub phone: Option<String>,
    pub password: Option<String>,
    pub enabled: bool,
    pub label: Option<String>,
    pub egress: String,
    pub user_id: Option<String>,
    pub pass_token: Option<String>,
    /// Harvested device identity (`{"deviceId":...,"pass_ua":...}`),
    /// merged non-destructively (Go `MergeDevice`).
    pub device_json: Option<String>,
    /// Full session Cookie header value.
    pub cookie: Option<String>,
    pub ph: Option<String>,
    pub session_fetched_at: Option<i64>,
    pub state: State,
    pub cooldown_until: Option<i64>,
    pub cooldown_class: Option<FailureClass>,
    pub cooldown_status: Option<u16>,
    pub cooldown_since: Option<i64>,
    pub disabled_reason: Option<String>,
    /// Dead passToken (70016): never auto-recovers.
    pub permanent: bool,
    pub failures_json: String,
    pub ewma_latency_ms: i64,
    pub created_at: i64,
    pub updated_at: i64,
}

/// One audit row (replaces Go's ephemeral last-error tracking).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StateEvent {
    pub id: i64,
    pub account_id: String,
    pub at: i64,
    pub kind: StateEventKind,
    pub class: Option<FailureClass>,
    pub status: Option<u16>,
    pub detail: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StateEventKind {
    Success,
    Failure,
    Cooldown,
    Disabled,
    Enabled,
    SessionRefreshed,
    OtpStarted,
    OtpSubmitted,
    RiskControl,
}

impl StateEventKind {
    pub const fn as_str(self) -> &'static str {
        match self {
            StateEventKind::Success => "success",
            StateEventKind::Failure => "failure",
            StateEventKind::Cooldown => "cooldown",
            StateEventKind::Disabled => "disabled",
            StateEventKind::Enabled => "enabled",
            StateEventKind::SessionRefreshed => "session_refreshed",
            StateEventKind::OtpStarted => "otp_started",
            StateEventKind::OtpSubmitted => "otp_submitted",
            StateEventKind::RiskControl => "risk_control",
        }
    }

    fn from_db(s: &str) -> Option<Self> {
        Some(match s {
            "success" => Self::Success,
            "failure" => Self::Failure,
            "cooldown" => Self::Cooldown,
            "disabled" => Self::Disabled,
            "enabled" => Self::Enabled,
            "session_refreshed" => Self::SessionRefreshed,
            "otp_started" => Self::OtpStarted,
            "otp_submitted" => Self::OtpSubmitted,
            "risk_control" => Self::RiskControl,
            _ => return None,
        })
    }
}

fn class_to_db(c: FailureClass) -> &'static str {
    match c {
        FailureClass::Transport => "transport",
        FailureClass::Upstream5xx => "upstream-5xx",
        FailureClass::Throttle429 => "throttle-429",
    }
}

fn class_from_db(s: &str) -> Option<FailureClass> {
    Some(match s {
        "transport" => FailureClass::Transport,
        "upstream-5xx" => FailureClass::Upstream5xx,
        "throttle-429" => FailureClass::Throttle429,
        _ => return None,
    })
}

fn state_to_db(s: State) -> &'static str {
    s.as_str()
}

fn state_from_db(s: &str) -> Option<State> {
    Some(match s {
        "available" => State::Available,
        "cooldown" => State::Cooldown,
        "disabled" => State::Disabled,
        _ => return None,
    })
}

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS accounts (
  id              TEXT PRIMARY KEY,
  phone           TEXT,
  password        TEXT,
  enabled         INTEGER NOT NULL DEFAULT 1,
  label           TEXT,
  egress          TEXT NOT NULL DEFAULT 'direct',
  user_id         TEXT,
  pass_token      TEXT,
  device_json     TEXT,
  cookie          TEXT,
  ph              TEXT,
  session_fetched_at INTEGER,
  state           TEXT NOT NULL DEFAULT 'available'
                  CHECK (state IN ('available','cooldown','disabled')),
  cooldown_until  INTEGER,
  cooldown_class  TEXT,
  cooldown_status INTEGER,
  cooldown_since  INTEGER,
  disabled_reason TEXT,
  permanent       INTEGER NOT NULL DEFAULT 0,
  failures_json   TEXT NOT NULL DEFAULT '{}',
  ewma_latency_ms INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_state ON accounts(state, cooldown_until);
CREATE TABLE IF NOT EXISTS state_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  at         INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  class      TEXT,
  status     INTEGER,
  detail     TEXT
);
CREATE INDEX IF NOT EXISTS idx_state_events_account ON state_events(account_id, at);
";

/// The SQLite store. `Clone` shares one connection (single writer).
#[derive(Clone)]
pub struct Store {
    conn: Arc<Mutex<Connection>>,
}

impl Store {
    /// Opens (creating if needed) the database at `path`, applies the
    /// schema, enables WAL + busy_timeout.
    pub fn open(path: impl AsRef<Path>) -> rusqlite::Result<Self> {
        let conn = Connection::open(path)?;
        conn.busy_timeout(std::time::Duration::from_millis(5000))?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.execute_batch(SCHEMA)?;
        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    /// In-memory store (tests, fixtures).
    pub fn open_in_memory() -> rusqlite::Result<Self> {
        let conn = Connection::open_in_memory()?;
        conn.execute_batch(SCHEMA)?;
        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    /// Inserts or replaces an account row wholesale (seeding path).
    pub fn upsert_account(&self, a: &StoredAccount) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        let now = now_secs();
        conn.execute(
            "INSERT INTO accounts (id, phone, password, enabled, label, egress, user_id,
                pass_token, device_json, cookie, ph, session_fetched_at, state,
                cooldown_until, cooldown_class, cooldown_status, cooldown_since,
                disabled_reason, permanent, failures_json, ewma_latency_ms,
                created_at, updated_at)
             VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16,?17,
                ?18,?19,?20,?21,?22,?23)
             ON CONFLICT(id) DO UPDATE SET
                phone=excluded.phone, password=excluded.password,
                enabled=excluded.enabled, label=excluded.label,
                egress=excluded.egress, user_id=excluded.user_id,
                pass_token=excluded.pass_token, device_json=excluded.device_json,
                cookie=excluded.cookie, ph=excluded.ph,
                session_fetched_at=excluded.session_fetched_at,
                state=excluded.state, cooldown_until=excluded.cooldown_until,
                cooldown_class=excluded.cooldown_class,
                cooldown_status=excluded.cooldown_status,
                cooldown_since=excluded.cooldown_since,
                disabled_reason=excluded.disabled_reason,
                permanent=excluded.permanent, failures_json=excluded.failures_json,
                ewma_latency_ms=excluded.ewma_latency_ms,
                updated_at=excluded.updated_at",
            params![
                a.id,
                a.phone,
                a.password,
                a.enabled as i64,
                a.label,
                a.egress,
                a.user_id,
                a.pass_token,
                a.device_json,
                a.cookie,
                a.ph,
                a.session_fetched_at,
                state_to_db(a.state),
                a.cooldown_until,
                a.cooldown_class.map(class_to_db),
                a.cooldown_status.map(|s| s as i64),
                a.cooldown_since,
                a.disabled_reason,
                a.permanent as i64,
                a.failures_json,
                a.ewma_latency_ms,
                a.created_at,
                now,
            ],
        )?;
        Ok(())
    }

    /// Write-through on session apply: credentials + refresh stamp.
    /// Never logged.
    pub fn apply_session(
        &self,
        id: &str,
        user_id: Option<&str>,
        pass_token: Option<&str>,
        cookie: Option<&str>,
        ph: Option<&str>,
        device_json: Option<&str>,
    ) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        // Device merge is non-destructive: a NULL/empty harvest never wipes
        // stored values (Go `MergeDevice`).
        conn.execute(
            "UPDATE accounts SET
                user_id = COALESCE(NULLIF(?2,''), user_id),
                pass_token = COALESCE(NULLIF(?3,''), pass_token),
                cookie = COALESCE(NULLIF(?4,''), cookie),
                ph = COALESCE(NULLIF(?5,''), ph),
                device_json = COALESCE(NULLIF(?6,''), device_json),
                session_fetched_at = ?7,
                updated_at = ?7
             WHERE id = ?1",
            params![id, user_id, pass_token, cookie, ph, device_json, now_secs()],
        )?;
        Ok(())
    }

    /// Write-through on state/cooldown change.
    #[allow(clippy::too_many_arguments)]
    pub fn set_pool_state(
        &self,
        id: &str,
        state: State,
        cooldown_until: Option<i64>,
        cooldown_class: Option<FailureClass>,
        cooldown_status: Option<u16>,
        disabled_reason: Option<&str>,
        permanent: bool,
    ) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        let since = if state == State::Cooldown {
            Some(now_secs())
        } else {
            None
        };
        conn.execute(
            "UPDATE accounts SET state=?2, cooldown_until=?3, cooldown_class=?4,
                cooldown_status=?5, disabled_reason=?6, permanent=?7,
                cooldown_since=COALESCE(?8, cooldown_since), updated_at=?9
             WHERE id=?1",
            params![
                id,
                state_to_db(state),
                cooldown_until,
                cooldown_class.map(class_to_db),
                cooldown_status.map(|s| s as i64),
                disabled_reason,
                permanent as i64,
                since,
                now_secs(),
            ],
        )?;
        Ok(())
    }

    /// Operator enable: clears cooldown/disabled state.
    pub fn set_enabled(&self, id: &str, enabled: bool) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE accounts SET enabled=?2,
                state=CASE WHEN ?2 THEN 'available' ELSE state END,
                permanent=CASE WHEN ?2 THEN 0 ELSE permanent END,
                disabled_reason=CASE WHEN ?2 THEN NULL ELSE disabled_reason END,
                updated_at=?3
             WHERE id=?1",
            params![id, enabled as i64, now_secs()],
        )?;
        Ok(())
    }

    /// Device persistence even on failed login (load-bearing, PORT_DESIGN
    /// §1: the operator's pending SMS code stays verifiable).
    pub fn merge_device_only(&self, id: &str, device_json: &str) -> rusqlite::Result<()> {
        if device_json.trim().is_empty() {
            return Ok(());
        }
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE accounts SET device_json=?2, updated_at=?3 WHERE id=?1",
            params![id, device_json, now_secs()],
        )?;
        Ok(())
    }

    /// Appends an audit row. `detail` must never carry credential material
    /// or raw upstream 451/461 bodies (INV-4).
    pub fn record_event(
        &self,
        account_id: &str,
        kind: StateEventKind,
        class: Option<FailureClass>,
        status: Option<u16>,
        detail: Option<&str>,
    ) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "INSERT INTO state_events (account_id, at, kind, class, status, detail)
             VALUES (?1,?2,?3,?4,?5,?6)",
            params![
                account_id,
                now_secs(),
                kind.as_str(),
                class.map(class_to_db),
                status.map(|s| s as i64),
                detail
            ],
        )?;
        Ok(())
    }

    /// All account rows (startup hydration; credentials included — the
    /// store is plugin-private).
    pub fn load_accounts(&self) -> rusqlite::Result<Vec<StoredAccount>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare(
            "SELECT id, phone, password, enabled, label, egress, user_id, pass_token,
                device_json, cookie, ph, session_fetched_at, state, cooldown_until,
                cooldown_class, cooldown_status, cooldown_since, disabled_reason,
                permanent, failures_json, ewma_latency_ms, created_at, updated_at
             FROM accounts ORDER BY id",
        )?;
        let rows = stmt.query_map([], row_to_account)?;
        rows.collect()
    }

    /// Audit rows for one account, newest first.
    pub fn events_for(&self, account_id: &str, limit: u64) -> rusqlite::Result<Vec<StateEvent>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare(
            "SELECT id, account_id, at, kind, class, status, detail
             FROM state_events WHERE account_id = ?1 ORDER BY at DESC, id DESC
             LIMIT ?2",
        )?;
        let rows = stmt.query_map(params![account_id, limit], |r| {
            let kind: String = r.get(3)?;
            Ok(StateEvent {
                id: r.get(0)?,
                account_id: r.get(1)?,
                at: r.get(2)?,
                kind: StateEventKind::from_db(&kind).unwrap_or(StateEventKind::Failure),
                class: r
                    .get::<_, Option<String>>(4)?
                    .and_then(|s| class_from_db(&s)),
                status: r.get::<_, Option<i64>>(5)?.map(|s| s as u16),
                detail: r.get(6)?,
            })
        })?;
        rows.collect()
    }

    /// Whether an id already exists (seeding dedup).
    pub fn contains(&self, id: &str) -> rusqlite::Result<bool> {
        let conn = self.conn.lock().unwrap();
        let n: Option<i64> = conn
            .query_row("SELECT 1 FROM accounts WHERE id=?1", params![id], |r| {
                r.get(0)
            })
            .optional()?;
        Ok(n.is_some())
    }
}

fn row_to_account(r: &rusqlite::Row<'_>) -> rusqlite::Result<StoredAccount> {
    let state: String = r.get(12)?;
    let cooldown_class: Option<String> = r.get(14)?;
    Ok(StoredAccount {
        id: r.get(0)?,
        phone: r.get(1)?,
        password: r.get(2)?,
        enabled: r.get::<_, i64>(3)? != 0,
        label: r.get(4)?,
        egress: r.get(5)?,
        user_id: r.get(6)?,
        pass_token: r.get(7)?,
        device_json: r.get(8)?,
        cookie: r.get(9)?,
        ph: r.get(10)?,
        session_fetched_at: r.get(11)?,
        state: state_from_db(&state).unwrap_or(State::Available),
        cooldown_until: r.get(13)?,
        cooldown_class: cooldown_class.as_deref().and_then(class_from_db),
        cooldown_status: r.get::<_, Option<i64>>(15)?.map(|s| s as u16),
        cooldown_since: r.get(16)?,
        disabled_reason: r.get(17)?,
        permanent: r.get::<_, i64>(18)? != 0,
        failures_json: r.get(19)?,
        ewma_latency_ms: r.get(20)?,
        created_at: r.get(21)?,
        updated_at: r.get(22)?,
    })
}

/// Seeds accounts from the `MIMO_ACCOUNT_N_PHONE` / `MIMO_ACCOUNT_N_PASSWORD`
/// env-var family (test-time contract: values enter via env only, never
/// committed). Existing ids are updated (phone/password refresh), not
/// duplicated. Returns the ids seeded.
pub fn seed_from_env(
    store: &Store,
    vars: &dyn Fn(&str) -> Option<String>,
) -> rusqlite::Result<Vec<String>> {
    let mut seeded = Vec::new();
    let mut n = 1;
    loop {
        let phone = vars(&format!("MIMO_ACCOUNT_{n}_PHONE"));
        let Some(phone) = phone else { break };
        let id = vars(&format!("MIMO_ACCOUNT_{n}_ID")).unwrap_or_else(|| format!("env-{n}"));
        let password = vars(&format!("MIMO_ACCOUNT_{n}_PASSWORD"));
        let egress = vars(&format!("MIMO_ACCOUNT_{n}_EGRESS"))
            .unwrap_or_else(|| crate::pool::DEFAULT_EGRESS.to_string());
        let now = now_secs();
        if store.contains(&id)? {
            let conn = store.conn.lock().unwrap();
            conn.execute(
                "UPDATE accounts SET phone=?2, password=COALESCE(?3,password),
                    egress=?4, updated_at=?5 WHERE id=?1",
                params![id, phone, password, egress, now],
            )?;
        } else {
            store.upsert_account(&StoredAccount {
                id: id.clone(),
                phone: Some(phone),
                password,
                enabled: true,
                egress,
                created_at: now,
                updated_at: now,
                ..Default::default()
            })?;
        }
        seeded.push(id);
        n += 1;
    }
    Ok(seeded)
}

/// Converts stored rows into pool `AccountRecord`s (disabled rows become
/// `State::Disabled`; `enabled=0` also disables — two belts, one intent).
pub fn to_pool_records(rows: &[StoredAccount]) -> Vec<AccountRecord> {
    rows.iter()
        .map(|a| AccountRecord {
            id: a.id.clone(),
            egress: a.egress.clone(),
            state: if a.enabled {
                Some(a.state)
            } else {
                Some(State::Disabled)
            },
            cooldown_until_ms: a.cooldown_until.map(|s| s * 1000),
            disabled_reason: a.disabled_reason.clone().unwrap_or_default(),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn stored(id: &str) -> StoredAccount {
        StoredAccount {
            id: id.to_string(),
            egress: "direct".to_string(),
            enabled: true,
            state: State::Available,
            failures_json: "{}".to_string(),
            created_at: 1,
            updated_at: 1,
            ..Default::default()
        }
    }

    #[test]
    fn roundtrip_account_all_fields() {
        let s = Store::open_in_memory().unwrap();
        let a = StoredAccount {
            phone: Some("18800000016".into()),
            password: Some("pw".into()),
            label: Some("acct-a".into()),
            user_id: Some("u1".into()),
            pass_token: Some("pt".into()),
            device_json: Some(r#"{"deviceId":"d"}"#.into()),
            cookie: Some("sid=x".into()),
            ph: Some("ph".into()),
            session_fetched_at: Some(100),
            state: State::Cooldown,
            cooldown_until: Some(200),
            cooldown_class: Some(FailureClass::Throttle429),
            cooldown_status: Some(429),
            cooldown_since: Some(100),
            permanent: false,
            ewma_latency_ms: 42,
            ..stored("a")
        };
        s.upsert_account(&a).unwrap();
        let rows = s.load_accounts().unwrap();
        assert_eq!(rows.len(), 1);
        // updated_at is server-stamped at write time; everything else
        // round-trips exactly.
        let got = rows.into_iter().map(|mut r| {
            r.updated_at = 0;
            r
        });
        let mut want = a;
        want.updated_at = 0;
        assert_eq!(got.collect::<Vec<_>>(), vec![want]);
    }

    #[test]
    fn apply_session_never_wipes_stored_values() {
        let s = Store::open_in_memory().unwrap();
        s.upsert_account(&stored("a")).unwrap();
        s.apply_session("a", Some("u"), Some("pt"), Some("ck"), Some("ph"), None)
            .unwrap();
        s.apply_session("a", None, None, None, None, Some(r#"{"deviceId":"d2"}"#))
            .unwrap();
        let a = &s.load_accounts().unwrap()[0];
        assert_eq!(a.user_id.as_deref(), Some("u")); // kept
        assert_eq!(a.pass_token.as_deref(), Some("pt")); // kept
        assert_eq!(a.cookie.as_deref(), Some("ck")); // kept
        assert_eq!(a.ph.as_deref(), Some("ph")); // kept
        assert_eq!(a.device_json.as_deref(), Some(r#"{"deviceId":"d2"}"#)); // set
        assert!(a.session_fetched_at.unwrap() > 0);
    }

    #[test]
    fn set_pool_state_and_operator_enable() {
        let s = Store::open_in_memory().unwrap();
        s.upsert_account(&stored("a")).unwrap();
        s.set_pool_state(
            "a",
            State::Disabled,
            None,
            None,
            None,
            Some("account restricted"),
            false,
        )
        .unwrap();
        let a = &s.load_accounts().unwrap()[0];
        assert_eq!(a.state, State::Disabled);
        assert_eq!(a.disabled_reason.as_deref(), Some("account restricted"));
        // Operator enable clears everything.
        s.set_enabled("a", true).unwrap();
        let a = &s.load_accounts().unwrap()[0];
        assert_eq!(a.state, State::Available);
        assert!(a.disabled_reason.is_none());
        assert!(!a.permanent);
    }

    #[test]
    fn cooldown_state_roundtrips() {
        let s = Store::open_in_memory().unwrap();
        s.upsert_account(&stored("a")).unwrap();
        s.set_pool_state(
            "a",
            State::Cooldown,
            Some(120),
            Some(FailureClass::Upstream5xx),
            Some(502),
            None,
            false,
        )
        .unwrap();
        let a = &s.load_accounts().unwrap()[0];
        assert_eq!(a.state, State::Cooldown);
        assert_eq!(a.cooldown_until, Some(120));
        assert_eq!(a.cooldown_class, Some(FailureClass::Upstream5xx));
        assert_eq!(a.cooldown_status, Some(502));
        assert!(a.cooldown_since.is_some());
    }

    #[test]
    fn events_record_and_query_newest_first() {
        let s = Store::open_in_memory().unwrap();
        s.upsert_account(&stored("a")).unwrap();
        s.record_event(
            "a",
            StateEventKind::Failure,
            Some(FailureClass::Throttle429),
            Some(429),
            Some("throttled"),
        )
        .unwrap();
        s.record_event("a", StateEventKind::Success, None, None, None)
            .unwrap();
        let evs = s.events_for("a", 10).unwrap();
        assert_eq!(evs.len(), 2);
        assert_eq!(evs[0].kind, StateEventKind::Success); // newest first
        assert_eq!(evs[1].kind, StateEventKind::Failure);
        assert_eq!(evs[1].class, Some(FailureClass::Throttle429));
        assert_eq!(evs[1].status, Some(429));
    }

    #[test]
    fn merge_device_only_on_empty_is_noop() {
        let s = Store::open_in_memory().unwrap();
        s.upsert_account(&stored("a")).unwrap();
        s.merge_device_only("a", "  ").unwrap();
        assert!(s.load_accounts().unwrap()[0].device_json.is_none());
        s.merge_device_only("a", r#"{"deviceId":"d"}"#).unwrap();
        assert_eq!(
            s.load_accounts().unwrap()[0].device_json.as_deref(),
            Some(r#"{"deviceId":"d"}"#)
        );
    }

    #[test]
    fn env_seeding_upserts_and_refreshes() {
        let s = Store::open_in_memory().unwrap();
        let vars = |k: &str| -> Option<String> {
            match k {
                "MIMO_ACCOUNT_1_PHONE" => Some("18800000001".into()),
                "MIMO_ACCOUNT_1_PASSWORD" => Some("p1".into()),
                "MIMO_ACCOUNT_2_PHONE" => Some("18800000002".into()),
                _ => None,
            }
        };
        let seeded = seed_from_env(&s, &vars).unwrap();
        assert_eq!(seeded, vec!["env-1", "env-2"]);
        let rows = s.load_accounts().unwrap();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].phone.as_deref(), Some("18800000001"));
        // Re-seed with a changed password: update, not duplicate.
        let vars2 = |k: &str| -> Option<String> {
            match k {
                "MIMO_ACCOUNT_1_PHONE" => Some("18800000001".into()),
                "MIMO_ACCOUNT_1_PASSWORD" => Some("p1-new".into()),
                _ => None,
            }
        };
        seed_from_env(&s, &vars2).unwrap();
        let rows = s.load_accounts().unwrap();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].password.as_deref(), Some("p1-new"));
    }

    #[test]
    fn to_pool_records_honors_enabled_flag() {
        let s = Store::open_in_memory().unwrap();
        let mut a = stored("a");
        a.state = State::Cooldown;
        a.cooldown_until = Some(200);
        s.upsert_account(&a).unwrap();
        let mut b = stored("b");
        b.enabled = false;
        s.upsert_account(&b).unwrap();
        let recs = to_pool_records(&s.load_accounts().unwrap());
        assert_eq!(recs[0].state, Some(State::Cooldown));
        assert_eq!(recs[0].cooldown_until_ms, Some(200_000));
        assert_eq!(recs[1].state, Some(State::Disabled));
    }
}
