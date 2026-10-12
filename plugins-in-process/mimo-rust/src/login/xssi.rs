//! Passport response parsing (Go login.go parseXSSI + otp.go loose-JSON
//! helpers). The `&&&START&&&` XSSI prefix is stripped exactly; garbage
//! before it is rejected (TDD LOGIN-003).

use serde_json::Value;

use crate::login::LoginError;

/// XSSI prefix on all passport JSON responses (SDD §2.2).
pub const XSSI_PREFIX: &str = "&&&START&&&";

/// Strip the XSSI prefix and decode the JSON body (Go parseXSSI,
/// login.go:46-60). Bodies without the exact prefix — or with garbage
/// before it — are rejected.
pub fn parse_xssi(body: &[u8]) -> Result<serde_json::Map<String, Value>, LoginError> {
    let s = String::from_utf8_lossy(body);
    let s = s.as_ref();
    let i = s.find(XSSI_PREFIX).ok_or_else(|| {
        LoginError::Protocol(format!(
            "passport response missing {XSSI_PREFIX} prefix: {:.120}",
            s
        ))
    })?;
    if !s[..i].trim().is_empty() {
        return Err(LoginError::Protocol(format!(
            "garbage before {XSSI_PREFIX} prefix: {:.60}",
            &s[..i]
        )));
    }
    let rest = &s[i + XSSI_PREFIX.len()..];
    let v: Value = serde_json::from_str(rest).map_err(|e| {
        LoginError::Protocol(format!("passport response not JSON after prefix: {e}"))
    })?;
    match v {
        Value::Object(m) => Ok(m),
        _ => Err(LoginError::Protocol(
            "passport response not a JSON object after prefix".into(),
        )),
    }
}

/// Decode a passport/identity response: XSSI prefix stripped when present,
/// plain JSON otherwise — the identity endpoints answer both dialects (Go
/// parseLooseJSON, otp.go:61-69).
pub fn parse_loose_json(body: &[u8]) -> Result<serde_json::Map<String, Value>, LoginError> {
    let mut s = String::from_utf8_lossy(body).trim().to_string();
    if let Some(rest) = s.strip_prefix(XSSI_PREFIX) {
        s = rest.trim().to_string();
    }
    let v: Value = serde_json::from_str(&s)
        .map_err(|e| LoginError::Protocol(format!("passport response not JSON: {e}")))?;
    match v {
        Value::Object(m) => Ok(m),
        _ => Err(LoginError::Protocol(
            "passport response not a JSON object".into(),
        )),
    }
}

/// Render a JSON string/number as a token string — userId is a number in
/// real passport responses (Go scalarString, otp.go:73-85).
pub fn scalar_string(v: &Value) -> Option<String> {
    match v {
        Value::String(s) => Some(s.clone()),
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                Some(i.to_string())
            } else {
                n.as_f64().map(|f| f.to_string())
            }
        }
        _ => None,
    }
}

/// Field as a string, string or number (userId shape).
pub fn field_string(m: &serde_json::Map<String, Value>, key: &str) -> String {
    m.get(key).and_then(scalar_string).unwrap_or_default()
}

/// Field as an integer code, when present (passport `code`).
pub fn field_code(m: &serde_json::Map<String, Value>) -> Option<i64> {
    m.get("code").and_then(Value::as_i64)
}

/// Field as float, when present (`flag` arrives as a JSON number).
pub fn field_f64(m: &serde_json::Map<String, Value>, key: &str) -> Option<f64> {
    m.get(key).and_then(Value::as_f64)
}

/// Scan a whole JSON tree for passToken/userId (case-insensitive keys,
/// string or number values, first wins) — the reference findTokens
/// (otp.go:88-120).
pub fn find_tokens(body: &[u8]) -> (String, String) {
    let mut pass_token = String::new();
    let mut user_id = String::new();
    let root = match parse_loose_json(body) {
        Ok(m) => Value::Object(m),
        Err(_) => return (pass_token, user_id),
    };
    walk_tokens(&root, &mut pass_token, &mut user_id);
    (pass_token, user_id)
}

fn walk_tokens(v: &Value, pass_token: &mut String, user_id: &mut String) {
    match v {
        Value::Object(m) => {
            for (k, x) in m {
                if let Some(s) = scalar_string(x) {
                    if !s.is_empty() {
                        match k.to_lowercase().as_str() {
                            "passtoken" if pass_token.is_empty() => {
                                *pass_token = s;
                            }
                            "userid" if user_id.is_empty() => {
                                *user_id = s;
                            }
                            _ => {}
                        }
                    }
                }
                walk_tokens(x, pass_token, user_id);
            }
        }
        Value::Array(items) => {
            for x in items {
                walk_tokens(x, pass_token, user_id);
            }
        }
        _ => {}
    }
}

/// Harvest passToken/userId from the cookie jar, case-insensitive, first
/// wins (Go jarTokens, otp.go:123-136).
pub fn jar_tokens(jar: &crate::login::Jar) -> (String, String) {
    let mut pass_token = String::new();
    let mut user_id = String::new();
    for (k, v) in jar {
        if v.is_empty() {
            continue;
        }
        match k.to_lowercase().as_str() {
            "passtoken" if pass_token.is_empty() => pass_token = v.clone(),
            "userid" if user_id.is_empty() => user_id = v.clone(),
            _ => {}
        }
    }
    (pass_token, user_id)
}

/// The reference rule: fail when `code` is present and is neither -1 nor 0
/// (Go codeCheck, otp.go:258-270).
pub fn code_check(m: &serde_json::Map<String, Value>, what: &str) -> Result<(), LoginError> {
    if let Some(code) = field_code(m) {
        if code != -1 && code != 0 {
            let mut desc = field_string(m, "desc");
            if desc.is_empty() {
                desc = field_string(m, "description");
            }
            return Err(LoginError::Protocol(format!(
                "{what} failed: code={code} {desc}"
            )));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn login_003_xssi_prefix_stripped_and_json_parsed() {
        let body = b"&&&START&&&{\"code\":0,\"location\":\"https://x/sts\"}";
        let m = parse_xssi(body).unwrap();
        assert_eq!(field_string(&m, "location"), "https://x/sts");
    }

    #[test]
    fn login_003_garbage_before_prefix_rejected() {
        let body = b"<html>hi</html>&&&START&&&{\"code\":0}";
        assert!(parse_xssi(body).is_err());
    }

    #[test]
    fn login_003_missing_prefix_rejected() {
        assert!(parse_xssi(b"{\"code\":0}").is_err());
    }

    #[test]
    fn login_003_garbage_after_prefix_rejected() {
        assert!(parse_xssi(b"&&&START&&&not json").is_err());
    }

    #[test]
    fn loose_json_accepts_both_dialects() {
        assert!(parse_loose_json(b"{\"code\":0}").is_ok());
        assert!(parse_loose_json(b"&&&START&&& {\"code\":0}").is_ok());
    }

    #[test]
    fn code_check_passes_minus_one_zero_absent() {
        code_check(&serde_json::from_str("{\"code\":0}").unwrap(), "x").unwrap();
        code_check(&serde_json::from_str("{\"code\":-1}").unwrap(), "x").unwrap();
        code_check(&serde_json::from_str("{}").unwrap(), "x").unwrap();
    }

    #[test]
    fn code_check_fails_on_nonzero() {
        let m: serde_json::Map<String, Value> =
            serde_json::from_str("{\"code\":70002,\"description\":\"bad\"}").unwrap();
        let err = code_check(&m, "otp verify").unwrap_err();
        assert!(err.to_string().contains("code=70002"));
        assert!(err.to_string().contains("bad"));
    }

    #[test]
    fn find_tokens_walks_nested_tree_case_insensitive() {
        let body = br#"{"data":{"UserId":123456,"nested":[{"PassToken":"tok1"}]}}"#;
        let (pt, uid) = find_tokens(body);
        assert_eq!(pt, "tok1");
        assert_eq!(uid, "123456");
    }

    #[test]
    fn find_tokens_duplicate_keys_depth_order_unspecified() {
        // Go walks map[string]any with randomized iteration order, serde_json
        // (BTreeMap) alphabetical — neither promises document order. With the
        // same key at two depths the winner is implementation-defined in
        // BOTH; the real contract is the token is always found.
        let body = br#"{"deep":{"userId":"b"},"userId":"a","passToken":"t"}"#;
        let (pt, uid) = find_tokens(body);
        assert_eq!(pt, "t");
        assert!(uid == "a" || uid == "b");
    }

    #[test]
    fn jar_tokens_case_insensitive() {
        let mut jar = crate::login::Jar::new();
        jar.insert("userId".into(), "u1".into());
        jar.insert("passToken".into(), "p1".into());
        assert_eq!(jar_tokens(&jar), ("p1".into(), "u1".into()));
    }
}
