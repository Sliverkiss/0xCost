//! OTP challenge, consent chain, STS landing-chain walk (Go otp.go +
//! consent.go port, SDD §2.2 amendment).

use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde_json::Value;
use url::Url;
use uuid::Uuid;

use crate::login::auth::MAX_CHAIN_HOPS;
use crate::login::chain::{auth_start_params, follow_chain, is_consent_chain, query_get, sso_do};
use crate::login::jar::{harvest_device, unquote_cookie, Jar};
use crate::login::xssi::{
    code_check, field_f64, field_string, find_tokens, jar_tokens, parse_loose_json,
};
use crate::login::{Client, LoginError, Session};

/// An OTP code is valid ~10 minutes; challenges expire with it (Go
/// OTPChallengeTTL, otp.go:20).
pub const OTP_CHALLENGE_TTL: Duration = Duration::from_secs(10 * 60);

/// Verification method (Go "Phone" | "Email").
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum OtpMethod {
    Phone,
    Email,
}

impl OtpMethod {
    pub fn as_str(self) -> &'static str {
        match self {
            OtpMethod::Phone => "Phone",
            OtpMethod::Email => "Email",
        }
    }
}

/// One started new-device verification (Go OTPChallenge, otp.go:27-37). The
/// code has been sent to the account owner's phone/email; `submit_otp`
/// completes the login with it. The jar holds the verification-session
/// cookies — the challenge is only usable together with them, in memory
/// only (never SQLite, PORT_DESIGN §1.2).
#[derive(Debug, Clone)]
pub struct OtpChallenge {
    /// Opaque handle for the admin submit endpoint.
    pub id: String,
    pub method: OtpMethod,
    /// Masked destination, e.g. "188****5616".
    pub notify: String,
    /// Passport sid for the final serviceLogin.
    pub sid: String,
    /// 10-minute window (seconds since the epoch).
    pub expires_at: i64,
    /// scheme://host of the passport base.
    pub(crate) pass_base: String,
    /// 4 = phone, 8 = email.
    pub(crate) flag: i64,
    /// Verification-session cookies.
    pub(crate) jar: Jar,
}

impl OtpChallenge {
    fn new(
        method: OtpMethod,
        notify: String,
        sid: String,
        pass_base: String,
        flag: i64,
        jar: Jar,
    ) -> Self {
        Self {
            // Go: "otp-" + RandomHex32().
            id: format!("otp-{}", Uuid::new_v4().simple()),
            method,
            notify,
            sid,
            expires_at: now_secs() + OTP_CHALLENGE_TTL.as_secs() as i64,
            pass_base,
            flag,
            jar,
        }
    }

    pub fn expired(&self) -> bool {
        now_secs() > self.expires_at
    }
}

/// Where the identity-verification steps must run (Go verificationTarget,
/// consent.go:136-143).
pub(crate) struct VerificationTarget {
    pass_base: String,
    sid: String,
    context: String,
    jar: Jar,
}

impl Client {
    /// Walk the notificationUrl chain, agree the cross-region demand when it
    /// appears AND `agree_consent`, and return the sid/context the
    /// /identity/* steps need (Go startVerification, consent.go:151-244).
    /// When `agree_consent` is false the consent POST is never issued — the
    /// demand surfaces as [`LoginError::CrossRegionConsent`].
    pub(crate) async fn start_verification(
        &self,
        notification_url: &str,
        sid_fallback: &str,
        seed_jar: &Jar,
        agree_consent: bool,
    ) -> Result<crate::login::otp::VerificationTarget, LoginError> {
        let mut jar = seed_jar.clone();
        let ntf = self.abs_passport_url(notification_url);
        let u = Url::parse(&ntf)
            .map_err(|e| LoginError::Protocol(format!("notification url parse: {e}")))?;
        // Authority (host[:port]) — Go u.Host keeps the port.
        let host = match u.port() {
            Some(p) => format!("{}:{}", u.host_str().unwrap_or_default(), p),
            None => u.host_str().unwrap_or_default().to_string(),
        };
        let host_noport = u.host_str().unwrap_or_default();
        if host_noport.is_empty() || !self.otp_redirect_ok(host_noport) {
            return Err(LoginError::Protocol(format!(
                "notification url host {host_noport:?} not allowlisted"
            )));
        }
        let pass_base = format!("{}://{host}", u.scheme());
        let mut sid = query_get(&ntf, "sid");
        if sid.is_empty() {
            sid = sid_fallback.to_string();
        }

        let (hops, page) = follow_chain(self, &mut jar, &ntf, MAX_CHAIN_HOPS)
            .await
            .map_err(|e| LoginError::Protocol(format!("notification chain: {e}")))?;

        // Legacy/simple shape: the notificationUrl itself (or its first hop)
        // is the identity entry carrying context.
        let (s, cx) = auth_start_params(&page);
        if !cx.is_empty() {
            let target_sid = if s.is_empty() { sid } else { s };
            return Ok(VerificationTarget {
                pass_base,
                sid: target_sid,
                context: cx,
                jar,
            });
        }

        let (user_id, consent) = is_consent_chain(&hops, &page);
        if !consent {
            return Err(LoginError::Protocol(format!(
                "login chain landed on {page} without an identity context (unsupported demand)"
            )));
        }
        if !agree_consent {
            return Err(LoginError::CrossRegionConsent);
        }
        if user_id.is_empty() {
            return Err(LoginError::Protocol(
                "cross-region consent page carried no userId".into(),
            ));
        }

        // Agree: POST /pass2/userCrossAgreement/update (status 1 = 同意).
        let form = form_urlencoded(&[
            ("userId", user_id.as_str()),
            ("status", "1"),
            ("needClose", "false"),
        ]);
        let (body, _, _) = sso_do(
            self,
            &mut jar,
            reqwest::Method::POST,
            &format!("{pass_base}/pass2/userCrossAgreement/update"),
            Some(&form),
            &[
                ("X-Requested-With", "XMLHttpRequest"),
                ("Referer", page.as_str()),
            ],
        )
        .await
        .map_err(|e| LoginError::Protocol(format!("cross-region consent: {e}")))?;
        let agree = parse_loose_json(&body)
            .map_err(|e| LoginError::Protocol(format!("cross-region consent response: {e}")))?;
        code_check(&agree, "cross-region consent")?;
        let loc = field_string(&agree, "location");
        if loc.is_empty() {
            return Err(LoginError::Protocol(
                "cross-region consent accepted but no location returned".into(),
            ));
        }

        // The location is /pass2/userinfo/check?callback=<authStart…>;
        // walking it plants the identity-session cookies.
        let (_, next) = follow_chain(self, &mut jar, &loc, MAX_CHAIN_HOPS)
            .await
            .map_err(|e| LoginError::Protocol(format!("post-consent chain: {e}")))?;
        let (mut s, mut cx) = auth_start_params(&loc);
        if cx.is_empty() {
            (s, cx) = auth_start_params(&next);
        }
        if cx.is_empty() {
            return Err(LoginError::Protocol(
                "post-consent chain carried no identity context".into(),
            ));
        }
        if !s.is_empty() {
            sid = s;
        }
        Ok(VerificationTarget {
            pass_base,
            sid,
            context: cx,
            jar,
        })
    }

    /// Open a new-device verification session and send the OTP code (Go
    /// startOTP, otp.go:290-380, SDD §2.2 amendment steps 1-4). The send
    /// POST carries X-Requested-With: XMLHttpRequest — MANDATORY (email
    /// send fails 66108 without it, capture 2026-09-23).
    pub(crate) async fn start_otp(
        &self,
        notification_url: &str,
        sid: &str,
        seed_jar: &Jar,
        agree_consent: bool,
    ) -> Result<OtpChallenge, LoginError> {
        let tgt = self
            .start_verification(notification_url, sid, seed_jar, agree_consent)
            .await?;
        let mut jar = tgt.jar;
        let pass_base = tgt.pass_base;
        let sid_param = tgt.sid;
        let otp_ctx = tgt.context;

        // Step 2: identity/list — verification method (flag 4=phone 8=email).
        let list_url = format!(
            "{pass_base}/identity/list?sid={}&supportedMask=0&_locale=zh_CN&context={}",
            urlencode(&sid_param),
            urlencode(&otp_ctx)
        );
        let (body, _, _) = sso_do(self, &mut jar, reqwest::Method::GET, &list_url, None, &[])
            .await
            .map_err(|e| LoginError::Protocol(format!("otp identity list: {e}")))?;
        let mut list = parse_loose_json(&body)
            .map_err(|e| LoginError::Protocol(format!("otp identity list parse: {e}")))?;
        if !list.contains_key("flag") {
            // data-wrapped flag (TDD OTP-003): merge the data object up.
            if let Some(Value::Object(data)) = list.get("data").cloned() {
                for (k, v) in data {
                    list.insert(k, v);
                }
            }
        }
        let mut flag: i64 = 4;
        if let Some(f) = field_f64(&list, "flag") {
            if f as i64 == 8 {
                flag = 8;
            }
        }
        let method = if flag == 8 {
            OtpMethod::Email
        } else {
            OtpMethod::Phone
        };
        let mut notify = String::new();
        for k in [
            "notify",
            "notifyPhone",
            "notifyEmail",
            "maskedPhone",
            "maskedEmail",
            "phone",
            "email",
            "tips",
            "description",
            "desc",
        ] {
            let v = field_string(&list, k);
            if !v.is_empty() {
                notify = v;
                break;
            }
        }

        // Step 3: trigger the verification session. The JSON answer carries
        // the masked destination ({"maskedPhone":"+86 182******16"}) —
        // /identity/list does not include it (live 2026-10-09).
        let trig_url = format!(
            "{pass_base}/identity/auth/verify{}?_flag={}&_json=true",
            method.as_str(),
            flag
        );
        let (body, _, _) = sso_do(self, &mut jar, reqwest::Method::GET, &trig_url, None, &[])
            .await
            .map_err(|e| LoginError::Protocol(format!("otp verify trigger: {e}")))?;
        if let Ok(trig) = parse_loose_json(&body) {
            code_check(&trig, "otp verify trigger")?;
            for k in [
                "maskedPhone",
                "maskedEmail",
                "notify",
                "notifyPhone",
                "notifyEmail",
            ] {
                let v = field_string(&trig, k);
                if !v.is_empty() {
                    notify = v;
                    break;
                }
            }
        }

        // Step 4: send the code (X-Requested-With mandatory).
        let send_url = format!(
            "{pass_base}/identity/auth/send{}Ticket?_dc={}",
            method.as_str(),
            now_millis()
        );
        let form = form_urlencoded(&[("retry", "0"), ("icode", ""), ("_json", "true")]);
        let (body, _, _) = sso_do(
            self,
            &mut jar,
            reqwest::Method::POST,
            &send_url,
            Some(&form),
            &[("X-Requested-With", "XMLHttpRequest")],
        )
        .await
        .map_err(|e| LoginError::Protocol(format!("otp send ticket: {e}")))?;
        if let Ok(sm) = parse_loose_json(&body) {
            code_check(&sm, "otp code send")?;
        }

        Ok(OtpChallenge::new(
            method, notify, sid_param, pass_base, flag, jar,
        ))
    }

    /// Complete the login with the received code (Go SubmitOTP, otp.go:391-462,
    /// SDD §2.2 amendment steps 5-7).
    pub async fn submit_otp(&self, ch: &OtpChallenge, code: &str) -> Result<Session, LoginError> {
        let code = code.trim();
        if code.is_empty() {
            return Err(LoginError::Protocol("otp code is empty".into()));
        }
        if ch.expired() {
            return Err(LoginError::Protocol(
                "otp challenge expired (10-minute window)".into(),
            ));
        }
        let mut jar = ch.jar.clone();

        // Step 5: submit the ticket (trust=true lowers re-trigger frequency).
        let v_url = format!(
            "{}/identity/auth/verify{}?_dc={}",
            ch.pass_base,
            ch.method.as_str(),
            now_millis()
        );
        let form = form_urlencoded(&[
            ("_flag", &ch.flag.to_string()),
            ("ticket", code),
            ("trust", "true"),
            ("_json", "true"),
        ]);
        let (body, _, _) = sso_do(
            self,
            &mut jar,
            reqwest::Method::POST,
            &v_url,
            Some(&form),
            &[("X-Requested-With", "XMLHttpRequest")],
        )
        .await
        .map_err(|e| LoginError::Protocol(format!("otp verify submit: {e}")))?;
        let vm = parse_loose_json(&body).unwrap_or_default();
        let loc = field_string(&vm, "location");
        if loc.is_empty() {
            code_check(&vm, "otp verify")?;
            return Err(LoginError::Protocol(
                "otp verify: response carried no location".into(),
            ));
        }

        // Step 6: walk the landing chain to its end — the session cookies
        // are planted at the upstream STS hop, not the first redirect. The
        // jar is threaded so the harvested tokens survive; passport variants
        // whose landing location is a bare intermediate fall back to the
        // explicit serviceLogin hop.
        let mut sess = match self.sts_exchange_with_jar(&loc, jar.clone()).await {
            Ok(s) => s,
            Err(_) => {
                jar = ch.jar.clone();
                self.otp_service_login_session(ch, &mut jar).await?
            }
        };

        // Step 7: identity — prefer what the chain planted in the jar.
        let (mut pass_token, mut user_id) = jar_tokens(&jar);
        if user_id.is_empty() {
            user_id = sess.user_id.clone();
        }
        if pass_token.is_empty() {
            pass_token = sess.pass_token.clone();
        }
        if user_id.is_empty() || pass_token.is_empty() {
            let next = field_string(&vm, "location");
            let (pt, uid) = self.collect_tokens(&mut jar, &next).await;
            if pass_token.is_empty() {
                pass_token = pt;
            }
            if user_id.is_empty() {
                user_id = uid;
            }
        }
        // passToken/userID are best-effort: only needed for renewal, and the
        // caller keeps the stored values when empty. The session cookie is
        // the deliverable — never throw a working login away over a missing
        // renewal token.
        sess.user_id = user_id;
        sess.pass_token = pass_token;
        sess.device = harvest_device(&jar);
        Ok(sess)
    }

    /// Fallback completion for passport variants that need the explicit
    /// serviceLogin hop (Go otpServiceLoginSession, otp.go:467-506): harvest
    /// passToken/userId, then exchange the offered STS location.
    async fn otp_service_login_session(
        &self,
        ch: &OtpChallenge,
        jar: &mut Jar,
    ) -> Result<Session, LoginError> {
        let sl_url = format!(
            "{}/pass/serviceLogin?sid={}&_json=true&_locale=zh_CN",
            ch.pass_base,
            urlencode(&ch.sid)
        );
        let (body, _, _) = sso_do(self, jar, reqwest::Method::GET, &sl_url, None, &[])
            .await
            .map_err(|e| LoginError::Protocol(format!("otp serviceLogin: {e}")))?;
        let (mut pass_token, mut user_id) = find_tokens(&body);
        if pass_token.is_empty() || user_id.is_empty() {
            let pm = parse_loose_json(&body).unwrap_or_default();
            let next = field_string(&pm, "location");
            let (pt, uid) = self.collect_tokens(jar, &next).await;
            if pass_token.is_empty() {
                pass_token = pt;
            }
            if user_id.is_empty() {
                user_id = uid;
            }
        }
        if pass_token.is_empty() || user_id.is_empty() {
            let (pt, uid) = jar_tokens(jar);
            if !pt.is_empty() && !uid.is_empty() {
                pass_token = pt;
                user_id = uid;
            }
        }
        let (body, _, _) = sso_do(self, jar, reqwest::Method::GET, &sl_url, None, &[])
            .await
            .map_err(|e| LoginError::Protocol(format!("otp sts serviceLogin: {e}")))?;
        let m2 = parse_loose_json(&body)
            .map_err(|e| LoginError::Protocol(format!("otp sts serviceLogin parse: {e}")))?;
        code_check(&m2, "otp sts serviceLogin")?;
        let loc2 = field_string(&m2, "location");
        if loc2.is_empty() {
            return Err(LoginError::Protocol(
                "otp: no STS location after verification".into(),
            ));
        }
        let mut sess = self.sts_exchange_with_jar(&loc2, jar.clone()).await?;
        sess.user_id = user_id;
        sess.pass_token = pass_token;
        Ok(sess)
    }

    /// Follow the whole STS landing chain seeded with the caller's cookies
    /// (Go stsExchangeWithJar, login.go:318-338). Live shape (2026-10-09):
    /// `/identity/result/check → 302 → /pass/serviceLoginAuth2/end → 302 →
    /// <upstream>/sts?sign=… → 307 → /open-apis/user/mi/get → 200` — the
    /// xiaomichatbot_* session cookies are planted at the UPSTREAM hop.
    /// The jar is threaded (passToken/cUserId land in the caller's map).
    pub(crate) async fn sts_exchange_with_jar(
        &self,
        location: &str,
        mut jar: Jar,
    ) -> Result<Session, LoginError> {
        let (hops, _last) = follow_chain(self, &mut jar, location, MAX_CHAIN_HOPS)
            .await
            .map_err(|e| LoginError::Protocol(format!("sts exchange: {e}")))?;
        if hops.is_empty() {
            return Err(LoginError::Protocol("sts exchange: empty chain".into()));
        }
        let token = unquote_cookie(
            jar.get("xiaomichatbot_serviceToken")
                .map(String::as_str)
                .unwrap_or(""),
        );
        if token.is_empty() {
            return Err(LoginError::Protocol(
                "sts response did not plant xiaomichatbot_serviceToken".into(),
            ));
        }
        Ok(self.session_from_jar(&jar, &token))
    }

    /// Compose the working upstream Cookie header from the chain's cookies,
    /// in the real browser's order and quoting (Go sessionFromJar,
    /// login.go:345-364, live capture 2026-10-09: cUserId; userId;
    /// xiaomichatbot_serviceToken/slh/ph quoted; the unquoted `serviceToken=`
    /// spelling kept because the upstream accepts either form). The session's
    /// user_id/pass_token come from the jar only as best-effort defaults —
    /// Login/Renew/SubmitOTP set the authoritative values afterwards.
    pub(crate) fn session_from_jar(&self, jar: &Jar, token: &str) -> Session {
        let ph = unquote_cookie(
            jar.get("xiaomichatbot_ph")
                .map(String::as_str)
                .unwrap_or(""),
        );
        let slh = unquote_cookie(
            jar.get("xiaomichatbot_slh")
                .map(String::as_str)
                .unwrap_or(""),
        );
        let mut parts: Vec<String> = Vec::new();
        if let Some(v) = jar.get("cUserId") {
            if !v.is_empty() {
                parts.push(format!("cUserId={v}"));
            }
        }
        if let Some(v) = jar.get("userId") {
            if !v.is_empty() {
                parts.push(format!("userId={v}"));
            }
        }
        parts.push(format!("serviceToken={token}"));
        parts.push(format!(r#"xiaomichatbot_serviceToken="{token}""#));
        if !slh.is_empty() {
            parts.push(format!(r#"xiaomichatbot_slh="{slh}""#));
        }
        if !ph.is_empty() {
            parts.push(format!(r#"xiaomichatbot_ph="{ph}""#));
        }
        Session {
            cookie: parts.join("; "),
            ph,
            device: harvest_device(jar),
            user_id: unquote_cookie(jar.get("userId").map(String::as_str).unwrap_or("")),
            pass_token: unquote_cookie(jar.get("passToken").map(String::as_str).unwrap_or("")),
        }
    }

    /// Follow the token-harvest redirect chain (Go collectTokens,
    /// otp.go:511-544): ≤2 hops, each host xiaomi.com/allowlisted; tokens
    /// harvested from the jar, the response body tree, and URL query.
    pub(crate) async fn collect_tokens(&self, jar: &mut Jar, location: &str) -> (String, String) {
        let mut next = self.abs_passport_url(location);
        for _ in 0..2 {
            if next.is_empty() {
                break;
            }
            let Ok(u) = Url::parse(&next) else { break };
            if !self.otp_redirect_ok(u.host_str().unwrap_or_default()) {
                break; // open-redirect guard
            }
            let (body, _status, headers) =
                match sso_do(self, jar, reqwest::Method::GET, &next, None, &[]).await {
                    Ok(r) => r,
                    Err(_) => break,
                };
            let (pt, uid) = find_tokens(&body);
            if !pt.is_empty() && !uid.is_empty() {
                return (pt, uid);
            }
            let (pt, uid) = jar_tokens(jar);
            if !pt.is_empty() && !uid.is_empty() {
                return (pt, uid);
            }
            let qpt = query_get(&next, "passToken");
            let quid = query_get(&next, "userId");
            if !qpt.is_empty() && !quid.is_empty() {
                return (qpt, quid);
            }
            // Next hop: Location header or body JSON location.
            let mut hop = headers
                .get(reqwest::header::LOCATION)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_string();
            if hop.is_empty() {
                if let Ok(m) = parse_loose_json(&body) {
                    hop = field_string(&m, "location");
                }
            }
            if !hop.is_empty() && !hop.contains("://") {
                hop = format!(
                    "{}://{}{}",
                    u.scheme(),
                    u.host_str().unwrap_or_default(),
                    hop
                );
            }
            next = hop;
        }
        (String::new(), String::new())
    }
}

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

fn now_millis() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

fn urlencode(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

fn form_urlencoded(form: &[(&str, &str)]) -> String {
    url::form_urlencoded::Serializer::new(String::new())
        .extend_pairs(form.iter().map(|(k, v)| (*k, *v)))
        .finish()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn otp_challenge_id_shape_and_expiry() {
        let jar = Jar::new();
        let ch = OtpChallenge::new(
            OtpMethod::Phone,
            "188****5616".into(),
            "xiaomichatbot".into(),
            "https://account.xiaomi.com".into(),
            4,
            jar,
        );
        assert!(ch.id.starts_with("otp-"));
        assert_eq!(ch.id.len(), "otp-".len() + 32);
        assert!(!ch.expired());
    }
}
