//! Password login, STS exchange, renewal, validation (Go login.go +
//! passport.go primeLoginPage port, SDD §2.2/§2.3).

use md5::{Digest, Md5};
use serde_json::Value;
use url::Url;

use crate::login::jar::{
    collect_cookies, cookie_header, harvest_device, merge_jars, resolve_passport_location,
    seed_device_jar, Jar,
};
use crate::login::xssi::{field_code, field_string, parse_xssi};
use crate::login::{Client, Device, LoginError};

/// Cap on manual redirect hops of a landing chain (Go maxChainHops,
/// consent.go:29).
pub(crate) const MAX_CHAIN_HOPS: usize = 8;

/// The parsed loginUrl parameter set required by serviceLoginAuth2 (Go
/// LoginContext, SDD §2.2 step 1, TDD LOGIN-001).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct LoginContext {
    pub login_url: String,
    pub callback: String,
    pub sid: String,
    pub qs: String,
    pub sign: String,
    pub service_param: String,
    pub group: String,
}

/// A derived upstream session (Go Session, SDD §2.1/§2.3) — maps onto the
/// account row's `cookie`/`ph`/`user_id`/`pass_token`/`device_json` columns.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Session {
    /// Full Cookie header value for upstream calls.
    pub cookie: String,
    /// xiaomichatbot_ph value (unquoted).
    pub ph: String,
    pub user_id: String,
    pub pass_token: String,
    /// Passport device identity the chain presented/harvested; the caller
    /// persists it per account (device.go).
    pub device: Device,
}

impl Client {
    /// Validate the loginUrl host against the passport allowlist and
    /// extract callback/sid/qs/_sign/serviceParam/_group (Go parseLoginURL,
    /// login.go:96-114, TDD LOGIN-001/008).
    pub(crate) fn parse_login_url(&self, raw: &str) -> Result<LoginContext, LoginError> {
        let u =
            Url::parse(raw).map_err(|e| LoginError::Protocol(format!("loginUrl parse: {e}")))?;
        let host = u.host_str().unwrap_or_default();
        if !host.is_empty() && !self.host_allowed(host) {
            return Err(LoginError::Protocol(format!(
                "loginUrl host {host:?} not in allowlist"
            )));
        }
        let get = |k: &str| {
            u.query_pairs()
                .find(|(key, _)| key == k)
                .map(|(_, v)| v.into_owned())
                .unwrap_or_default()
        };
        Ok(LoginContext {
            login_url: raw.to_string(),
            callback: get("callback"),
            sid: get("sid"),
            qs: get("qs"),
            sign: get("_sign"),
            service_param: get("serviceParam"),
            group: get("_group"),
        })
    }

    /// Probe GET /open-apis/user/mi/get and extract the loginUrl parameters
    /// (Go fetchLoginContext, login.go:118-144, TDD LOGIN-001). The 401 body
    /// carries loginUrl as plain JSON (aistudio dialect) or XSSI-prefixed
    /// (passport dialect).
    pub async fn fetch_login_context(&self) -> Result<LoginContext, LoginError> {
        let url = format!("{}/open-apis/user/mi/get", self.base());
        let resp = self
            .http
            .get(&url)
            .headers(self.aistudio_headers())
            .send()
            .await
            .map_err(|e| LoginError::Transport(format!("login context probe: {e}")))?;
        let body = read_limited(resp).await?;
        // Plain JSON dialect first.
        if let Ok(probe) = serde_json::from_slice::<Value>(&body) {
            if let Some(u) = probe.get("loginUrl").and_then(Value::as_str) {
                if !u.is_empty() {
                    return self.parse_login_url(u);
                }
            }
        }
        // XSSI-prefixed passport dialect.
        if let Ok(m) = parse_xssi(&body) {
            let u = field_string(&m, "loginUrl");
            if !u.is_empty() {
                return self.parse_login_url(&u);
            }
        }
        Err(LoginError::Protocol(
            "login context: no loginUrl in probe response".into(),
        ))
    }

    /// The navigation a browser does before posting the login form (Go
    /// primeLoginPage, passport.go:101-145): ≤2 GET hops with navigation
    /// headers, seed jar presented as Cookie, collectCookies each hop, only
    /// 3xx-with-Location followed, host allowlist enforced. Best-effort: a
    /// failure returns what was collected — a login presenting no device
    /// cookies is still better than none (the pre-fix path).
    pub(crate) async fn prime_login_page(&self, login_url: &str, seed: &Jar) -> Jar {
        let mut jar = seed.clone();
        if login_url.is_empty() {
            return jar;
        }
        let mut target = login_url.to_string();
        for hop in 0..2 {
            let mut req = self
                .http
                .get(&target)
                .headers(self.navigation_headers(&format!("{}/", self.base())));
            if hop > 0 {
                req = req.header(reqwest::header::REFERER, login_url);
            }
            // A cookieless navigation is exactly what makes the passport
            // mint a brand-new deviceId (live capture).
            let ck = cookie_header(&jar);
            if !ck.is_empty() {
                req = req.header(reqwest::header::COOKIE, ck);
            }
            let resp = match req.send().await {
                Ok(r) => r,
                Err(_) => return jar,
            };
            let status = resp.status().as_u16();
            let headers = resp.headers().clone();
            // Cookies only — the login page body carries no data we need.
            let _ = resp.bytes().await;
            jar = merge_jars(&jar, &collect_cookies(&headers));
            let loc = headers
                .get(reqwest::header::LOCATION)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_string();
            if loc.is_empty() || !(300..=399).contains(&status) {
                return jar;
            }
            match resolve_passport_location(self, &target, &loc) {
                Ok(next) => target = next,
                Err(_) => return jar,
            }
        }
        jar
    }

    /// Full password login (Go Login/login, SDD §2.2 steps 1-3, TDD
    /// LOGIN-002..009): context probe → prime login page → POST
    /// /pass/serviceLoginAuth2 (form-encoded, hash = MD5(password) hex
    /// upper, no salt) → follow location → collect Set-Cookie into a
    /// session. Cross-region consent is auto-agreed unless the client was
    /// built with it disabled.
    pub async fn login(&self, phone: &str, password: &str) -> Result<Session, LoginError> {
        self.login_with_consent(phone, password, !self.disable_cross_region_agree)
            .await
    }

    /// Login but agree the cross-region demand regardless of the client's
    /// setting (Go LoginWithConsent, login.go:166-168).
    pub async fn login_with_consent(
        &self,
        phone: &str,
        password: &str,
        agree_consent: bool,
    ) -> Result<Session, LoginError> {
        let lctx = self.fetch_login_context().await?;

        let body = form_urlencoded(&[
            ("_json", "true"),
            ("callback", lctx.callback.as_str()),
            ("sid", lctx.sid.as_str()),
            ("qs", lctx.qs.as_str()),
            ("_sign", lctx.sign.as_str()),
            ("serviceParam", lctx.service_param.as_str()),
            ("user", phone),
            ("hash", &md5_upper_hex(password)),
            ("policyName", "miaccount"),
            ("captCode", ""),
            ("_group", lctx.group.as_str()),
        ]);

        let pu = Url::parse(&lctx.login_url)
            .map_err(|e| LoginError::Protocol(format!("loginUrl parse: {e}")))?;
        // Authority (host[:port]) — Go u.Host keeps the port; host_str() drops it.
        let host = match pu.port() {
            Some(p) => format!("{}:{}", pu.host_str().unwrap_or_default(), p),
            None => pu.host_str().unwrap_or_default().to_string(),
        };
        let origin = format!("{}://{host}", pu.scheme());
        let auth_url = format!("{origin}/pass/serviceLoginAuth2");

        // Prime the login page first: that navigation is where the passport
        // hands out the device anchor (deviceId=wb_<uuid>, pass_ua, uLocale,
        // JSESSIONID); the stored device identity rides on top of whatever
        // the page issues (Set-Cookie overwrites the value we sent — DEV-004).
        let jar = self
            .prime_login_page(&lctx.login_url, &seed_device_jar(&self.device))
            .await;

        let mut req = self
            .http
            .post(&auth_url)
            .headers(self.form_post_headers(&origin, &lctx.login_url))
            .body(body);
        let ck = cookie_header(&jar);
        if !ck.is_empty() {
            req = req.header(reqwest::header::COOKIE, ck);
        }
        let resp = req
            .send()
            .await
            .map_err(|e| LoginError::Transport(format!("serviceLoginAuth2: {e}")))?;
        let status = resp.status().as_u16();
        let headers = resp.headers().clone();
        let resp_body = read_limited(resp).await?;
        // The passport answers 200 with a JSON/XSSI body, including for its
        // own error codes. A 5xx or 429 therefore means an intermediary:
        // parsing that body as a login result would accept a proxy's error
        // page (REC-003).
        if status >= 500 || status == 429 {
            return Err(LoginError::Transport(format!(
                "serviceLoginAuth2: upstream status {status}"
            )));
        }

        let jar = merge_jars(&jar, &collect_cookies(&headers));
        let m = parse_xssi(&resp_body)?;
        let code = field_code(&m).unwrap_or(0);
        if code != 0 {
            let desc = field_string(&m, "description");
            let cu = field_string(&m, "captchaUrl");
            if !cu.is_empty() {
                return Err(LoginError::AccountAction {
                    reason: "login requires slider captcha; manual re-login needed".into(),
                    url: cu,
                    cause: None,
                    device: harvest_device(&jar),
                });
            }
            let nu = field_string(&m, "notificationUrl");
            if !nu.is_empty() {
                return Err(LoginError::AccountAction {
                    reason: "login requires new-device verification (OTP); manual re-login needed"
                        .into(),
                    url: nu,
                    cause: None,
                    device: harvest_device(&jar),
                });
            }
            return Err(LoginError::Protocol(format!(
                "login failed: code={code} {desc}"
            )));
        }
        let loc = field_string(&m, "location");
        if loc.is_empty() {
            // Live-captured protocol gap (2026-10-08): the new-device OTP
            // demand also arrives with code=0 — securityStatus=128, empty
            // location, notificationUrl set. Auto-start the challenge
            // (SDD §2.2 amendment); completion is out-of-band via
            // submit_otp.
            let nu = field_string(&m, "notificationUrl");
            if !nu.is_empty() {
                if !self.disable_otp_auto {
                    match self.start_otp(&nu, &lctx.sid, &jar, agree_consent).await {
                        Ok(challenge) => {
                            let notify = challenge.notify.clone();
                            return Err(LoginError::OtpPending {
                                challenge: Box::new(challenge),
                                device: harvest_device(&jar),
                                notify,
                            });
                        }
                        Err(cerr) => {
                            if matches!(cerr, LoginError::CrossRegionConsent) {
                                return Err(LoginError::AccountAction {
                                    reason: "login demands the cross-region data-transfer agreement: agree once in a browser from this egress IP, or set MIMO_RUST_CROSS_REGION_AGREE=true".into(),
                                    url: nu,
                                    cause: Some(Box::new(cerr)),
                                    device: harvest_device(&jar),
                                });
                            }
                            return Err(LoginError::AccountAction {
                                reason: format!(
                                    "new-device OTP verification required (auto-start failed: {cerr})"
                                ),
                                url: nu,
                                cause: Some(Box::new(cerr)),
                                device: harvest_device(&jar),
                            });
                        }
                    }
                }
                return Err(LoginError::AccountAction {
                    reason: "new-device OTP verification required".into(),
                    url: nu,
                    cause: None,
                    device: harvest_device(&jar),
                });
            }
            return Err(LoginError::Protocol(
                "login succeeded but no location for STS exchange".into(),
            ));
        }

        let mut user_id = field_string(&m, "userId");
        if user_id.is_empty() {
            user_id = jar.get("userId").cloned().unwrap_or_default();
        }
        let pass_token = jar.get("passToken").cloned().unwrap_or_default();
        if user_id.is_empty() || pass_token.is_empty() {
            return Err(LoginError::Protocol(
                "login response missing userId/passToken — protocol drift".into(),
            ));
        }

        let mut sess = self.sts_exchange_with_jar(&loc, jar).await?;
        sess.user_id = user_id;
        sess.pass_token = pass_token;
        Ok(sess)
    }

    /// Renew the serviceToken from the passToken via STS (Go Renew, SDD
    /// §2.3, TDD RENEW-001..003/009): probe with userId only → loginUrl →
    /// GET loginUrl&_json=true with userId+passToken cookies → follow
    /// location → fresh cookies. code 70016 → PassTokenDead.
    pub async fn renew(&self, user_id: &str, pass_token: &str) -> Result<Session, LoginError> {
        // Step 1: probe for the official loginUrl.
        let url = format!("{}/open-apis/user/mi/get", self.base());
        let resp = self
            .http
            .get(&url)
            .headers(self.aistudio_headers())
            .header(reqwest::header::COOKIE, format!("userId={user_id};"))
            .send()
            .await
            .map_err(|e| LoginError::Transport(format!("renewal probe: {e}")))?;
        let body = read_limited(resp).await?;
        let mut login_url = serde_json::from_slice::<Value>(&body)
            .ok()
            .and_then(|v| {
                v.get("loginUrl")
                    .and_then(Value::as_str)
                    .map(str::to_string)
            })
            .unwrap_or_default();
        if login_url.is_empty() {
            // Fallback: construct the official renewal URL (session.js:149-153).
            let cb = format!(
                "{}/sts?sign=%2FKuvzNMEPYoLWPMSl3fRtWQeN%2Bo%3D&followup={}",
                self.base(),
                urlencode(&format!("{}/", self.base()))
            );
            login_url = format!(
                "https://account.xiaomi.com/pass/serviceLogin?callback={}&sid=xiaomichatbot&_group=DEFAULT",
                urlencode(&cb)
            );
        }

        // Step 2: serviceLogin with _json=true and the passToken cookie.
        let pu = rebuild_with_json_param(&login_url)?;
        let host = pu.host_str().unwrap_or_default();
        if !self.host_allowed(host) {
            return Err(LoginError::Protocol(format!(
                "renewal loginUrl host {host:?} not in allowlist"
            )));
        }

        let mut ck = format!("userId={user_id}; passToken={pass_token}");
        let dev = crate::login::jar::device_cookie_header(&self.device);
        if !dev.is_empty() {
            ck = format!("{ck}; {dev}");
        }
        let resp = self
            .http
            .get(pu.as_str())
            .headers(self.navigation_headers(&format!("{}/", self.base())))
            .header(reqwest::header::COOKIE, ck)
            .send()
            .await
            .map_err(|e| LoginError::Transport(format!("renewal serviceLogin: {e}")))?;
        let body = read_limited(resp).await?;
        let m = parse_xssi(&body)?;
        let code = field_code(&m).unwrap_or(0);
        if code != 0 {
            let desc = field_string(&m, "description");
            if code == 70016 {
                return Err(LoginError::PassTokenDead(desc));
            }
            return Err(LoginError::Protocol(format!(
                "renewal failed: code={code} {desc}"
            )));
        }
        let loc = field_string(&m, "location");
        if loc.is_empty() {
            return Err(LoginError::Protocol(
                "renewal response missing location".into(),
            ));
        }

        // Step 3: follow the STS location, seeded with the device identity.
        // The jar is threaded through the exchange; the Session it returns
        // exposes what the chain planted (passToken best-effort from the
        // walked jar — DEV-006).
        let seed = seed_device_jar(&self.device);
        let mut sess = self.sts_exchange_with_jar(&loc, seed).await?;
        if sess.user_id.is_empty() {
            sess.user_id = user_id.to_string();
        }
        // Prefer a rotated passToken when the chain planted one; writing the
        // stored value over a fresh one would silently drop the refresh
        // (zero-cost insurance).
        let rotated = sess.pass_token.clone();
        if rotated.is_empty() || rotated == pass_token {
            sess.pass_token = pass_token.to_string();
        }
        Ok(sess)
    }

    /// Check a session cookie against the upstream (Go Validate, SDD
    /// §2.3, TDD RENEW-009): 200 + business code 0 means healthy; anything
    /// else is invalid, auth-shaped outcomes wrapped as Auth.
    pub async fn validate(&self, cookie: &str) -> Result<(), LoginError> {
        let url = format!("{}/open-apis/user/mi/get", self.base());
        let resp = self
            .http
            .get(&url)
            .headers(self.aistudio_headers())
            .header(reqwest::header::COOKIE, cookie)
            .send()
            .await
            .map_err(|e| LoginError::Transport(format!("validate: {e}")))?;
        let status = resp.status().as_u16();
        if status == 401 {
            return Err(LoginError::Auth("session expired (401)".into()));
        }
        if status != 200 {
            return Err(LoginError::Transport(format!("validate: status {status}")));
        }
        let body = read_limited(resp).await?;
        if let Ok(result) = serde_json::from_slice::<Value>(&body) {
            if let Some(code) = result.get("code").and_then(Value::as_i64) {
                if code != 0 {
                    return Err(LoginError::Auth(format!("validate: business code {code}")));
                }
            }
        }
        Ok(())
    }
}

/// Rebuild a URL with `_json=true` set in its query (Go q.Set("_json",
/// "true") + pu.RawQuery = q.Encode(): the existing `_json` value, if any,
/// is replaced).
fn rebuild_with_json_param(raw: &str) -> Result<Url, LoginError> {
    let mut u = Url::parse(raw)
        .map_err(|e| LoginError::Protocol(format!("renewal loginUrl parse: {e}")))?;
    let mut pairs: Vec<(String, String)> = u
        .query()
        .map(|q| {
            url::form_urlencoded::parse(q.as_bytes())
                .map(|(k, v)| (k.into_owned(), v.into_owned()))
                .filter(|(k, _)| k != "_json")
                .collect()
        })
        .unwrap_or_default();
    pairs.push(("_json".into(), "true".into()));
    u.set_query(Some(
        &url::form_urlencoded::Serializer::new(String::new())
            .extend_pairs(pairs.iter().map(|(k, v)| (k.as_str(), v.as_str())))
            .finish(),
    ));
    Ok(u)
}

/// MD5(password).hex().toUpperCase(), no salt (login.go:186-187) — the only
/// cryptographic operation in the login chain.
fn md5_upper_hex(password: &str) -> String {
    let digest = Md5::digest(password.as_bytes());
    let mut out = String::with_capacity(32);
    for b in digest {
        out.push_str(&format!("{b:02X}"));
    }
    out
}

/// Serialize (key, value) pairs in order (Go url.Values.Encode: sorted keys;
/// the fixed login form fields are all distinct, so insertion order equals
/// the sorted order — values here are the Go form fields verbatim).
fn form_urlencoded(form: &[(&str, &str)]) -> String {
    url::form_urlencoded::Serializer::new(String::new())
        .extend_pairs(form.iter().map(|(k, v)| (*k, *v)))
        .finish()
}

fn urlencode(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

/// Read a response body capped at 1 MiB (Go io.LimitReader(resp.Body, 1<<20)).
pub(crate) async fn read_limited(resp: reqwest::Response) -> Result<Vec<u8>, LoginError> {
    let bytes = resp
        .bytes()
        .await
        .map_err(|e| LoginError::Transport(format!("read body: {e}")))?;
    Ok(bytes.slice(..(1 << 20).min(bytes.len())).to_vec())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn login_002_hash_is_md5_upper_hex_no_salt() {
        // md5("password") = 5f4dcc3b5aa765d61d8327deb882cf99, uppercased.
        assert_eq!(
            md5_upper_hex("password"),
            "5F4DCC3B5AA765D61D8327DEB882CF99"
        );
        // md5("") = d41d8cd98f00b204e9800998ecf8427e — proves no salt.
        assert_eq!(md5_upper_hex(""), "D41D8CD98F00B204E9800998ECF8427E");
    }

    #[test]
    fn renew_json_param_replaces_existing() {
        let u =
            rebuild_with_json_param("https://a/pass/serviceLogin?callback=cb&sid=x&_json=false")
                .unwrap();
        let q: Vec<(String, String)> = u
            .query_pairs()
            .map(|(k, v)| (k.into_owned(), v.into_owned()))
            .collect();
        assert!(q.contains(&("_json".to_string(), "true".to_string())));
        assert!(q.contains(&("callback".to_string(), "cb".to_string())));
        assert!(!q.contains(&("_json".to_string(), "false".to_string())));
    }
}
