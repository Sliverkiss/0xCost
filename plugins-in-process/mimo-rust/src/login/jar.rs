//! Manual cookie jar + device identity (Go otp.go jar helpers + device.go).
//!
//! The jar is a name→value map with sorted-key rendering and last-wins
//! merge (browser cookie-store semantics); RFC 6265 domain/path/expiry are
//! deliberately not modeled — the passport chain is same-host
//! (PORT_DESIGN §1.2).

use std::collections::BTreeMap;

use crate::login::Client;

/// Device-identity cookies: a `BTreeMap<String, String>` per account row
/// (PORT_DESIGN §1.2). Serialized as a JSON object by the store layer.
pub type Device = BTreeMap<String, String>;

/// The flow jar (Go map[string]string).
pub type Jar = BTreeMap<String, String>;

/// Device cookie whitelist (device.go:33). Session cookies are deliberately
/// excluded: every STS exchange rewrites them and they must not be carried
/// into a later login.
pub const DEVICE_COOKIE_NAMES: [&str; 5] =
    ["deviceId", "pass_ua", "uLocale", "cUserId", "sdkVersion"];

/// Render the jar as a Cookie header, sorted keys (Go cookieHeader,
/// otp.go:140-159). Deterministic across map iterations.
pub fn cookie_header(jar: &Jar) -> String {
    if jar.is_empty() {
        return String::new();
    }
    jar.iter()
        .map(|(k, v)| format!("{k}={v}"))
        .collect::<Vec<_>>()
        .join("; ")
}

/// Merge `b` over `a` (b wins) and return a new jar (Go mergeJars,
/// passport.go:170-178). Never mutates the inputs.
pub fn merge_jars(a: &Jar, b: &Jar) -> Jar {
    let mut out = a.clone();
    out.extend(b.iter().map(|(k, v)| (k.clone(), v.clone())));
    out
}

/// Strip the surrounding quotes passport cookies carry (Go unquoteCookie,
/// login.go:367-373). Repeatedly, like the Go loop.
pub fn unquote_cookie(v: &str) -> String {
    let mut v = v.trim();
    while v.len() >= 2 && v.starts_with('"') && v.ends_with('"') {
        v = &v[1..v.len() - 1];
    }
    v.to_string()
}

/// Turn Set-Cookie headers into a name→value map (Go collectCookies,
/// login.go:376-396): split on the first `;`, name = trimmed prefix before
/// `=`, skip empty or `""` values.
pub fn collect_cookies(headers: &reqwest::header::HeaderMap) -> Jar {
    let mut out = Jar::new();
    for sc in headers.get_all(reqwest::header::SET_COOKIE) {
        let Some(sc) = sc.to_str().ok() else { continue };
        let head = sc.split(';').next().unwrap_or("");
        let Some(eq) = head.find('=') else {
            continue;
        };
        if eq == 0 {
            continue;
        }
        let name = head[..eq].trim();
        let val = head[eq + 1..].trim();
        if name.is_empty() || val.is_empty() || val == "\"\"" {
            continue;
        }
        out.insert(name.to_string(), val.to_string());
    }
    out
}

/// Copy the device-identity cookies out of a chain jar (Go harvestDevice,
/// device.go:38-49). Empty map when the jar holds none — an account that
/// has never logged in stays JSON-clean.
pub fn harvest_device(jar: &Jar) -> Device {
    let mut out = Device::new();
    for k in DEVICE_COOKIE_NAMES {
        if let Some(v) = jar.get(k) {
            if !v.is_empty() {
                out.insert(k.to_string(), v.clone());
            }
        }
    }
    out
}

/// Merge a fresh device identity over the stored one (Go MergeDevice,
/// device.go:56-75): server-issued values win, values it stayed silent
/// about survive (a login that plants nothing must NOT wipe the stored
/// identity), both-empty → empty (JSON-clean account). Never aliases the
/// stored map.
pub fn merge_device(stored: &Device, fresh: &Device) -> Device {
    let mut out = Device::new();
    for (k, v) in stored.iter().chain(fresh.iter()) {
        if !v.is_empty() {
            out.insert(k.clone(), v.clone());
        }
    }
    out
}

/// Jar pre-seeded with the device identity (Go seedDeviceJar, device.go:80-88)
/// so the whole SSO chain presents the same device a browser would.
pub fn seed_device_jar(device: &Device) -> Jar {
    device
        .iter()
        .filter(|(_, v)| !v.is_empty())
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect()
}

/// Render the device cookies for a request outside the SSO jar (Go
/// deviceCookieHeader, device.go:94-118): whitelist order first, then any
/// stored extras sorted — deterministic for captures and tests.
pub fn device_cookie_header(device: &Device) -> String {
    if device.is_empty() {
        return String::new();
    }
    let mut parts: Vec<String> = Vec::with_capacity(device.len());
    for k in DEVICE_COOKIE_NAMES {
        if let Some(v) = device.get(k) {
            if !v.is_empty() {
                parts.push(format!("{k}={v}"));
            }
        }
    }
    // Names outside the whitelist still belong to the device blob (a future
    // passport cookie) — append sorted.
    if parts.len() != device.len() {
        let mut extra: Vec<String> = device
            .iter()
            .filter(|(k, v)| !v.is_empty() && !DEVICE_COOKIE_NAMES.contains(&k.as_str()))
            .map(|(k, v)| format!("{k}={v}"))
            .collect();
        extra.sort();
        parts.extend(extra);
    }
    parts.join("; ")
}

/// Resolve a redirect target and enforce the host allowlist (Go
/// resolvePassportLocation, passport.go:150-164): a hostile Location must
/// not send the client (and its cookies) elsewhere.
pub(crate) fn resolve_passport_location(
    client: &Client,
    base: &str,
    loc: &str,
) -> Result<String, String> {
    let bu = url::Url::parse(base).map_err(|e| format!("base parse: {e}"))?;
    let lu = url::Url::parse(loc).map_err(|e| format!("location parse: {e}"))?;
    let abs = bu.join(lu.as_str()).map_err(|e| format!("resolve: {e}"))?;
    let host = abs.host_str().unwrap_or_default();
    if !client.host_allowed(host) {
        return Err(format!(
            "login page redirect host {host:?} not in allowlist"
        ));
    }
    Ok(abs.to_string())
}
