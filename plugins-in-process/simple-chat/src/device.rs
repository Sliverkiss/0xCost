//! Device identity minting — byte-identical port of Go `region.go` (§3.4 of
//! PORT_DESIGN).
//!
//! Per-account, deterministic:
//! - `synthetic_android_id` = `hex(sha256("simple-chat/device-id/" + identity))[..16]`
//! - `device_id` = `base64(AES-128-CBC/PKCS7(android_id + "_" + "google",
//!   key = MD5("com.deepseek.chat"), iv = zeros(16)))`
//! - `rangers_id` = `uuidv5(ns, "simple-chat/rangers-id/" + identity)`
//! - web channel: `x-device-id` header = `uuidv5(ns,
//!   "simple-chat/web-header-device-id/" + identity)`; android: header id =
//!   body device id.
//!
//! The fixed UUIDv5 namespace isolates ids minted by this deployment from
//! UUIDv5s under standard namespaces.

use aes::cipher::{BlockDecryptMut, BlockEncryptMut, KeyIvInit};
use md5::Digest;
use sha2::Sha256;

// Everything below serves the store (boot-time fingerprint mint) and wire
// client modules that land in the next commits; today only tests exercise
// them, so per-item allows keep the dead-code lint honest about intent.

#[allow(dead_code)] // store/client modules land next
type Aes128CbcEnc = cbc::Encryptor<aes::Aes128>;
#[allow(dead_code)] // store/client modules land next
type Aes128CbcDec = cbc::Decryptor<aes::Aes128>;

/// Fixed UUIDv5 namespace (Go `deviceIDNamespace`, `region.go`).
pub const DEVICE_ID_NAMESPACE: [u8; 16] = [
    0xa5, 0xc3, 0xf7, 0xe1, 0x2b, 0x4d, 0x4f, 0x6a, 0x8c, 0x91, 0x0d, 0x3e, 0x5f, 0x7a, 0x9b, 0x2c,
];

#[allow(dead_code)] // client module lands next (login/completion headers)
const APP_PACKAGE_NAME: &str = "com.deepseek.chat";
#[allow(dead_code)] // client module lands next (login/completion headers)
const APP_DEVICE_BRAND: &str = "google";

/// RFC 4122 version-5 (SHA-1 name-based) UUID of `name` under `ns`.
#[allow(dead_code)] // store/client modules land next
fn uuidv5(ns: [u8; 16], name: &str) -> [u8; 16] {
    let mut h = sha1::Sha1::new();
    h.update(ns);
    h.update(name.as_bytes());
    let digest = h.finalize();
    let mut u = [0u8; 16];
    u.copy_from_slice(&digest[..16]);
    u[6] = (u[6] & 0x0f) | 0x50; // version 5
    u[8] = (u[8] & 0x3f) | 0x80; // RFC 4122 variant
    u
}

/// Render 16 UUID bytes in the canonical 8-4-4-4-12 lowercase layout.
#[allow(dead_code)] // store/client modules land next
fn format_uuid(b: [u8; 16]) -> String {
    let hex = |slice: &[u8]| -> String { slice.iter().map(|byte| format!("{byte:02x}")).collect() };
    format!(
        "{}-{}-{}-{}-{}",
        hex(&b[0..4]),
        hex(&b[4..6]),
        hex(&b[6..8]),
        hex(&b[8..10]),
        hex(&b[10..16])
    )
}

/// MD5 of the app package name — the AES key the app uses (Go `appDeviceKey`).
#[allow(dead_code)] // store/client modules land next
fn app_device_key() -> [u8; 16] {
    let mut hasher = md5::Md5::new();
    hasher.update(APP_PACKAGE_NAME.as_bytes());
    let sum: [u8; 16] = hasher.finalize().into();
    sum
}

/// Stable 16-hex-char ANDROID_ID derived from the account identity
/// (Go `syntheticAndroidID`): `hex(sha256("simple-chat/device-id/" + id))[..8 bytes]`.
#[allow(dead_code)] // store/client modules land next
fn synthetic_android_id(identity: &str) -> String {
    let mut h = Sha256::new();
    h.update(format!("simple-chat/device-id/{identity}"));
    let sum = h.finalize();
    let mut out = String::with_capacity(16);
    for byte in &sum[..8] {
        out.push_str(&format!("{byte:02x}"));
    }
    out
}

/// The app's mint: AES-128-CBC/PKCS7 encrypt of `ANDROID_ID + "_" + brand`
/// under MD5(pkg) with a zero IV, base64 (std) encoded (Go `mintAppDeviceID`).
#[allow(dead_code)] // store/client modules land next
pub fn mint_app_device_id(android_id: &str, brand: &str) -> String {
    use aes::cipher::block_padding::Pkcs7;
    use base64::Engine;
    let ct = Aes128CbcEnc::new(&app_device_key().into(), &[0u8; 16].into())
        .encrypt_padded_vec_mut::<Pkcs7>(format!("{android_id}_{brand}").as_bytes());
    base64::engine::general_purpose::STANDARD.encode(ct)
}

/// Inverse of [`mint_app_device_id`] — the test seam that proves the mint is
/// the app's construction, not just a base64 blob (Go `decryptAppDeviceID`).
#[allow(dead_code)] // test seam; store/client modules land next
pub fn decrypt_app_device_id(id: &str) -> Option<String> {
    use aes::cipher::block_padding::Pkcs7;
    use base64::Engine;
    let raw = base64::engine::general_purpose::STANDARD.decode(id).ok()?;
    if raw.is_empty() || raw.len() % 16 != 0 {
        return None;
    }
    let plain = Aes128CbcDec::new(&app_device_key().into(), &[0u8; 16].into())
        .decrypt_padded_vec_mut::<Pkcs7>(&raw)
        .ok()?;
    String::from_utf8(plain).ok()
}

/// The account's login-wire device id: the explicit value verbatim, else the
/// deterministic mint over the identity (Go `ResolveDeviceID`).
#[allow(dead_code)] // store/client modules land next
pub fn resolve_device_id(explicit: &str, identity: &str) -> String {
    let explicit = explicit.trim();
    if !explicit.is_empty() {
        return explicit.to_string();
    }
    let identity = identity.trim();
    if identity.is_empty() {
        // Validate() rejects such accounts upstream; keep parity with Go's
        // random fallback by minting over a fresh random id.
        let mut b = [0u8; 16];
        use rand::RngCore;
        rand::thread_rng().fill_bytes(&mut b);
        let synthetic: String = b.iter().map(|byte| format!("{byte:02x}")).collect();
        return mint_app_device_id(&synthetic, APP_DEVICE_BRAND);
    }
    mint_app_device_id(&synthetic_android_id(identity), APP_DEVICE_BRAND)
}

/// The account's full wire device profile (Go `resolveDeviceProfile`):
/// `rangers_id` plus the `x-device-id` header value, which is the body device
/// id on android and a distinct minted UUID on the web channel.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DeviceProfile {
    /// Login-body device id (explicit or minted).
    pub device_id: String,
    /// `x-rangers-id` header value (APM install id, minted UUID).
    pub rangers_id: String,
    /// `x-device-id` header value.
    pub header_device_id: String,
    /// `"web"` or `""` (android) — drives the login body `os` value.
    pub channel: String,
}

impl DeviceProfile {
    /// Login body `os` value for this channel (Go `channelOS`).
    pub fn channel_os(&self) -> &'static str {
        if self.channel == "web" {
            "web"
        } else {
            "android"
        }
    }
}

/// Mint the device profile for one account (Go `resolveDeviceProfile`).
#[allow(dead_code)] // store/client modules land next
pub fn resolve_device_profile(device_id: &str, identity: &str, channel: &str) -> DeviceProfile {
    let device_id = device_id.trim().to_string();
    let identity = identity.trim();
    // Identity for header minting: mobile, else email, else the device id.
    let mint_identity = if identity.is_empty() {
        device_id.as_str()
    } else {
        identity
    };
    let rangers = format_uuid(uuidv5(
        DEVICE_ID_NAMESPACE,
        &format!("simple-chat/rangers-id/{mint_identity}"),
    ));
    if channel.trim() == "web" {
        let header = format_uuid(uuidv5(
            DEVICE_ID_NAMESPACE,
            &format!("simple-chat/web-header-device-id/{mint_identity}"),
        ));
        DeviceProfile {
            device_id,
            rangers_id: rangers,
            header_device_id: header,
            channel: "web".into(),
        }
    } else {
        let header_device_id = device_id.clone();
        DeviceProfile {
            device_id,
            rangers_id: rangers,
            header_device_id,
            channel: String::new(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mint_round_trips_through_decrypt() {
        for android_id in ["0123456789abcdef", "abcdef0123456789", "0000000000000000"] {
            let minted = mint_app_device_id(android_id, APP_DEVICE_BRAND);
            let plain =
                decrypt_app_device_id(&minted).unwrap_or_else(|| panic!("decrypt {android_id}"));
            assert_eq!(plain, format!("{android_id}_{APP_DEVICE_BRAND}"));
        }
    }

    #[test]
    fn mint_is_deterministic_and_shaped() {
        let a = mint_app_device_id("0123456789abcdef", "google");
        let b = mint_app_device_id("0123456789abcdef", "google");
        assert_eq!(a, b, "same inputs → same id");
        // 16-char android id + "_google" = 23 bytes → PKCS7 pads to 2 blocks
        // → 32 ciphertext bytes → 44 base64 chars.
        assert_eq!(a.len(), 44);
        // Short input pads to one block → 24 base64 chars.
        let short = mint_app_device_id("short", "google");
        assert_eq!(short.len(), 24);
        // Decryption rejects corrupted padding / non-block sizes.
        assert!(decrypt_app_device_id("not-base64!!").is_none());
        assert!(decrypt_app_device_id("AAAA").is_none());
    }

    #[test]
    fn synthetic_android_id_is_16_hex() {
        let id = synthetic_android_id("13800001111");
        assert_eq!(id.len(), 16);
        assert!(id.bytes().all(|b| b.is_ascii_hexdigit()));
        // Different identities → different ids (correlation guard).
        assert_ne!(id, synthetic_android_id("other@x.io"));
    }

    #[test]
    fn resolve_device_id_explicit_wins() {
        assert_eq!(resolve_device_id("B-shumei-123", "138"), "B-shumei-123");
        // Empty → deterministic mint over identity.
        let a = resolve_device_id("", "138");
        let b = resolve_device_id("", "138");
        assert_eq!(a, b);
        // Synthetic id + "_google" = 23 bytes → 2 padded blocks → 44 chars.
        assert_eq!(a.len(), 44);
        // Decrypts back to the synthetic android id.
        let plain = decrypt_app_device_id(&a).unwrap();
        assert_eq!(plain, format!("{}_google", synthetic_android_id("138")));
    }

    #[test]
    fn uuidv5_shape_and_namespace_isolation() {
        let u = uuidv5(DEVICE_ID_NAMESPACE, "simple-chat/rangers-id/138");
        assert_eq!(u[6] >> 4, 5, "version 5");
        assert_eq!(u[8] >> 6, 0b10, "RFC 4122 variant");
        let s = format_uuid(u);
        assert_eq!(s.len(), 36);
        assert_eq!(s.matches('-').count(), 4);
        assert!(s.chars().all(|c| c.is_ascii_hexdigit() || c == '-'));
        // Name changes → id changes.
        assert_ne!(u, uuidv5(DEVICE_ID_NAMESPACE, "simple-chat/rangers-id/139"));
    }

    #[test]
    fn android_profile_header_equals_body_id() {
        let body = resolve_device_id("", "138");
        let p = resolve_device_profile(&body, "138", "");
        assert_eq!(p.device_id, body);
        assert_eq!(p.header_device_id, body);
        assert_eq!(p.channel_os(), "android");
        assert!(p.rangers_id.contains('-'));
    }

    #[test]
    fn web_profile_header_is_distinct_mint() {
        let p = resolve_device_profile("B-shumei-id", "a@b.io", "web");
        assert_eq!(p.device_id, "B-shumei-id");
        assert_eq!(p.channel_os(), "web");
        // Header id is a minted UUID, NOT the Shumei id (browser parity).
        assert_ne!(p.header_device_id, "B-shumei-id");
        assert_eq!(p.header_device_id.len(), 36);
        // Deterministic per identity.
        let q = resolve_device_profile("B-shumei-id", "a@b.io", "web");
        assert_eq!(p, q);
    }
}
