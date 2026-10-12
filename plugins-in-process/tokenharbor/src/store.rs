//! Plugin-private SQLite store (contract §3 account-pool boundary: plugin
//! state lives in its own storage engine, never in host process memory that
//! could leak into git; credentials arrive only via env vars).
//!
//! Schema (v1):
//! - `accounts` — one row per TokenHarbor account. Password is stored
//!   because refresh requires re-login when the refresh token is exhausted
//!   (single-use rotation: a crash between grant and persist loses the
//!   session). Token material is NOT logged anywhere (§7 checklist).
//! - `catalog` — cached `/models` RSC snapshot rows + per-account
//!   free-tier entitlement sets (dossier risk #2: model ids churn weekly,
//!   so the store is a cache, never the source of truth).
//!
//! SQLite is accessed synchronously behind a std `Mutex` — TokenHarbor is
//! a 1-2s-paced, low-concurrency upstream by design, so connection
//! contention is not on the request path in any measurable way.

use std::path::Path;
use std::sync::Mutex;

use rusqlite::Connection;

/// Errors from the store. Internal — the plugin maps them onto health/
/// upstream errors, never onto client-facing §4 bodies directly.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("sqlite: {0}")]
    Sql(#[from] rusqlite::Error),
}

/// One account's persisted state.
#[derive(Debug, Clone, PartialEq)]
pub struct AccountRow {
    pub email: String,
    /// Supabase password (re-login path when refresh chain dies).
    pub password: String,
    pub access_token: String,
    pub refresh_token: String,
    /// Unix seconds; 0 = unknown/expired.
    pub access_expires_at: i64,
    /// Full Supabase `user` JSON blob (needed inside the auth cookie).
    pub user_json: String,
    /// Privacy consent accepted (`/api/me/privacy {free_models_enabled:true}`
    /// was POSTed for this account — dossier §2, idempotent gate).
    pub consent_accepted: bool,
    /// Quarantine reason when the account is known-bad (e.g. live-observed
    /// `account_suspended` SSE code). Null when healthy.
    pub quarantined_reason: Option<String>,
    /// Unix seconds of the quarantine event (0 = never).
    pub quarantined_at: i64,
}

impl AccountRow {
    /// Seed row from env-style credentials: no tokens yet, needs login.
    pub fn seed(email: impl Into<String>, password: impl Into<String>) -> Self {
        Self {
            email: email.into(),
            password: password.into(),
            access_token: String::new(),
            refresh_token: String::new(),
            access_expires_at: 0,
            user_json: String::new(),
            consent_accepted: false,
            quarantined_reason: None,
            quarantined_at: 0,
        }
    }

    /// Is the persisted access token still plausibly valid?
    /// Refresh margin: 60s before expiry.
    pub fn access_valid(&self, now_epoch: i64) -> bool {
        !self.access_token.is_empty() && self.access_expires_at - 60 > now_epoch
    }
}

/// One cached model row (subset of the `/models` RSC fields the plugin needs).
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct ModelRow {
    /// Full model id the stream API expects, e.g.
    /// `vercel-ai-gateway/deepseek/deepseek-v4.1-flash:free`.
    pub full_id: String,
    /// RSC `surface` (carries the `:free` suffix on free rows).
    pub surface: String,
    pub label: String,
    /// RSC `isFree`.
    pub is_free: bool,
    /// ISO-8601 expiry of a promo free window; `None` = permanent.
    pub free_until: Option<String>,
}

/// Cached catalog snapshot (models RSC + the serverNow it was parsed with).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct CatalogSnapshot {
    pub models: Vec<ModelRow>,
    /// Unix seconds when the snapshot was fetched (0 = never).
    pub fetched_at: i64,
    /// Page-level `serverNow` from the RSC payload (ISO-8601), when parsed.
    pub server_now: Option<String>,
}

/// One account's free-tier entitlement set (`/api/me/free-tier.model_ids`).
#[derive(Debug, Clone, PartialEq)]
pub struct FreeTier {
    pub email: String,
    /// Surface-style ids WITHOUT `:free` suffix (dossier MODELS.md: this
    /// list ≠ the public free table; it is the authoritative per-account set).
    pub model_ids: Vec<String>,
    pub fetched_at: i64,
}

/// Thread-safe SQLite handle for the plugin's private state.
pub struct Store {
    conn: Mutex<Connection>,
}

impl Store {
    /// Open (creating if needed) the plugin-private database at `path`.
    pub fn open(path: &Path) -> Result<Self, StoreError> {
        let conn = Connection::open(path)?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        Self::with_conn(conn)
    }

    /// In-memory store for tests.
    pub fn in_memory() -> Result<Self, StoreError> {
        Self::with_conn(Connection::open_in_memory()?)
    }

    fn with_conn(conn: Connection) -> Result<Self, StoreError> {
        Self::migrate(&conn)?;
        Ok(Self {
            conn: Mutex::new(conn),
        })
    }

    fn migrate(conn: &Connection) -> Result<(), StoreError> {
        conn.execute_batch(
            r#"
            CREATE TABLE IF NOT EXISTS accounts (
                email               TEXT PRIMARY KEY,
                password            TEXT NOT NULL,
                access_token        TEXT NOT NULL DEFAULT '',
                refresh_token       TEXT NOT NULL DEFAULT '',
                access_expires_at   INTEGER NOT NULL DEFAULT 0,
                user_json           TEXT NOT NULL DEFAULT '{}',
                consent_accepted    INTEGER NOT NULL DEFAULT 0,
                quarantined_reason  TEXT,
                quarantined_at      INTEGER NOT NULL DEFAULT 0
            );
            CREATE TABLE IF NOT EXISTS catalog (
                id          INTEGER PRIMARY KEY CHECK (id = 1),
                models_json TEXT NOT NULL DEFAULT '[]',
                fetched_at  INTEGER NOT NULL DEFAULT 0,
                server_now  TEXT
            );
            CREATE TABLE IF NOT EXISTS free_tier (
                email      TEXT PRIMARY KEY,
                model_ids_json TEXT NOT NULL DEFAULT '[]',
                fetched_at INTEGER NOT NULL DEFAULT 0
            );
            INSERT OR IGNORE INTO catalog (id) VALUES (1);
            "#,
        )?;
        Ok(())
    }

    /// Upsert an account row (seeding path).
    pub fn upsert_account(&self, row: &AccountRow) -> Result<(), StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        conn.execute(
            "INSERT INTO accounts (email, password, access_token, refresh_token,
                                   access_expires_at, user_json, consent_accepted,
                                   quarantined_reason, quarantined_at)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
             ON CONFLICT(email) DO UPDATE SET
                password = excluded.password,
                consent_accepted = excluded.consent_accepted,
                quarantined_reason = excluded.quarantined_reason,
                quarantined_at = excluded.quarantined_at",
            rusqlite::params![
                row.email,
                row.password,
                row.access_token,
                row.refresh_token,
                row.access_expires_at,
                row.user_json,
                row.consent_accepted as i64,
                row.quarantined_reason,
                row.quarantined_at,
            ],
        )?;
        Ok(())
    }

    /// Persist ONLY the token set of an account (login/refresh path — the
    /// dedicated writer so a refresh never clobbers quarantine state).
    ///
    /// **This is the single-use-rotation persistence point** (dossier top
    /// risk #1): the caller must invoke this immediately after a refresh
    /// grant, before issuing any further upstream call.
    pub fn save_tokens(
        &self,
        email: &str,
        access_token: &str,
        refresh_token: &str,
        access_expires_at: i64,
        user_json: &str,
    ) -> Result<(), StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        conn.execute(
            "UPDATE accounts SET access_token = ?2, refresh_token = ?3,
                    access_expires_at = ?4, user_json = ?5
             WHERE email = ?1",
            rusqlite::params![
                email,
                access_token,
                refresh_token,
                access_expires_at,
                user_json
            ],
        )?;
        Ok(())
    }

    /// Mark the privacy consent gate as accepted for one account.
    pub fn set_consent(&self, email: &str, accepted: bool) -> Result<(), StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        conn.execute(
            "UPDATE accounts SET consent_accepted = ?2 WHERE email = ?1",
            rusqlite::params![email, accepted as i64],
        )?;
        Ok(())
    }

    /// Quarantine (or un-quarantine with `None`) one account. Never fails
    /// the caller's request path — callers log and continue.
    pub fn set_quarantine(
        &self,
        email: &str,
        reason: Option<&str>,
        at_epoch: i64,
    ) -> Result<(), StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        conn.execute(
            "UPDATE accounts SET quarantined_reason = ?2, quarantined_at = ?3 WHERE email = ?1",
            rusqlite::params![email, reason, at_epoch],
        )?;
        Ok(())
    }

    /// All accounts, email order.
    pub fn accounts(&self) -> Result<Vec<AccountRow>, StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        let mut stmt = conn.prepare(
            "SELECT email, password, access_token, refresh_token, access_expires_at,
                    user_json, consent_accepted, quarantined_reason, quarantined_at
             FROM accounts ORDER BY email",
        )?;
        let rows = stmt
            .query_map([], |r| {
                Ok(AccountRow {
                    email: r.get(0)?,
                    password: r.get(1)?,
                    access_token: r.get(2)?,
                    refresh_token: r.get(3)?,
                    access_expires_at: r.get(4)?,
                    user_json: r.get(5)?,
                    consent_accepted: r.get::<_, i64>(6)? != 0,
                    quarantined_reason: r.get(7)?,
                    quarantined_at: r.get(8)?,
                })
            })?
            .collect::<Result<Vec<_>, _>>()?;
        Ok(rows)
    }

    /// Persist a catalog snapshot (single row, id=1).
    pub fn save_catalog(&self, snapshot: &CatalogSnapshot) -> Result<(), StoreError> {
        let models_json = serde_json::to_string(&snapshot.models).unwrap_or_else(|_| "[]".into());
        let conn = self.conn.lock().expect("store lock poisoned");
        conn.execute(
            "UPDATE catalog SET models_json = ?1, fetched_at = ?2, server_now = ?3 WHERE id = 1",
            rusqlite::params![models_json, snapshot.fetched_at, snapshot.server_now],
        )?;
        Ok(())
    }

    /// Load the cached catalog snapshot (empty models when never fetched).
    pub fn catalog(&self) -> Result<CatalogSnapshot, StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        let mut stmt =
            conn.prepare("SELECT models_json, fetched_at, server_now FROM catalog WHERE id = 1")?;
        let row = stmt.query_row([], |r| {
            let models_json: String = r.get(0)?;
            Ok((
                models_json,
                r.get::<_, i64>(1)?,
                r.get::<_, Option<String>>(2)?,
            ))
        });
        let (models_json, fetched_at, server_now) = match row {
            Ok(v) => v,
            Err(rusqlite::Error::QueryReturnedNoRows) => (String::from("[]"), 0, None),
            Err(e) => return Err(e.into()),
        };
        let models: Vec<ModelRow> = serde_json::from_str(&models_json).unwrap_or_default();
        Ok(CatalogSnapshot {
            models,
            fetched_at,
            server_now,
        })
    }

    /// Persist one account's free-tier entitlement set.
    pub fn save_free_tier(&self, tier: &FreeTier) -> Result<(), StoreError> {
        let ids_json = serde_json::to_string(&tier.model_ids).unwrap_or_else(|_| "[]".into());
        let conn = self.conn.lock().expect("store lock poisoned");
        conn.execute(
            "INSERT INTO free_tier (email, model_ids_json, fetched_at)
             VALUES (?1, ?2, ?3)
             ON CONFLICT(email) DO UPDATE SET
                model_ids_json = excluded.model_ids_json,
                fetched_at = excluded.fetched_at",
            rusqlite::params![tier.email, ids_json, tier.fetched_at],
        )?;
        Ok(())
    }

    /// Load one account's free-tier entitlement set.
    pub fn free_tier(&self, email: &str) -> Result<Option<FreeTier>, StoreError> {
        let conn = self.conn.lock().expect("store lock poisoned");
        let mut stmt =
            conn.prepare("SELECT model_ids_json, fetched_at FROM free_tier WHERE email = ?1")?;
        let row = stmt.query_row([email], |r| {
            let ids_json: String = r.get(0)?;
            Ok((ids_json, r.get::<_, i64>(1)?))
        });
        match row {
            Ok((ids_json, fetched_at)) => {
                let model_ids: Vec<String> = serde_json::from_str(&ids_json).unwrap_or_default();
                Ok(Some(FreeTier {
                    email: email.to_string(),
                    model_ids,
                    fetched_at,
                }))
            }
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(e.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn seed_upsert_roundtrip() {
        let store = Store::in_memory().unwrap();
        let seed = AccountRow::seed("a@example.com", "pw-a");
        store.upsert_account(&seed).unwrap();

        let rows = store.accounts().unwrap();
        assert_eq!(rows, vec![seed.clone()]);
        assert!(!seed.access_valid(0));
    }

    #[test]
    fn save_tokens_does_not_clobber_quarantine() {
        let store = Store::in_memory().unwrap();
        store
            .upsert_account(&AccountRow::seed("a@example.com", "pw"))
            .unwrap();
        store
            .set_quarantine("a@example.com", Some("account_suspended"), 100)
            .unwrap();
        // Refresh writes tokens only — quarantine survives.
        store
            .save_tokens("a@example.com", "at", "rt", 200, "{}")
            .unwrap();

        let row = &store.accounts().unwrap()[0];
        assert_eq!(row.access_token, "at");
        assert_eq!(row.quarantined_reason.as_deref(), Some("account_suspended"));
        assert_eq!(row.quarantined_at, 100);
    }

    #[test]
    fn upsert_seed_does_not_clobber_tokens() {
        // Re-seeding from env on restart must not wipe a live session.
        let store = Store::in_memory().unwrap();
        store
            .upsert_account(&AccountRow::seed("a@example.com", "pw"))
            .unwrap();
        store
            .save_tokens("a@example.com", "at", "rt", 200, "{}")
            .unwrap();
        // Host restart: seeds upserted again (same env values).
        store
            .upsert_account(&AccountRow::seed("a@example.com", "pw"))
            .unwrap();
        let row = &store.accounts().unwrap()[0];
        assert_eq!(row.refresh_token, "rt", "seed upsert keeps live tokens");
    }

    #[test]
    fn access_validity_with_margin() {
        let mut row = AccountRow::seed("a@example.com", "pw");
        row.access_token = "tok".into();
        row.access_expires_at = 1000;
        assert!(row.access_valid(500));
        assert!(!row.access_valid(950), "60s margin: 950 is too late");
        assert!(!row.access_valid(1001));
    }

    #[test]
    fn consent_flag_roundtrip() {
        let store = Store::in_memory().unwrap();
        store
            .upsert_account(&AccountRow::seed("a@example.com", "pw"))
            .unwrap();
        assert!(!store.accounts().unwrap()[0].consent_accepted);
        store.set_consent("a@example.com", true).unwrap();
        assert!(store.accounts().unwrap()[0].consent_accepted);
    }

    #[test]
    fn catalog_roundtrip() {
        let store = Store::in_memory().unwrap();
        assert!(store.catalog().unwrap().models.is_empty());

        let snapshot = CatalogSnapshot {
            models: vec![ModelRow {
                full_id: "xiaomi/mimo-v2.6-flash:free".into(),
                surface: "mimo-v2.6-flash:free".into(),
                label: "MiMo V2.6 Flash".into(),
                is_free: true,
                free_until: None,
            }],
            fetched_at: 12345,
            server_now: Some("2026-10-12T05:30:18.826Z".into()),
        };
        store.save_catalog(&snapshot).unwrap();
        assert_eq!(store.catalog().unwrap(), snapshot);
    }

    #[test]
    fn free_tier_roundtrip() {
        let store = Store::in_memory().unwrap();
        assert!(store.free_tier("a@example.com").unwrap().is_none());

        let tier = FreeTier {
            email: "a@example.com".into(),
            model_ids: vec![
                "deepseek-v4.1-flash".into(),
                "mimo-v2.6-flash".into(),
                "th-rudder".into(),
            ],
            fetched_at: 42,
        };
        store.save_free_tier(&tier).unwrap();
        store.save_free_tier(&tier).unwrap(); // idempotent upsert
        assert_eq!(store.free_tier("a@example.com").unwrap(), Some(tier));
    }

    #[test]
    fn file_store_persists_across_reopen() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("th.db");
        {
            let store = Store::open(&path).unwrap();
            store
                .upsert_account(&AccountRow::seed("a@example.com", "pw"))
                .unwrap();
            store
                .save_tokens("a@example.com", "at2", "rt2", 999, "{\"id\":\"u\"}")
                .unwrap();
        }
        let store = Store::open(&path).unwrap();
        let row = &store.accounts().unwrap()[0];
        assert_eq!(row.refresh_token, "rt2");
        assert_eq!(row.user_json, "{\"id\":\"u\"}");
    }
}
