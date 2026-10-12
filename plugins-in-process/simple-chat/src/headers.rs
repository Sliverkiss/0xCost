//! The android-app impersonation header table (Go `baseHeaders` + the
//! app wire constants). The User-Agent and friends are functional
//! wire-protocol values: the upstream WAF rejects a neutral UA with HTTP
//! 202 + empty body (live-verified 2026-09-19). Do not rebrand them.

use crate::device::DeviceProfile;

// Impersonated-device wire values (apk-alignment.md §2/§11): the exact
// fingerprint the DeepSeek Android 2.5.3 client emits for our device
// profile (Pixel 8 / Android 15 / en_US build / UTC+8). Functional
// wire-protocol constants — the upstream risk service scores them.
pub const APP_CLIENT_VERSION: &str = "2.5.3";
pub const APP_USER_AGENT: &str = "DeepSeek/2.5.3 Android/35";
pub const APP_CLIENT_LOCALE: &str = "en_US";
pub const APP_BUNDLE_ID: &str = "com.deepseek.chat";
pub const APP_TIMEZONE_OFF: &str = "28800"; // +08:00, seconds
pub const APP_DEVICE_MODEL: &str = "Pixel 8";

/// The android-client impersonation headers (Go `baseHeaders`) — the
/// exact dj.java case-18 block for our device profile. The User-Agent
/// and friends are functional wire-protocol values: the upstream WAF
/// rejects a neutral UA with HTTP 202 + empty body (live-verified
/// 2026-09-19). Do not rebrand them.
pub fn base_headers(profile: &DeviceProfile) -> Vec<(&'static str, String)> {
    vec![
        ("Accept", "application/json".into()),
        ("Referer", "https://chat.deepseek.com".into()),
        ("User-Agent", APP_USER_AGENT.into()),
        ("x-client-platform", "android".into()),
        ("x-client-version", APP_CLIENT_VERSION.into()),
        ("x-client-locale", APP_CLIENT_LOCALE.into()),
        ("x-client-bundle-id", APP_BUNDLE_ID.into()),
        ("x-client-timezone-offset", APP_TIMEZONE_OFF.into()),
        ("x-device-model", APP_DEVICE_MODEL.into()),
        ("x-device-id", profile.header_device_id.clone()),
        ("x-rangers-id", profile.rangers_id.clone()),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::device::resolve_device_profile;

    #[test]
    fn base_headers_complete_and_ordered() {
        let profile = resolve_device_profile("", "138", "");
        let headers = base_headers(&profile);
        let names: Vec<&str> = headers.iter().map(|(k, _)| *k).collect();
        assert_eq!(
            names,
            [
                "Accept",
                "Referer",
                "User-Agent",
                "x-client-platform",
                "x-client-version",
                "x-client-locale",
                "x-client-bundle-id",
                "x-client-timezone-offset",
                "x-device-model",
                "x-device-id",
                "x-rangers-id"
            ]
        );
        let get = |k: &str| {
            headers
                .iter()
                .find(|(name, _)| *name == k)
                .map(|(_, v)| v.clone())
                .unwrap()
        };
        assert_eq!(get("User-Agent"), "DeepSeek/2.5.3 Android/35");
        assert_eq!(get("x-client-version"), "2.5.3");
        assert_eq!(get("x-client-timezone-offset"), "28800");
        assert_eq!(get("x-device-model"), "Pixel 8");
        // Android: header id = body device id; rangers is a minted UUID.
        assert_eq!(get("x-device-id"), profile.device_id);
        assert!(get("x-rangers-id").contains('-'));
        // Web: header id is the distinct minted UUID, body id is the
        // harvested Shumei id.
        let web = resolve_device_profile("B-shumei", "a@b.io", "web");
        let headers = base_headers(&web);
        let hdr_id = headers
            .iter()
            .find(|(k, _)| *k == "x-device-id")
            .map(|(_, v)| v.clone())
            .unwrap();
        assert_ne!(hdr_id, "B-shumei");
        assert_eq!(hdr_id, web.header_device_id);
    }
}
