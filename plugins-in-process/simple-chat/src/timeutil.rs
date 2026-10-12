//! RFC3339 UTC time helpers — the only timestamp format the SQLite schema
//! accepts (PORT_DESIGN §2.1: Go writes `time.RFC3339` in `applyParkRecord`).
//!
//! No chrono dependency: a civil-date conversion (Howard Hinnant's algorithm)
//! is all the format needs. Parsing is deliberately Go-`time.Parse`-shaped:
//! `YYYY-MM-DDTHH:MM:SS` + `Z` or `±HH:MM`, optional fractional seconds.
//! Anything else is `None` — a corrupt `park_until` must parse as *unknown*,
//! never panic (RISK_REGISTER R3 quarantine class).

use std::time::{Duration, SystemTime, UNIX_EPOCH};

// Helpers serving the store/client modules (next commits) are exercised today
// only by tests; per-item allows keep the dead-code lint honest about intent.

/// Days from 1970-01-01 for the given proleptic-Gregorian date (Hinnant).
fn days_from_civil(y: i64, m: u32, d: u32) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y.rem_euclid(400);
    let mp = ((m + 9) % 12) as i64;
    let doy = (153 * mp + 2) / 5 + (d as i64) - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

/// Civil date for the days since 1970-01-01 (inverse of `days_from_civil`).
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let y = if m <= 2 { y + 1 } else { y };
    (y, m, d)
}

/// True when `y` is a leap year (needed only by tests via round-trips).
#[cfg(test)]
fn is_leap(y: i64) -> bool {
    (y % 4 == 0 && y % 100 != 0) || y % 400 == 0
}

/// (year, month, day, hour, min, sec) for a count of seconds since the epoch.
fn civil_from_secs(secs: i64) -> (i64, u32, u32, u32, u32, u32) {
    let days = secs.div_euclid(86_400);
    let rem = secs.rem_euclid(86_400);
    let (y, m, d) = civil_from_days(days);
    (
        y,
        m,
        d,
        (rem / 3600) as u32,
        ((rem % 3600) / 60) as u32,
        (rem % 60) as u32,
    )
}

/// Count of seconds since the epoch for a civil date-time.
fn secs_from_civil(y: i64, m: u32, d: u32, hh: u32, mm: u32, ss: u32) -> i64 {
    days_from_civil(y, m, d) * 86_400 + hh as i64 * 3600 + mm as i64 * 60 + ss as i64
}

/// Parse one RFC3339 timestamp (`Z` or `±HH:MM` offset, optional fraction).
/// Go's `time.Parse(time.RFC3339, …)` parity, minus panic paths.
pub fn parse_rfc3339(s: &str) -> Option<SystemTime> {
    let bytes = s.as_bytes();
    if bytes.len() < 20 {
        return None;
    }
    let num = |from: usize, len: usize| -> Option<i64> {
        let part = s.get(from..from + len)?;
        if !part.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        part.parse().ok()
    };
    if bytes[4] != b'-'
        || bytes[7] != b'-'
        || bytes[10] != b'T'
        || bytes[13] != b':'
        || bytes[16] != b':'
    {
        return None;
    }
    let y = num(0, 4)?;
    let mo = num(5, 2)? as u32;
    let d = num(8, 2)? as u32;
    let hh = num(11, 2)? as u32;
    let mi = num(14, 2)? as u32;
    let ss = num(17, 2)? as u32;
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || hh > 23 || mi > 59 || ss > 60 {
        return None;
    }
    let mut idx = 19;
    // Optional fractional seconds — accepted and ignored (whole-second store).
    if idx < bytes.len() && bytes[idx] == b'.' {
        idx += 1;
        let start = idx;
        while idx < bytes.len() && bytes[idx].is_ascii_digit() {
            idx += 1;
        }
        if idx == start {
            return None;
        }
    }
    // Offset: `Z`/`z` or ±HH:MM.
    let offset_secs: i64 = match bytes.get(idx) {
        Some(b'Z') | Some(b'z') => {
            idx += 1;
            0
        }
        Some(sign @ (b'+' | b'-')) => {
            if bytes.len() < idx + 6 || bytes[idx + 3] != b':' {
                return None;
            }
            let oh = num(idx + 1, 2)?;
            let om = num(idx + 4, 2)?;
            if oh > 23 || om > 59 {
                return None;
            }
            let mag = oh * 3600 + om * 60;
            idx += 6;
            if *sign == b'-' {
                -mag
            } else {
                mag
            }
        }
        _ => return None,
    };
    if idx != bytes.len() {
        return None;
    }
    let utc = secs_from_civil(y, mo, d, hh, mi, ss) - offset_secs;
    Some(system_time_from_unix(utc))
}

/// `SystemTime` from a (possibly negative) unix-seconds count.
#[allow(dead_code)] // store/client modules land next
pub fn system_time_from_unix(secs: i64) -> SystemTime {
    if secs >= 0 {
        UNIX_EPOCH + Duration::from_secs(secs as u64)
    } else {
        UNIX_EPOCH - Duration::from_secs((-secs) as u64)
    }
}

/// Unix-seconds count (floor) for a `SystemTime`.
#[allow(dead_code)] // store/client modules land next
pub fn unix_secs(t: SystemTime) -> i64 {
    match t.duration_since(UNIX_EPOCH) {
        Ok(d) => d.as_secs() as i64,
        Err(e) => -(e.duration().as_secs() as i64),
    }
}

/// Format a unix-seconds count as RFC3339 UTC with a `Z` suffix
/// (`time.RFC3339` rendering of a UTC time).
#[allow(dead_code)] // store/client modules land next
pub fn unix_to_rfc3339(secs: i64) -> String {
    let (y, mo, d, hh, mi, ss) = civil_from_secs(secs);
    format!("{y:04}-{mo:02}-{d:02}T{hh:02}:{mi:02}:{ss:02}Z")
}

/// Format a `SystemTime` as RFC3339 UTC (whole seconds).
#[allow(dead_code)] // store/client modules land next
pub fn format_rfc3339(t: SystemTime) -> String {
    unix_to_rfc3339(unix_secs(t))
}

/// Parse one `mute_until` candidate (Go `parseMuteUntilValue`): unix seconds
/// as a JSON number or decimal string, or an RFC3339 timestamp. `raw` is the
/// raw JSON value text (e.g. `1750000000`, `"1750000000"`, `"2026-…"`,
/// `null`, or an empty string).
#[allow(dead_code)] // store/client modules land next
pub fn parse_mute_until_value(raw: &str) -> Option<SystemTime> {
    let s = raw.trim().trim_matches('"');
    if s.is_empty() || s == "null" {
        return None;
    }
    if let Ok(secs) = s.parse::<i64>() {
        return Some(system_time_from_unix(secs));
    }
    parse_rfc3339(s)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_and_parses_round_trip() {
        for secs in [
            0i64,
            1,
            86_399,
            1_700_000_000,
            951_782_400,
            1_800_000_000,
            -1,
        ] {
            let s = unix_to_rfc3339(secs);
            let t = parse_rfc3339(&s).unwrap_or_else(|| panic!("parse {s}"));
            assert_eq!(unix_secs(t), secs, "round-trip {s}");
        }
    }

    #[test]
    fn parses_offsets_and_fractions() {
        // +08:00 offset = 28800 seconds behind UTC on the clock.
        assert_eq!(
            parse_rfc3339("2026-10-12T08:00:00+08:00").map(unix_secs),
            Some(unix_secs(parse_rfc3339("2026-10-12T00:00:00Z").unwrap()))
        );
        assert_eq!(
            parse_rfc3339("2026-10-12T00:00:00-01:30").map(unix_secs),
            Some(unix_secs(parse_rfc3339("2026-10-12T01:30:00Z").unwrap()))
        );
        // Fractional seconds accepted, whole-second store.
        assert_eq!(
            parse_rfc3339("2026-10-12T00:00:00.123Z").map(unix_secs),
            Some(unix_secs(parse_rfc3339("2026-10-12T00:00:00Z").unwrap()))
        );
    }

    #[test]
    fn rejects_corrupt_timestamps() {
        for bad in [
            "",
            "not-a-date",
            "2026-10-12",
            "2026-10-12T00:00:00",
            "2026-13-12T00:00:00Z",
            "2026-10-32T00:00:00Z",
            "2026-10-12T25:00:00Z",
            "2026-10-12T00:60:00Z",
            "2026-10-12T00:00:00Z ",
            "2026-10-12T00:00:00+8:00",
            "2026/10/12T00:00:00Z",
        ] {
            assert!(parse_rfc3339(bad).is_none(), "expected reject: {bad:?}");
        }
    }

    #[test]
    fn civil_conversions_agree_with_leap_years() {
        // 2024-02-29 exists; 2023-02-29 does not (day 60 in 2023 is 03-01).
        assert_eq!(
            days_from_civil(2024, 2, 29),
            days_from_civil(2024, 3, 1) - 1
        );
        assert_eq!(
            days_from_civil(2023, 2, 28),
            days_from_civil(2023, 3, 1) - 1
        );
        assert!(is_leap(2024));
        assert!(!is_leap(2023));
        assert!(!is_leap(1900));
        assert!(is_leap(2000));
    }

    #[test]
    fn epoch_anchors() {
        assert_eq!(unix_to_rfc3339(0), "1970-01-01T00:00:00Z");
        assert_eq!(unix_to_rfc3339(1_700_000_000), "2023-11-14T22:13:20Z");
        assert_eq!(unix_to_rfc3339(951_782_400), "2000-02-29T00:00:00Z");
    }

    #[test]
    fn mute_until_value_candidates() {
        // Unix seconds, bare number or quoted.
        assert_eq!(
            parse_mute_until_value("1750000000").map(unix_secs),
            Some(1_750_000_000)
        );
        assert_eq!(
            parse_mute_until_value("\"1750000000\"").map(unix_secs),
            Some(1_750_000_000)
        );
        // RFC3339.
        assert_eq!(
            parse_mute_until_value("\"2026-10-13T00:00:00Z\"").map(unix_secs),
            parse_rfc3339("2026-10-13T00:00:00Z").map(unix_secs)
        );
        // Null / garbage.
        assert!(parse_mute_until_value("null").is_none());
        assert!(parse_mute_until_value("").is_none());
        assert!(parse_mute_until_value("garbage").is_none());
    }
}
