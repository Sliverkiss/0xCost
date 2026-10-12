//! Account types mirroring the Go `upstream.Account` shape byte-for-byte at
//! the store boundary (PC-41): mobile/email/password/region/channel +
//! device_id + park fields + session_token.
//!
//! All timestamps are RFC3339 UTC strings — the format Go writes
//! (`time.RFC3339` in `applyParkRecord`) and the only format the SQLite
//! schema accepts (PORT_DESIGN §2.1).

use std::time::SystemTime;

/// Park states (`pool.go` BanState). Banned/muted/risk are the only
/// persisted kinds; `None` renders empty and clears the persisted fields.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BanState {
    None,
    Banned,
    Muted,
    RiskDevice,
}

impl BanState {
    /// The persisted `park_kind` string (`parkKindNames` in Go).
    pub const fn name(self) -> &'static str {
        match self {
            BanState::None => "",
            BanState::Banned => "banned",
            BanState::Muted => "muted",
            BanState::RiskDevice => "risk",
        }
    }

    /// Parse a persisted `park_kind` value; unknown/empty = [`BanState::None`].
    pub fn parse(s: &str) -> Self {
        match s {
            "banned" => BanState::Banned,
            "muted" => BanState::Muted,
            "risk" => BanState::RiskDevice,
            _ => BanState::None,
        }
    }
}

/// Validation errors for one account row (Go `Account.Validate`).
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum AccountError {
    #[error("account entry missing password")]
    MissingPassword,
    #[error("account entry needs mobile or email")]
    MissingIdentity,
    #[error("unknown region (only \"cn\" is supported)")]
    UnknownRegion,
    #[error("unknown channel (\"\" = android, \"web\")")]
    UnknownChannel,
    #[error("web channel requires an explicit device_id")]
    WebChannelNeedsDeviceID,
}

/// One upstream account. Field order/shape mirrors the Go JSON record so a
/// seed import of Upstash data (PC-41) round-trips losslessly.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Account {
    pub mobile: String,
    pub email: String,
    pub password: String,
    /// Empty means "cn" (the only real region; unknown values reject).
    pub region: String,
    /// Explicit device identity; empty = mint deterministically at boot.
    pub device_id: String,
    /// `""` (android default) or `"web"` (browser-harvested Shumei id).
    pub channel: String,
    pub park_kind: String,
    /// RFC3339 UTC, or empty (unknown recovery).
    pub park_until: String,
    pub park_reason: String,
    /// RFC3339 UTC, or empty.
    pub parked_at: String,
    /// Last login token, write-through; never replayed at boot.
    pub session_token: String,
}

impl Account {
    /// Primary identity: mobile, else email (Go `Account.Identity()`).
    pub fn identity(&self) -> &str {
        let m = self.mobile.trim();
        if !m.is_empty() {
            return m;
        }
        self.email.trim()
    }

    /// Does `id` (mobile or email) identify this account?
    pub fn matches_identity(&self, id: &str) -> bool {
        let id = id.trim();
        if id.is_empty() {
            return false;
        }
        if !self.mobile.is_empty() {
            return self.mobile == id;
        }
        !self.email.is_empty() && self.email == id
    }

    /// Normalized region: `""`/"cn" → "cn", anything else rejected.
    pub fn normalized_region(&self) -> Result<&'static str, AccountError> {
        match self.region.as_str() {
            "" | "cn" => Ok("cn"),
            _ => Err(AccountError::UnknownRegion),
        }
    }

    /// Validate at load/seed time (Go `Account.Validate`).
    pub fn validate(&self) -> Result<(), AccountError> {
        if self.password.is_empty() {
            return Err(AccountError::MissingPassword);
        }
        if self.mobile.trim().is_empty() && self.email.trim().is_empty() {
            return Err(AccountError::MissingIdentity);
        }
        self.normalized_region()?;
        match self.channel.as_str() {
            "" | "web" => {}
            _ => return Err(AccountError::UnknownChannel),
        }
        if self.channel == "web" && self.device_id.trim().is_empty() {
            return Err(AccountError::WebChannelNeedsDeviceID);
        }
        Ok(())
    }

    /// Merge one park transition into the fields in place; reports whether
    /// anything changed. A re-park of the identical state is a no-op so
    /// `parked_at` does not churn; an unpark clears the fields (Go
    /// `applyParkRecord`).
    pub fn apply_park_record(
        &mut self,
        kind: BanState,
        until: Option<&str>,
        reason: &str,
        now: &str,
    ) -> bool {
        if kind == BanState::None {
            let changed = !self.park_kind.is_empty()
                || !self.park_until.is_empty()
                || !self.park_reason.is_empty()
                || !self.parked_at.is_empty();
            self.park_kind = String::new();
            self.park_until = String::new();
            self.park_reason = String::new();
            self.parked_at = String::new();
            return changed;
        }
        let until = until.unwrap_or("");
        if self.park_kind == kind.name() && self.park_until == until && self.park_reason == reason {
            return false; // already persisted exactly this state
        }
        self.park_kind = kind.name().to_string();
        self.park_until = until.to_string();
        self.park_reason = reason.to_string();
        self.parked_at = now.to_string();
        true
    }

    /// Does this mute/risk park have a parseable deadline that has elapsed?
    /// Missing or corrupt deadlines have unknown recovery and must remain
    /// parked (Go `store.parkExpired` — clearing them would admit a disabled
    /// identity on restart).
    pub fn park_expired(&self, now: SystemTime) -> bool {
        let kind = BanState::parse(&self.park_kind);
        if !matches!(kind, BanState::Muted | BanState::RiskDevice) {
            return false;
        }
        match crate::timeutil::parse_rfc3339(&self.park_until) {
            Some(until) => now >= until,
            None => false,
        }
    }

    /// Strip expired park fields (load-time cleanup). Returns whether the
    /// record changed.
    pub fn strip_expired_park(&mut self, now: SystemTime) -> bool {
        if self.park_expired(now) {
            self.park_kind = String::new();
            self.park_until = String::new();
            self.park_reason = String::new();
            self.parked_at = String::new();
            return true;
        }
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn identity_prefers_mobile() {
        let a = Account {
            mobile: "138".into(),
            email: "a@b".into(),
            password: "p".into(),
            ..Default::default()
        };
        assert_eq!(a.identity(), "138");
        let b = Account {
            email: "a@b".into(),
            password: "p".into(),
            ..Default::default()
        };
        assert_eq!(b.identity(), "a@b");
    }

    #[test]
    fn park_kind_names_round_trip() {
        for k in [BanState::Banned, BanState::Muted, BanState::RiskDevice] {
            assert_eq!(BanState::parse(k.name()), k);
        }
        assert_eq!(BanState::parse(""), BanState::None);
        assert_eq!(BanState::parse("garbage"), BanState::None);
    }

    #[test]
    fn validate_rules() {
        let mut a = Account {
            mobile: "1".into(),
            password: "p".into(),
            ..Default::default()
        };
        assert!(a.validate().is_ok());
        a.region = "mars".into();
        assert_eq!(a.validate(), Err(AccountError::UnknownRegion));
        a.region.clear();
        a.channel = "ios".into();
        assert_eq!(a.validate(), Err(AccountError::UnknownChannel));
        a.channel = "web".into();
        assert_eq!(a.validate(), Err(AccountError::WebChannelNeedsDeviceID));
        a.device_id = "B-shumei".into();
        assert!(a.validate().is_ok());
    }

    #[test]
    fn apply_park_record_idempotent_and_unpark() {
        let now = "2026-10-12T00:00:00Z";
        let mut a = Account::default();
        assert!(a.apply_park_record(
            BanState::Muted,
            Some("2026-10-13T00:00:00Z"),
            "muted upstream",
            now
        ));
        assert_eq!(a.park_kind, "muted");
        assert!(!a.apply_park_record(
            BanState::Muted,
            Some("2026-10-13T00:00:00Z"),
            "muted upstream",
            now
        ));
        assert!(a.apply_park_record(BanState::None, None, "", now));
        assert!(a.park_kind.is_empty() && a.park_until.is_empty());
        assert!(!a.apply_park_record(BanState::None, None, "", now));
    }

    #[test]
    fn park_expiry_unknown_deadline_quarantined() {
        let now = SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000);
        // Unknown deadline: never expires.
        let mut a = Account {
            park_kind: "muted".into(),
            ..Default::default()
        };
        assert!(!a.park_expired(now));
        assert!(!a.strip_expired_park(now));
        // Corrupt deadline: same.
        a.park_until = "not-a-date".into();
        assert!(!a.park_expired(now));
        // Parseable future: not expired.
        a.park_until = "2999-01-01T00:00:00Z".into();
        assert!(!a.park_expired(now));
        // Parseable past: stripped.
        a.park_until = "2000-01-01T00:00:00Z".into();
        assert!(a.strip_expired_park(now));
        assert!(a.park_kind.is_empty());
        // Banned never expires even with a past deadline.
        let b = Account {
            park_kind: "banned".into(),
            park_until: "2000-01-01T00:00:00Z".into(),
            ..Default::default()
        };
        assert!(!b.park_expired(now));
    }
}
