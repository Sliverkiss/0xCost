//! Environment contract (PORT_DESIGN §6.2): the host injects
//! `MIMO_RUST_*` variables; this module resolves them once at plugin
//! construction. No secret ever has a default and none is logged.
//!
//! Reading is a function of an injected `vars` closure so tests can pin
//! the whole environment offline; the constructor passes
//! `|k| std::env::var(k).ok()`.

use std::collections::HashMap;
use std::path::PathBuf;
use std::time::Duration;

use crate::login::Options as LoginOptions;
use crate::pool::Options as PoolOptions;

/// Everything the plugin needs at startup, resolved from env.
/// (No `Debug`: pool options embed a clock closure that isn't printable.)
#[derive(Clone)]
pub struct Config {
    /// SQLite path (plugin-private dir). Default `mimo-rust.db` in the
    /// host's working directory (the host's plugin data dir is not
    /// visible to the plugin process today; setting
    /// `MIMO_RUST_DB_PATH` is the supported placement).
    pub db_path: PathBuf,
    /// Upstream base URL.
    pub base: String,
    /// Model fallback when the request says "auto"/"".
    pub default_model: String,
    /// Thinking default for models without a catalog-declared default
    /// (ADR 0031: pro models default ON).
    pub thinking_default_on: bool,
    /// Login-chain knobs (browser identity, idle window, login behavior).
    pub login: LoginOptions,
    /// Pool knobs (cooldowns, inflight, lease TTL, queue wait).
    pub pool: PoolOptions,
    /// The egress labels the plugin must build proxied clients for
    /// (`MIMO_RUST_EGRESS_<LABEL>` → proxy URL), keyed by label.
    pub egress: HashMap<String, url::Url>,
}

impl Config {
    /// Resolve the whole contract from env (PORT_DESIGN §6.2 table).
    /// Unknown vars are ignored; missing vars fall to Go-parity defaults.
    pub fn from_env(vars: &dyn Fn(&str) -> Option<String>) -> Self {
        let db_path = vars("MIMO_RUST_DB_PATH")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("mimo-rust.db"));
        let base = vars("MIMO_RUST_UPSTREAM_BASE")
            .unwrap_or_else(|| crate::login::DEFAULT_BASE.to_string());
        let default_model =
            vars("MIMO_RUST_DEFAULT_MODEL").unwrap_or_else(|| "mimo-v2.6-pro".to_string());
        let thinking_default_on = bool_var(vars, "MIMO_RUST_THINKING_DEFAULT_ON", true);

        let login = LoginOptions {
            base: Some(base.clone()),
            // Login behavior (Go MIMO2API_OTP_AUTO default false,
            // MIMO2API_CROSS_REGION_AGREE default true).
            disable_otp_auto: !bool_var(vars, "MIMO_RUST_OTP_AUTO", false),
            disable_cross_region_agree: !bool_var(vars, "MIMO_RUST_CROSS_REGION_AGREE", true),
            ..Default::default()
        };
        // Optional browser identity + silence window (Go
        // MIMO2API_SSE_IDLE_TIMEOUT_S; the login client's 180s default
        // already applies when unset).
        let mut login = login;
        if let Some(ua) = vars("MIMO_RUST_USER_AGENT") {
            login.user_agent = Some(ua);
        }
        if let Some(ch) = vars("MIMO_RUST_SEC_CH_UA") {
            login.sec_ch_ua = Some(ch);
        }
        if let Some(tz) = vars("MIMO_RUST_X_TIMEZONE") {
            login.timezone = Some(tz);
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_SSE_IDLE_TIMEOUT_S") {
            login.idle_timeout = Some(Duration::from_secs(s));
        }

        // Pool knobs (Go MIMO2API_* names, MIMO_RUST_ namespace).
        let mut pool = PoolOptions::default();
        if let Some(n) = int_var(vars, "MIMO_RUST_MAX_INFLIGHT_PER_ACCOUNT") {
            pool.max_inflight = n.max(1) as usize;
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_QUEUE_WAIT_S") {
            pool.queue_wait = Duration::from_secs(s);
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_COOLDOWN_429_S") {
            pool.cooldown_429 = Duration::from_secs(s);
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_COOLDOWN_5XX_S") {
            pool.cooldown_5xx = Duration::from_secs(s);
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_COOLDOWN_TRANSPORT_S") {
            pool.cooldown_transport = Duration::from_secs(s);
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_COOLDOWN_CAP_S") {
            pool.cooldown_cap = Duration::from_secs(s);
        }
        if let Some(s) = secs_var(vars, "MIMO_RUST_COOLDOWN_TRANSPORT_CAP_S") {
            pool.cooldown_transport_cap = Duration::from_secs(s);
        }
        // Lease TTL = the slot silence window (Go MIMO2API_LEASE_TTL_S;
        // the pool's 120s default applies when unset). Zero disables
        // the reaper.
        if let Some(s) = secs_var(vars, "MIMO_RUST_LEASE_TTL_S") {
            pool.slot_ttl = Duration::from_secs(s);
        }

        // Egress labels: MIMO_RUST_EGRESS_LABELS lists them
        // (comma-separated, host-injected), each label's proxy URL read
        // from MIMO_RUST_EGRESS_<LABEL>. An env-probing closure cannot
        // enumerate keys, so labels are declared, never guessed.
        let mut egress = HashMap::new();
        if let Some(list) = vars("MIMO_RUST_EGRESS_LABELS") {
            for label in list.split(',').map(str::trim).filter(|l| !l.is_empty()) {
                // Env keys are uppercased: label "jp" →
                // MIMO_RUST_EGRESS_JP (the label itself stays lowercase).
                let key = format!("MIMO_RUST_EGRESS_{}", label.to_uppercase());
                if let Some(u) = vars(&key).and_then(|v| url::Url::parse(v.trim()).ok()) {
                    egress.insert(label.to_string(), u);
                }
            }
        }
        login.egress = egress.clone();

        Config {
            db_path,
            base,
            default_model,
            thinking_default_on,
            login,
            pool,
            egress,
        }
    }
}

fn bool_var(vars: &dyn Fn(&str) -> Option<String>, key: &str, default: bool) -> bool {
    match vars(key).as_deref().map(str::trim) {
        Some("1") | Some("true") | Some("TRUE") | Some("True") => true,
        Some("0") | Some("false") | Some("FALSE") | Some("False") => false,
        _ => default,
    }
}

fn secs_var(vars: &dyn Fn(&str) -> Option<String>, key: &str) -> Option<u64> {
    vars(key).and_then(|v| v.trim().parse().ok())
}

fn int_var(vars: &dyn Fn(&str) -> Option<String>, key: &str) -> Option<i64> {
    vars(key).and_then(|v| v.trim().parse().ok())
}

#[cfg(test)]
#[path = "config_tests.rs"]
mod tests;
