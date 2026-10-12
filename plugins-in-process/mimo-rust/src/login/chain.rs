//! Manual redirect-chain machinery shared by the consent/STS/OTP flows
//! (Go consent.go ssoDo/followChain/absURL/queryOf + hopRecord port).
//! Redirects are followed by hand, never by the transport, so every
//! intermediate Set-Cookie and Location is observable and captured.

use url::Url;

use crate::login::auth::read_limited;
use crate::login::jar::{collect_cookies, cookie_header, Jar};
use crate::login::Client;
use crate::login::LoginError;

/// One request/redirect observed while following a chain by hand (Go
/// hopRecord, consent.go:31-36). Status/URL are part of the hop record the
/// consent chain is inspected through; keep the shape even where a given
/// call site reads only `location`.
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub(crate) struct HopRecord {
    pub status: u16,
    pub url: String,
    pub location: String,
}

/// One SSO request with jar cookies in and Set-Cookie out (Go ssoDo,
/// otp.go:184-220). Redirects are followed by hand, never by the transport,
/// so cookies land in our jar. `extra` headers override the XHR defaults.
pub(crate) async fn sso_do(
    client: &Client,
    jar: &mut Jar,
    method: reqwest::Method,
    target: &str,
    form: Option<&str>,
    extra: &[(&str, &str)],
) -> Result<(Vec<u8>, u16, reqwest::header::HeaderMap), LoginError> {
    let mut req = client.http.request(method, target);
    // identity/* are same-origin XHRs from the account SPA: send what its
    // axios instance sends (passport.go), then let `extra` override.
    if let Ok(tu) = Url::parse(target) {
        // Authority (host[:port]) — Go u.Host keeps the port.
        let authority = match tu.port() {
            Some(p) => format!("{}:{}", tu.host_str().unwrap_or_default(), p),
            None => tu.host_str().unwrap_or_default().to_string(),
        };
        let origin = format!("{}://{}", tu.scheme(), authority);
        req = req.headers(client.xhr_headers(&origin, &format!("{origin}/")));
    }
    if let Some(form) = form {
        req = req
            .header(
                reqwest::header::CONTENT_TYPE,
                "application/x-www-form-urlencoded",
            )
            .body(form.to_string());
    }
    for (k, v) in extra {
        req = req.header(*k, *v);
    }
    let ck = cookie_header(jar);
    if !ck.is_empty() {
        req = req.header(reqwest::header::COOKIE, ck);
    }
    let resp = req
        .send()
        .await
        .map_err(|e| LoginError::Transport(format!("sso {target}: {e}")))?;
    let status = resp.status().as_u16();
    let headers = resp.headers().clone();
    let body = read_limited(resp).await?;
    // Set-Cookie out (last wins, browser store semantics).
    for (k, v) in collect_cookies(&headers) {
        jar.insert(k, v);
    }
    Ok((body, status, headers))
}

/// GET a chain with redirects followed by hand (Go followChain,
/// consent.go:67-90): every intermediate Location is observable — the
/// consent handoff and authStart context ride on intermediate Locations.
/// Cookies accumulate into the jar. Returns the hops and the final URL.
pub(crate) async fn follow_chain(
    client: &Client,
    jar: &mut Jar,
    start: &str,
    max_hops: usize,
) -> Result<(Vec<HopRecord>, String), LoginError> {
    let mut hops = Vec::new();
    let mut target = client.abs_passport_url(start);
    for i in 0..max_hops {
        let u = Url::parse(&target)
            .map_err(|e| LoginError::Protocol(format!("chain hop {i}: parse {target:?}: {e}")))?;
        let host = u.host_str().unwrap_or_default();
        if host.is_empty() || !client.otp_redirect_ok(host) {
            return Err(LoginError::Protocol(format!(
                "chain hop {i}: host {host:?} not allowlisted"
            )));
        }
        let (_body, status, headers) =
            sso_do(client, jar, reqwest::Method::GET, &target, None, &[])
                .await
                .map_err(|e| LoginError::Protocol(format!("chain hop {i}: {e}")))?;
        let loc = headers
            .get(reqwest::header::LOCATION)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        hops.push(HopRecord {
            status,
            url: target.clone(),
            location: loc.clone(),
        });
        if !(300..=399).contains(&status) || loc.is_empty() {
            return Ok((hops, target));
        }
        target = abs_url(&target, &loc);
    }
    Err(LoginError::Protocol(format!(
        "notification chain exceeded {max_hops} hops"
    )))
}

/// Resolve a possibly-relative Location against its source URL (Go absURL,
/// consent.go:45-61).
pub(crate) fn abs_url(base: &str, reference: &str) -> String {
    if reference.is_empty() {
        return String::new();
    }
    if reference.starts_with("http://") || reference.starts_with("https://") {
        return reference.to_string();
    }
    match Url::parse(base) {
        Ok(b) => b
            .join(reference)
            .map(|u| u.to_string())
            .unwrap_or_else(|_| reference.to_string()),
        Err(_) => reference.to_string(),
    }
}

/// Query of a raw URL, tolerating junk (Go queryOf, consent.go:93-99).
pub(crate) fn query_of(raw: &str) -> Vec<(String, String)> {
    Url::parse(raw)
        .map(|u| {
            u.query_pairs()
                .map(|(k, v)| (k.into_owned(), v.into_owned()))
                .collect()
        })
        .unwrap_or_default()
}

pub(crate) fn query_get(raw: &str, key: &str) -> String {
    query_of(raw)
        .into_iter()
        .find(|(k, _)| k == key)
        .map(|(_, v)| v)
        .unwrap_or_default()
}

/// Whether the walked chain went through the cross-region consent page, and
/// its userId when present (Go isConsentChain, consent.go:103-114).
pub(crate) fn is_consent_chain(hops: &[HopRecord], page: &str) -> (String, bool) {
    for h in hops {
        if query_get(&h.location, "bizType") == "UserCross" {
            return (query_get(&h.location, "userId"), true);
        }
    }
    if page.contains("/fe/service/userCross") {
        return (query_get(page, "userId"), true);
    }
    (String::new(), false)
}

/// Extract sid/context from an authStart URL, following ≤3 levels of
/// `?callback=` indirection — the consent flow answers with
/// /pass2/userinfo/check?callback=<percent-encoded authStart URL> (Go
/// authStartParams, consent.go:119-133).
pub(crate) fn auth_start_params(raw: &str) -> (String, String) {
    let mut cur = raw.to_string();
    for _ in 0..3 {
        if cur.is_empty() {
            break;
        }
        let context = query_get(&cur, "context");
        if !context.is_empty() {
            let mut sid = query_get(&cur, "sid");
            if sid.is_empty() {
                sid = "xiaomichatbot".into();
            }
            return (sid, context);
        }
        cur = query_get(&cur, "callback");
    }
    (String::new(), String::new())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn urlencode(s: &str) -> String {
        url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
    }

    #[test]
    fn abs_url_absolute_passthrough() {
        assert_eq!(abs_url("https://a/x", "https://b/y"), "https://b/y");
    }

    #[test]
    fn abs_url_relative_resolved() {
        assert_eq!(abs_url("https://a/x/y", "/z"), "https://a/z");
    }

    #[test]
    fn auth_start_params_direct_context() {
        let (sid, cx) =
            auth_start_params("https://a/fe/service/identity/authStart?sid=mysid&context=ctx1");
        assert_eq!(sid, "mysid");
        assert_eq!(cx, "ctx1");
    }

    #[test]
    fn auth_start_params_sid_defaults_to_xiaomichatbot() {
        let (sid, cx) = auth_start_params("https://a/authStart?context=ctx2");
        assert_eq!(sid, "xiaomichatbot");
        assert_eq!(cx, "ctx2");
    }

    #[test]
    fn auth_start_params_follows_callback_indirection() {
        // Percent-encoded authStart URL as a callback value.
        let inner = "https://a/authStart?sid=inner&context=deep";
        let raw = format!(
            "https://a/pass2/userinfo/check?callback={}",
            urlencode(inner)
        );
        let (sid, cx) = auth_start_params(&raw);
        assert_eq!(sid, "inner");
        assert_eq!(cx, "deep");
    }

    #[test]
    fn is_consent_chain_detects_biztype() {
        let hops = vec![HopRecord {
            status: 302,
            url: "https://a/x".into(),
            location: "https://a/pass2/userinfo/start?bizType=UserCross&userId=42".into(),
        }];
        assert_eq!(
            is_consent_chain(&hops, "https://a/end"),
            ("42".into(), true)
        );
    }

    #[test]
    fn is_consent_chain_detects_page_path() {
        let hops = vec![HopRecord {
            status: 200,
            url: "https://a/fe/service/userCross?needClose=false&userId=9".into(),
            location: String::new(),
        }];
        assert_eq!(
            is_consent_chain(
                &hops,
                "https://a/fe/service/userCross?needClose=false&userId=9"
            ),
            ("9".into(), true)
        );
    }

    #[test]
    fn is_consent_chain_plain_chain() {
        let hops = vec![HopRecord {
            status: 200,
            url: "https://a/other".into(),
            location: String::new(),
        }];
        assert_eq!(
            is_consent_chain(&hops, "https://a/other"),
            ("".into(), false)
        );
    }
}
