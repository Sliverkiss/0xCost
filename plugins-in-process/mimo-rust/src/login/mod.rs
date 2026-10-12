//! Passport login chain: client identity, header sets, error taxonomy
//! (Go `internal/mimo` client.go/passport.go port, SDD §2.4).
//!
//! Two header surfaces exist on purpose and must never merge
//! (client.go:290-295): the aistudio SPA surface (fetch-based, no
//! X-Requested-With) and the identity/passport surface (axios-shaped XHR
//! where X-Requested-With is mandatory — email ticket send fails 66108
//! without it, packet capture 2026-09-23).

mod auth;
mod chain;
mod jar;
#[cfg(test)]
mod mockpass; // wiremock mock passport + recorded requests (tests.rs)
mod otp;
#[cfg(test)]
mod tests;
mod xssi;

pub use auth::{LoginContext, Session};
pub use jar::{merge_device, Device, Jar};
pub use otp::{OtpChallenge, OtpMethod, OTP_CHALLENGE_TTL};

use std::collections::HashMap;
use std::time::Duration;

use thiserror::Error;
use url::Url;

/// Default upstream base (client.go:93).
pub const DEFAULT_BASE: &str = "https://aistudio.xiaomimimo.com";
/// Default User-Agent: Chrome 153 (client.go:99).
pub const DEFAULT_USER_AGENT: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) \
AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36";
/// Default sec-ch-ua (client.go:102).
pub const DEFAULT_SEC_CH_UA: &str =
    r#""Google Chrome";v="153", "Chromium";v="153", "Not)A;Brand";v="24""#;
/// Default x-timeZone (client.go:105).
pub const DEFAULT_TIMEZONE: &str = "Asia/Shanghai";
/// Default passport host allowlist (client.go:127).
pub const DEFAULT_PASSPORT_HOST: &str = "account.xiaomi.com";
/// Default no-SSE-event watchdog window (SDD §7.3).
pub const DEFAULT_IDLE_TIMEOUT: Duration = Duration::from_secs(180);
/// Overall request timeout (Go: 30-minute http.Client, client.go:109).
pub const REQUEST_TIMEOUT: Duration = Duration::from_secs(30 * 60);

/// Chrome's navigation/HTML-form Accept (passport.go:32, verbatim live
/// capture).
pub const CHROME_ACCEPT_HTML: &str = "text/html,application/xhtml+xml,\
application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,\
application/signed-exchange;v=b3;q=0.7";
/// The account SPA's axios Accept (passport.go:34).
pub const CHROME_ACCEPT_JSON: &str = "application/json, text/plain, */*";
/// Chrome's raw navigation Accept-Language (passport.go:35) — passport
/// surface only; the aistudio SPA sets `zh-CN` itself.
pub const CHROME_ACCEPT_LANG: &str = "zh-CN,zh;q=0.9";

/// Login error taxonomy (Go login.go/otp.go/consent.go sentinels).
#[derive(Debug, Error)]
pub enum LoginError {
    /// Transport-level failure (connect/timeout/read).
    #[error("{0}")]
    Transport(String),
    /// A passport response failed to parse (XSSI/JSON shape).
    #[error("{0}")]
    Protocol(String),
    /// Login outcome that needs a human: captcha, manual re-login
    /// (Go ErrAccountAction, TDD LOGIN-004/005).
    #[error("{reason}")]
    AccountAction {
        reason: String,
        url: String,
        #[source]
        cause: Option<Box<LoginError>>,
        /// Device identity harvested before the login stopped — the caller
        /// persists it (device.go, ADR 0013).
        device: Device,
    },
    /// Login hit the new-device OTP demand and auto-started a challenge;
    /// the code arrives out-of-band (Go ErrOTPPending).
    #[error("new-device OTP verification required; code sent to {notify}")]
    OtpPending {
        challenge: Box<OtpChallenge>,
        device: Device,
        /// Masked destination for the error message (challenge.notify).
        notify: String,
    },
    /// Cross-region data-transfer agreement demanded and auto-agree off
    /// (Go ErrCrossRegionConsent, consent.go:42).
    #[error(
        "login demands the cross-region personal-data-transfer agreement \
         (MIMO_RUST_CROSS_REGION_AGREE=false)"
    )]
    CrossRegionConsent,
    /// passToken dead, code 70016 — full re-login needed (Go ErrPassTokenDead).
    #[error("passToken expired (code 70016): {0}")]
    PassTokenDead(String),
    /// Session expired/invalid (Go AuthError) — the caller retries with one
    /// transparent renewal before failing.
    #[error("{0}")]
    Auth(String),
}

impl LoginError {
    /// Auth-expired signal (Go IsAuthExpiredErr): the retry layer refreshes
    /// the session once.
    pub fn is_auth_expired(&self) -> bool {
        matches!(self, LoginError::Auth(_))
    }
}

/// Client options (Go client.go:67-88). `base` may be a mock-server URL in
/// tests; `passport_hosts` points the allowlist at a mock passport.
#[derive(Debug, Clone, Default)]
pub struct Options {
    pub base: Option<String>,
    pub user_agent: Option<String>,
    pub sec_ch_ua: Option<String>,
    pub timezone: Option<String>,
    pub passport_hosts: Vec<String>,
    /// SSE idle watchdog (SDD §7.3; tests override small).
    pub idle_timeout: Option<Duration>,
    /// Disable auto-starting the new-device OTP challenge (MIMO_RUST_OTP_AUTO=false).
    pub disable_otp_auto: bool,
    /// Disable auto-agreeing the cross-region demand (MIMO_RUST_CROSS_REGION_AGREE=false).
    pub disable_cross_region_agree: bool,
    /// Egress label → proxy URL. Every call through [`Client::for`] dials
    /// through its label's proxy (SDD §4.8).
    pub egress: HashMap<String, Url>,
}

/// The upstream client (Go Client, client.go:22-46). Built once; `for` and
/// `with_device` return shallow copies that never mutate the receiver.
#[derive(Debug, Clone)]
pub struct Client {
    http: reqwest::Client,
    /// One client per egress label, identical except the proxy (SDD §4.8).
    egress_http: HashMap<String, reqwest::Client>,
    base: String,
    upstream_host: String,
    user_agent: String,
    sec_ch_ua: String,
    timezone: String,
    passport_hosts: Vec<String>,
    idle_timeout: Duration,
    disable_otp_auto: bool,
    disable_cross_region_agree: bool,
    /// Device identity (passport cookies) replayed on every login/renewal
    /// hop (device.go; empty = present none, the pre-2026-10-09 behaviour).
    device: Device,
}

/// The shared, unlabelled egress label (client.go:149).
pub const EGRESS_DIRECT: &str = "direct";

impl Client {
    /// Build a client (Go New, client.go:91-146).
    pub fn new(opts: Options) -> Result<Self, LoginError> {
        let base = normalize_base(opts.base.as_deref());
        let user_agent = opts
            .user_agent
            .unwrap_or_else(|| DEFAULT_USER_AGENT.to_string());
        let sec_ch_ua = opts
            .sec_ch_ua
            .unwrap_or_else(|| DEFAULT_SEC_CH_UA.to_string());
        let timezone = opts
            .timezone
            .unwrap_or_else(|| DEFAULT_TIMEZONE.to_string());
        let upstream_host = Url::parse(&base)
            .map(|u| u.host_str().unwrap_or_default().to_string())
            .map_err(|e| LoginError::Protocol(format!("base url parse: {e}")))?;
        let mut hosts = opts.passport_hosts;
        if hosts.is_empty() {
            hosts.push(DEFAULT_PASSPORT_HOST.to_string());
        }

        // Manual redirects everywhere (Policy::none): every hop of every
        // chain is followed in plugin code so intermediate Set-Cookie is
        // captured (PORT_DESIGN §1.2 — a stock following client loses them).
        let http = build_http(None)?;
        let mut egress_http = HashMap::new();
        for (label, proxy) in &opts.egress {
            egress_http.insert(label.clone(), build_http(Some(proxy))?);
        }

        Ok(Self {
            http,
            egress_http,
            base,
            upstream_host,
            user_agent,
            sec_ch_ua,
            timezone,
            passport_hosts: hosts,
            idle_timeout: opts.idle_timeout.unwrap_or(DEFAULT_IDLE_TIMEOUT),
            disable_otp_auto: opts.disable_otp_auto,
            disable_cross_region_agree: opts.disable_cross_region_agree,
            device: Device::new(),
        })
    }

    /// The client whose requests leave through an egress label (Go For,
    /// client.go:160-171). An unknown label returns the receiver unchanged:
    /// a label without an endpoint is rejected at startup, so failing here
    /// would turn a config mistake into a per-request error storm.
    pub fn for_egress(&self, label: &str) -> Client {
        if label.is_empty() || label == EGRESS_DIRECT {
            return self.clone();
        }
        match self.egress_http.get(label) {
            Some(hc) => {
                let mut cp = self.clone();
                cp.http = hc.clone();
                cp
            }
            None => self.clone(),
        }
    }

    /// Shallow copy presenting the given device identity (Go WithDevice,
    /// client.go:56-63). Empty device yields the receiver unchanged.
    pub fn with_device(&self, device: &Device) -> Client {
        if device.is_empty() {
            return self.clone();
        }
        let mut cp = self.clone();
        cp.device = device.clone();
        cp
    }

    pub fn base(&self) -> &str {
        &self.base
    }

    pub fn idle_timeout(&self) -> Duration {
        self.idle_timeout
    }

    /// Host-allowlist check (Go hostAllowed, login.go:83-91): exact match,
    /// case-insensitive, one trailing dot tolerated.
    pub(crate) fn host_allowed(&self, host: &str) -> bool {
        host_allowed(host, &self.passport_hosts)
    }

    /// Redirect-target guard for the OTP/STS chains (Go otpRedirectOK,
    /// otp.go:225-234): xiaomi.com apex/subdomain, the upstream host, or the
    /// allowlist — the post-verification landing chain ends at
    /// `<upstream>/sts` (live 2026-10-09).
    pub(crate) fn otp_redirect_ok(&self, host: &str) -> bool {
        let host = normalize_host(host);
        if host == "xiaomi.com" || host.ends_with(".xiaomi.com") {
            return true;
        }
        if !self.upstream_host.is_empty() && host == self.upstream_host.to_lowercase() {
            return true;
        }
        self.host_allowed(&host)
    }

    /// Resolve a possibly-relative URL against the first allowlisted host
    /// (Go absPassportURL, otp.go:238-254).
    pub(crate) fn abs_passport_url(&self, maybe: &str) -> String {
        let s = maybe.trim();
        if s.is_empty() {
            return String::new();
        }
        if s.starts_with("http://") || s.starts_with("https://") {
            return s.to_string();
        }
        let host = self
            .passport_hosts
            .first()
            .map(String::as_str)
            .unwrap_or(DEFAULT_PASSPORT_HOST);
        let path = if s.starts_with('/') {
            s.to_string()
        } else {
            format!("/{s}")
        };
        format!("https://{host}{path}")
    }

    /// The aistudio SPA surface (Go setAistudioHeaders, client.go:296-313):
    /// fetch defaults — Accept `*/*`, the SPA's own Accept-Language `zh-CN`
    /// (NOT the passport `zh-CN,zh;q=0.9`), x-timeZone, same-origin
    /// sec-fetch triplet. No Content-Type: this client is a GET most of the
    /// time and a fetch GET carries no entity header.
    pub(crate) fn aistudio_headers(&self) -> reqwest::header::HeaderMap {
        let mut h = reqwest::header::HeaderMap::new();
        set_header(&mut h, "accept", "*/*");
        set_header(&mut h, "accept-language", "zh-CN");
        set_header(&mut h, "x-timeZone", &self.timezone);
        set_header(&mut h, "origin", &self.base);
        set_header(&mut h, "referer", &format!("{}/", self.base));
        self.common_sec_headers(&mut h);
        set_header(&mut h, "sec-fetch-dest", "empty");
        set_header(&mut h, "sec-fetch-mode", "cors");
        set_header(&mut h, "sec-fetch-site", "same-origin");
        h
    }

    /// The upload-PUT variant (Go setAistudioStorageHeaders,
    /// client.go:326-339): XHR branch — cross-site sec-fetch, no Cookie
    /// travels to storage (ADR 0035). v1 chat does not upload, kept for
    /// fidelity documentation.
    #[allow(dead_code)]
    pub(crate) fn aistudio_storage_headers(&self) -> reqwest::header::HeaderMap {
        let mut h = self.aistudio_headers();
        h.remove("sec-fetch-site");
        set_header(&mut h, "sec-fetch-site", "cross-site");
        h
    }

    /// Chrome navigating to a document (Go setNavigationHeaders,
    /// passport.go:41-56): HTML Accept, Upgrade-Insecure-Requests, client
    /// hints, cross-site top-level navigation.
    pub(crate) fn navigation_headers(&self, referer: &str) -> reqwest::header::HeaderMap {
        let mut h = reqwest::header::HeaderMap::new();
        set_header(&mut h, "accept", CHROME_ACCEPT_HTML);
        set_header(&mut h, "accept-language", CHROME_ACCEPT_LANG);
        set_header(&mut h, "Upgrade-Insecure-Requests", "1");
        self.common_sec_headers(&mut h);
        set_header(&mut h, "sec-fetch-dest", "document");
        set_header(&mut h, "sec-fetch-mode", "navigate");
        set_header(&mut h, "sec-fetch-site", "cross-site");
        set_header(&mut h, "sec-fetch-user", "?1");
        if !referer.is_empty() {
            set_header(&mut h, "referer", referer);
        }
        h
    }

    /// The HTML form POST a browser submits from the login page (Go
    /// setFormPostHeaders, passport.go:63-68): navigation set plus form
    /// encoding and the page's own origin.
    pub(crate) fn form_post_headers(
        &self,
        origin: &str,
        referer: &str,
    ) -> reqwest::header::HeaderMap {
        let mut h = reqwest::header::HeaderMap::new();
        set_header(&mut h, "content-type", "application/x-www-form-urlencoded");
        for (k, v) in self.navigation_headers(referer).iter() {
            h.insert(k, v.clone());
        }
        h.remove("sec-fetch-site");
        set_header(&mut h, "sec-fetch-site", "same-origin");
        set_header(&mut h, "origin", origin);
        h
    }

    /// The account SPA's same-origin XHR (Go setXHRHeaders, passport.go:72-89):
    /// JSON Accept, X-Requested-With mandatory, cors/same-origin.
    pub(crate) fn xhr_headers(&self, origin: &str, referer: &str) -> reqwest::header::HeaderMap {
        let mut h = reqwest::header::HeaderMap::new();
        set_header(&mut h, "accept", CHROME_ACCEPT_JSON);
        set_header(&mut h, "accept-language", CHROME_ACCEPT_LANG);
        set_header(&mut h, "X-Requested-With", "XMLHttpRequest");
        self.common_sec_headers(&mut h);
        set_header(&mut h, "sec-fetch-dest", "empty");
        set_header(&mut h, "sec-fetch-mode", "cors");
        set_header(&mut h, "sec-fetch-site", "same-origin");
        if !origin.is_empty() {
            set_header(&mut h, "origin", origin);
        }
        if !referer.is_empty() {
            set_header(&mut h, "referer", referer);
        }
        h
    }

    /// User-Agent + client hints shared by every set (passport.go).
    fn common_sec_headers(&self, h: &mut reqwest::header::HeaderMap) {
        set_header(h, "user-agent", &self.user_agent);
        set_header(h, "sec-ch-ua", &self.sec_ch_ua);
        set_header(h, "sec-ch-ua-mobile", "?0");
        set_header(h, "sec-ch-ua-platform", "\"Windows\"");
    }
}

/// Trim trailing slashes off a base URL (client.go:95-97).
fn normalize_base(base: Option<&str>) -> String {
    let mut b = base.unwrap_or(DEFAULT_BASE).trim().to_string();
    while b.ends_with('/') {
        b.pop();
    }
    b
}

/// Lowercase + strip one trailing dot (login.go:84, otp.go:226).
pub(crate) fn normalize_host(host: &str) -> String {
    let h = host.trim().to_lowercase();
    h.strip_suffix('.').unwrap_or(&h).to_string()
}

/// Exact-match host allowlist (login.go:83-91).
pub(crate) fn host_allowed(host: &str, allow: &[String]) -> bool {
    let h = normalize_host(host);
    allow.iter().any(|a| normalize_host(a) == h)
}

fn set_header(h: &mut reqwest::header::HeaderMap, name: &str, value: &str) {
    use reqwest::header::{HeaderName, HeaderValue};
    let name = HeaderName::from_bytes(name.as_bytes()).expect("static header names are valid");
    let value = HeaderValue::from_str(value).expect("static header values are valid");
    h.insert(name, value);
}

/// One manual-redirect HTTP client, optionally proxied (client.go New).
fn build_http(proxy: Option<&Url>) -> Result<reqwest::Client, LoginError> {
    let mut b = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(REQUEST_TIMEOUT);
    if let Some(p) = proxy {
        b = b.proxy(
            reqwest::Proxy::all(p.clone())
                .map_err(|e| LoginError::Transport(format!("egress proxy {p}: {e}")))?,
        );
    }
    b.build()
        .map_err(|e| LoginError::Transport(format!("http client: {e}")))
}
