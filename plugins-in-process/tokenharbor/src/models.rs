//! Model resolution: `/models` RSC snapshot parsing + free-tier policy
//! (dossier §7/§8/§9, risk #2: **never hardcode free model ids** — the free
//! table churns weekly; promo windows like `claude-haiku-5.5:free` expire,
//! reference defaults like `th-rudder` disappear from the site entirely).
//!
//! Sources of truth, layered:
//! 1. `/models` RSC snapshot (anonymous, cheap) — the public table: rows
//!    with `isFree:true`, each with `surface` (carries the `:free` suffix),
//!    `label`, `freeUntil` (promo expiry ISO-8601, or null = permanent),
//!    `family`, and page-level `serverNow`.
//! 2. `/api/me/free-tier` `model_ids` (authenticated) — the per-account
//!    authoritative entitlement set (surface ids, no `:free` suffix; may
//!    include retired legacy models like `th-rudder` that the site no
//!    longer lists — dossier MODELS.md).
//!
//! The full model id the stream API expects follows the reference's
//! `{provider}/{surface}` convention, mapped from the RSC `family` field
//! (familyFromModelId). The one deviation: deepseek free ids ride the
//! `vercel-ai-gateway/deepseek/` sub-path (reference-confirmed hypothesis,
//! dossier open-questions #7 — see PROTOCOL.md).

use crate::store::{FreeTier, ModelRow};
use chrono::{DateTime, Utc};

/// Parse the `/models` RSC payload into model rows.
///
/// The RSC wire format is a sequence of newline-delimited chunks. Model
/// rows appear in two shapes across captures (both live-observed):
/// - bare per-line JSON objects carrying `surface` (small captures);
/// - one large RSC row line where row objects sit inside `rows` /
///   `freeRows` arrays of a bigger React payload.
///
/// The parser therefore scans for `{"surface"...}` objects anywhere in
/// the payload, bracket-matching each to a complete JSON object, and
/// dedupes by surface. Unknown fields and unparseable regions are
/// skipped. `serverNow` is picked up from its bare JSON line or from the
/// big payload line.
pub fn parse_rsc_snapshot(payload: &str) -> Vec<ModelRow> {
    let mut rows: Vec<ModelRow> = Vec::new();
    let mut seen: std::collections::BTreeSet<String> = std::collections::BTreeSet::new();
    for obj in scan_surface_objects(payload) {
        let surface = match obj.get("surface").and_then(|v| v.as_str()) {
            Some(s) => s,
            None => continue,
        };
        if !seen.insert(surface.to_string()) {
            continue;
        }
        rows.push(ModelRow {
            full_id: full_model_id(
                obj.get("family").and_then(|v| v.as_str()).unwrap_or(""),
                surface,
            ),
            surface: surface.to_string(),
            label: obj
                .get("label")
                .and_then(|v| v.as_str())
                .unwrap_or(surface)
                .to_string(),
            is_free: obj.get("isFree").and_then(|v| v.as_bool()).unwrap_or(false),
            free_until: obj
                .get("freeUntil")
                .and_then(|v| v.as_str())
                .map(str::to_string),
        });
    }
    rows
}

/// Scan a payload for balanced `{"surface"...}` JSON objects.
///
/// Bracket matching is JSON-aware enough for this payload family (the
/// RSC chunk stream is machine-generated; braces do not appear unescaped
/// inside its strings). Objects failing to parse are skipped — a partial
/// write or an unexpected shape must not kill the catalog.
fn scan_surface_objects(payload: &str) -> Vec<serde_json::Value> {
    let mut objects = Vec::new();
    let bytes = payload.as_bytes();
    let needle = b"{\"surface\"";
    let mut from = 0;
    while let Some(rel) = find_from(bytes, needle, from) {
        let start = from + rel;
        // Bracket-match the object starting at `start`.
        let mut depth: i32 = 0;
        let mut in_string = false;
        let mut escape = false;
        let mut end = None;
        for (i, &b) in bytes.iter().enumerate().skip(start) {
            let c = b as char;
            if escape {
                escape = false;
                continue;
            }
            match c {
                '\\' if in_string => escape = true,
                '"' => in_string = !in_string,
                '{' if !in_string => depth += 1,
                '}' if !in_string => {
                    depth -= 1;
                    if depth == 0 {
                        end = Some(i);
                        break;
                    }
                }
                _ => {}
            }
        }
        if let Some(end) = end {
            if let Ok(v) = serde_json::from_slice(&bytes[start..=end]) {
                objects.push(v);
            }
            from = end + 1;
        } else {
            // Unterminated object: nothing more to find.
            break;
        }
    }
    objects
}

/// Find `needle` in `hay` at/after `from` (byte-oriented).
fn find_from(hay: &[u8], needle: &[u8], from: usize) -> Option<usize> {
    if from >= hay.len() || needle.is_empty() {
        return None;
    }
    hay[from..].windows(needle.len()).position(|w| w == needle)
}

/// Extract the page-level `serverNow` (ISO-8601) from an RSC payload:
/// either its bare JSON line or the big payload line.
pub fn parse_server_now(payload: &str) -> Option<String> {
    let marker = "\"serverNow\":";
    let i = payload.find(marker)?;
    let rest = &payload[i + marker.len()..];
    // Skip whitespace, expect a quoted string.
    let rest = rest.trim_start();
    let rest = rest.strip_prefix('"')?;
    let end = rest.find('"')?;
    Some(rest[..end].to_string())
}

/// Rows in the snapshot that are free RIGHT NOW (per `serverNow` when
/// parseable, else wall clock): `isFree:true` and, when `freeUntil` is set,
/// the promo window has not closed.
pub fn free_models_now(
    rows: &[ModelRow],
    server_now: Option<&str>,
    now: DateTime<Utc>,
) -> Vec<ModelRow> {
    let server_now = server_now
        .and_then(|s| DateTime::parse_from_rfc3339(s).ok())
        .map(|d| d.with_timezone(&Utc));
    let reference = server_now.unwrap_or(now);
    rows.iter()
        .filter(|row| {
            if !row.is_free {
                return false;
            }
            match &row.free_until {
                None => true, // permanent free
                Some(expiry) => DateTime::parse_from_rfc3339(expiry)
                    .map(|d| d.with_timezone(&Utc) > reference)
                    .unwrap_or(false),
            }
        })
        .cloned()
        .collect()
}

/// `{provider}/{surface}` full id (reference familyFromModelId mapping).
fn full_model_id(family: &str, surface: &str) -> String {
    match family {
        // Deepseek free ids ride the vercel-ai-gateway sub-path; paid
        // deepseek surfaces take the plain family prefix (reference rule).
        "deepseek" if surface.ends_with(":free") => {
            format!("vercel-ai-gateway/deepseek/{surface}")
        }
        other => format!("{other}/{surface}"),
    }
}

/// Resolve the servable free-model set for one account:
/// the public free table (now-valid) INTERSECTED with the account's
/// entitlement set, plus entitlements whose `:free` variant exists in the
/// public table (entitlement ids lack the `:free` suffix).
///
/// Rationale (dossier MODELS.md): entitlements are authoritative per
/// account (a model not in `free-tier.model_ids` will not stream free),
/// while the public table tells us which surfaces are currently free and
/// the exact full id (provider path + `:free` suffix) the stream API
/// expects. Legacy entitlements with no live free row (e.g. `th-rudder`)
/// are dropped — they 403 `model_unavailable` today.
pub fn servable_free_models(
    rows: &[ModelRow],
    server_now: Option<&str>,
    now: DateTime<Utc>,
    tier: &FreeTier,
) -> Vec<ModelRow> {
    let free_now = free_models_now(rows, server_now, now);
    let entitled: std::collections::BTreeSet<&str> =
        tier.model_ids.iter().map(String::as_str).collect();
    // Entitlement id `x` matches free row surface `x:free` (or bare `x`).
    free_now
        .into_iter()
        .filter(|row| {
            let base = row.surface.strip_suffix(":free").unwrap_or(&row.surface);
            entitled.contains(base) || entitled.contains(row.surface.as_str())
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    const RSC: &str = include_str!("../fixtures/models-rsc-small.txt");
    const RSC_FULL: &str = include_str!("../fixtures/models-rsc-2026-10-12.txt");

    fn tier(ids: &[&str]) -> FreeTier {
        FreeTier {
            email: "t@example.com".into(),
            model_ids: ids.iter().map(|s| s.to_string()).collect(),
            fetched_at: 0,
        }
    }

    #[test]
    fn parses_model_rows_skipping_rsc_noise() {
        let rows = parse_rsc_snapshot(RSC);
        // small fixture: 2 paid rows + 3 free rows.
        assert_eq!(rows.len(), 5, "{rows:?}");
        assert_eq!(rows[0].surface, "claude-opus-5.5");
        assert!(!rows[0].is_free);
        assert_eq!(rows[2].surface, "claude-haiku-5.5:free");
        assert!(rows[2].is_free);
        assert_eq!(
            rows[2].free_until.as_deref(),
            Some("2026-10-15T13:00:00+00:00")
        );
    }

    #[test]
    fn parses_server_now() {
        assert_eq!(
            parse_server_now(RSC).as_deref(),
            Some("2026-10-12T05:30:18.826Z")
        );
    }

    #[test]
    fn full_id_provider_paths() {
        assert_eq!(
            full_model_id("xiaomi", "mimo-v2.6-flash:free"),
            "xiaomi/mimo-v2.6-flash:free"
        );
        assert_eq!(
            full_model_id("deepseek", "deepseek-v4.1-flash:free"),
            "vercel-ai-gateway/deepseek/deepseek-v4.1-flash:free"
        );
        assert_eq!(
            full_model_id("deepseek", "deepseek-v4.1-flash"),
            "deepseek/deepseek-v4.1-flash"
        );
        assert_eq!(
            full_model_id("anthropic", "claude-opus-5.5"),
            "anthropic/claude-opus-5.5"
        );
    }

    #[test]
    fn free_models_now_respects_promo_window() {
        let rows = parse_rsc_snapshot(RSC);
        let server_now = parse_server_now(RSC);
        // Snapshot day: haiku promo still open.
        let snap_day = DateTime::parse_from_rfc3339("2026-10-12T05:30:18Z")
            .unwrap()
            .with_timezone(&Utc);
        let free = free_models_now(&rows, server_now.as_deref(), snap_day);
        let surfaces: Vec<&str> = free.iter().map(|r| r.surface.as_str()).collect();
        assert_eq!(
            surfaces,
            vec![
                "claude-haiku-5.5:free",
                "deepseek-v4.1-flash:free",
                "mimo-v2.6-flash:free"
            ]
        );

        // After the promo closes only the two permanent rows remain.
        let after = DateTime::parse_from_rfc3339("2026-10-16T00:00:00Z")
            .unwrap()
            .with_timezone(&Utc);
        let free = free_models_now(&rows, None, after);
        let surfaces: Vec<&str> = free.iter().map(|r| r.surface.as_str()).collect();
        assert_eq!(
            surfaces,
            vec!["deepseek-v4.1-flash:free", "mimo-v2.6-flash:free"]
        );
    }

    #[test]
    fn entitlement_intersection_drops_unentitled_and_legacy() {
        let rows = parse_rsc_snapshot(RSC);
        let server_now = parse_server_now(RSC);
        let now = DateTime::parse_from_rfc3339("2026-10-12T05:30:18Z")
            .unwrap()
            .with_timezone(&Utc);

        // Recon-account-shaped entitlements: includes legacy th-rudder, not
        // entitled to the haiku promo.
        let t = tier(&["deepseek-v4.1-flash", "mimo-v2.6-flash", "th-rudder"]);
        let servable = servable_free_models(&rows, server_now.as_deref(), now, &t);
        let ids: Vec<&str> = servable.iter().map(|r| r.full_id.as_str()).collect();
        assert_eq!(
            ids,
            vec![
                "vercel-ai-gateway/deepseek/deepseek-v4.1-flash:free",
                "xiaomi/mimo-v2.6-flash:free",
            ],
            "haiku dropped (not entitled), th-rudder dropped (no live free row)"
        );
    }

    #[test]
    fn full_snapshot_yields_expected_free_table() {
        // Dossier MODELS.md 2026-10-12: exactly 3 free rows.
        let rows = parse_rsc_snapshot(RSC_FULL);
        let free = free_models_now(&rows, parse_server_now(RSC_FULL).as_deref(), Utc::now());
        assert_eq!(free.len(), 3, "{free:?}");
        assert!(free.iter().any(|r| r.surface == "claude-haiku-5.5:free"));
        assert!(free.iter().any(|r| r.surface == "deepseek-v4.1-flash:free"));
        assert!(free.iter().any(|r| r.surface == "mimo-v2.6-flash:free"));
    }
}
