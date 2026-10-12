//! Request bodies — the Go `client.go` serializers for the login and
//! completion payloads, kept separate from the response-envelope
//! semantics in [`crate::wire`] so each stays reviewable. Field order and
//! null-literal shapes are wire fingerprints: the completion body must
//! serialize in kotlinx declaration order (qj1.java:16-26), so the body
//! goes straight to JSON text — a `serde_json::Value` round-trip would
//! re-sort keys (BTreeMap) and break the fingerprint.

use crate::device::DeviceProfile;
use crate::wire::{Config, UpstreamError};

/// Login body (Go `Login`): a `map[string]any` that Go's json.Marshal
/// emits alphabetically — serde_json's default map is also alphabetical,
/// so the wire bytes match. `os` is always sent; `area_code` is a literal
/// JSON `null` on mobile logins (the app's explicitNulls encoding).
pub fn login_body(
    cfg: &Config,
    profile: &DeviceProfile,
) -> Result<serde_json::Value, UpstreamError> {
    let mut body = serde_json::Map::new();
    body.insert("password".into(), cfg.password.clone().into());
    body.insert("device_id".into(), profile.device_id.clone().into());
    body.insert("os".into(), profile.channel_os().into());
    let email = cfg.email.trim();
    let mobile = normalize_mobile(&cfg.mobile);
    if !email.is_empty() {
        body.insert("email".into(), email.into());
    } else if !mobile.is_empty() {
        body.insert("mobile".into(), mobile.into());
        body.insert("area_code".into(), serde_json::Value::Null);
    } else {
        return Err(UpstreamError::Malformed(
            "account needs mobile or email".into(),
        ));
    }
    Ok(serde_json::Value::Object(body))
}

/// Strip `+86` prefixes from mainland numbers (Go `normalizeMobile`).
pub fn normalize_mobile(raw: &str) -> String {
    let s = raw.trim();
    let has_plus = s.starts_with('+');
    let digits: String = s.chars().filter(|c| c.is_ascii_digit()).collect();
    if (has_plus || digits.starts_with("86")) && digits.starts_with("86") && digits.len() == 13 {
        return digits[2..].to_string();
    }
    digits
}

/// The completion request (Go `CompletionRequest`). `thinking_disabled`
/// inverts the upstream flag so the default (zero) = reasoning ON.
#[derive(Debug, Clone, Default)]
pub struct CompletionRequest {
    pub session_id: String,
    pub prompt: String,
    pub ref_file_ids: Vec<String>,
    pub thinking_disabled: bool,
    pub search_enabled: bool,
    pub temperature: f64,
    pub top_p: f64,
    pub max_tokens: i64,
}

/// Completion body (Go `completionBody`) — kotlinx descriptor order
/// (qj1.java:16-26) with encodeDefaults: every field serializes every
/// time (`ref_file_ids: []` not null; `audio_id`/`action` literal nulls;
/// `preempt` unconditional). serde emits struct fields in declaration
/// order, matching the app's fingerprint; a sorted map would break it.
/// The optional sampling trio appends only when nonzero (Go omitempty).
#[derive(Debug, serde::Serialize)]
struct CompletionBody<'a> {
    chat_session_id: &'a str,
    parent_message_id: Option<i64>,
    prompt: &'a str,
    ref_file_ids: &'a [String],
    thinking_enabled: bool,
    search_enabled: bool,
    audio_id: Option<String>,
    preempt: bool,
    model_type: &'a str,
    action: Option<String>,
    #[serde(skip_serializing_if = "zero_f64")]
    temperature: f64,
    #[serde(skip_serializing_if = "zero_f64")]
    top_p: f64,
    #[serde(skip_serializing_if = "is_zero_i64")]
    max_tokens: i64,
}

fn zero_f64(v: &f64) -> bool {
    *v == 0.0
}
fn is_zero_i64(v: &i64) -> bool {
    *v == 0
}

pub fn completion_body(req: &CompletionRequest) -> String {
    // First-send flow (parent_message_id null): the app feeds model_type
    // from the session's model-type state flow — "default" with
    // expert/vision disabled; audio_id is the literal null of the
    // text-input path.
    let body = CompletionBody {
        chat_session_id: &req.session_id,
        parent_message_id: None,
        prompt: &req.prompt,
        ref_file_ids: &req.ref_file_ids,
        thinking_enabled: !req.thinking_disabled,
        search_enabled: req.search_enabled,
        audio_id: None,
        preempt: false,
        model_type: "default",
        action: None,
        temperature: req.temperature,
        top_p: req.top_p,
        max_tokens: req.max_tokens,
    };
    serde_json::to_string(&body).expect("completion body serializes")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::device::resolve_device_profile;

    #[test]
    fn login_body_shapes() {
        let profile = resolve_device_profile("", "138", "");
        // Mobile: password, device_id, os, mobile, area_code=null —
        // serialized alphabetically like Go's map marshal.
        let cfg = Config {
            mobile: "+8613800001111".into(),
            password: "pw".into(),
            ..Default::default()
        };
        let body = login_body(&cfg, &profile).unwrap();
        let obj = body.as_object().unwrap();
        assert_eq!(obj.len(), 5);
        assert_eq!(obj["os"], "android");
        assert_eq!(obj["mobile"], "13800001111"); // +86 stripped
        assert!(obj["area_code"].is_null());
        // Serialization is alphabetical (Go json.Marshal map parity).
        let s = serde_json::to_string(&body).unwrap();
        let keys: Vec<&str> = obj.keys().map(|k| k.as_str()).collect();
        let mut sorted = keys.clone();
        sorted.sort();
        assert_eq!(keys, sorted);
        assert!(s.contains("\"area_code\":null"));
        // Email login: no mobile, no area_code.
        let cfg = Config {
            email: "a@b.io".into(),
            password: "pw".into(),
            ..Default::default()
        };
        let body = login_body(&cfg, &profile).unwrap();
        assert!(body.get("mobile").is_none());
        assert!(body.get("area_code").is_none());
        assert_eq!(body["email"], "a@b.io");
        assert_eq!(body["os"], "android");
        // Web channel: os = "web".
        let profile_web = resolve_device_profile("B-shumei", "a@b.io", "web");
        let body = login_body(&cfg, &profile_web).unwrap();
        assert_eq!(body["os"], "web");
        assert_eq!(body["device_id"], "B-shumei");
        // Neither identity: error.
        let bad = Config {
            password: "pw".into(),
            ..Default::default()
        };
        assert!(login_body(&bad, &profile).is_err());
    }
    #[test]
    fn normalize_mobile_rules() {
        assert_eq!(normalize_mobile("+8613800001111"), "13800001111");
        assert_eq!(normalize_mobile("8613800001111"), "13800001111");
        assert_eq!(normalize_mobile("13800001111"), "13800001111");
        // Non-mainland shapes pass through as digits.
        assert_eq!(normalize_mobile("+85212345678"), "85212345678");
        assert_eq!(normalize_mobile("138-0000 1111"), "13800001111");
    }
    #[test]
    fn completion_body_field_order_and_nulls() {
        // The body serializes as a raw JSON string so the kotlinx
        // declaration order survives (a Value round-trip would re-sort).
        let req = CompletionRequest {
            session_id: "s1".into(),
            prompt: "hi".into(),
            ..Default::default()
        };
        let raw = completion_body(&req);
        assert_eq!(
            raw,
            r#"{"chat_session_id":"s1","parent_message_id":null,"prompt":"hi","ref_file_ids":[],"thinking_enabled":true,"search_enabled":false,"audio_id":null,"preempt":false,"model_type":"default","action":null}"#
        );
        let body: serde_json::Value = serde_json::from_str(&raw).unwrap();
        assert_eq!(body["thinking_enabled"], true); // default-on reasoning
        assert_eq!(body["search_enabled"], false); // opt-in search
                                                   // Sampling trio appended in order when nonzero (omitempty parity).
        let req = CompletionRequest {
            session_id: "s".into(),
            prompt: "p".into(),
            thinking_disabled: true,
            search_enabled: true,
            temperature: 0.7,
            top_p: 0.9,
            max_tokens: 1024,
            ..Default::default()
        };
        let raw = completion_body(&req);
        assert!(raw.contains("\"thinking_enabled\":false"));
        assert!(raw.contains("\"search_enabled\":true"));
        assert!(raw.ends_with("\"temperature\":0.7,\"top_p\":0.9,\"max_tokens\":1024}"));
        // ref_file_ids pass-through.
        let req = CompletionRequest {
            session_id: "s".into(),
            prompt: "p".into(),
            ref_file_ids: vec!["f1".to_string(), "f2".to_string()],
            ..Default::default()
        };
        let raw = completion_body(&req);
        assert!(raw.contains("\"ref_file_ids\":[\"f1\",\"f2\"]"));
    }
}
