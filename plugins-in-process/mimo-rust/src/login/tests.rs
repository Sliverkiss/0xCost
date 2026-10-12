//! TDD vector replay: LOGIN/RENEW/DEV against a MockPassport
//! (docs/TDD.md §3.5-3.8; no Go *_test.go on the branch, TDD.md is
//! authoritative). Live execution of the OTP/consent/admin paths is
//! forbidden — everything here is mock-only. OTP vectors live in
//! tests_otp.rs (one `mod` split to keep both files under the 800-line
//! gate).

#[path = "tests_otp.rs"]
mod tests_otp;

use crate::login::mockpass::MockPassport;
use crate::login::{jar, Client, Device, LoginError, Options};
use url::Url;

/// Standard loginUrl the mock aistudio hands out; its host is the mock
/// server, so the allowlist must include it (tests pass
/// `passport_hosts = [server host]`).
pub(crate) fn login_url(base: &str) -> String {
    format!("{base}/pass/serviceLogin?callback=https%3A%2F%2Faistudio%2Fsts&sid=xiaomichatbot&qs=abc&_sign=s1&serviceParam=%7B%7D&_group=DEFAULT")
}

/// Recorded POSTs hitting the auth2 endpoint exactly (not `/end`).
pub(crate) fn auth2_posts(mp: &MockPassport) -> Vec<crate::login::mockpass::Recorded> {
    mp.recorded_for("/pass/serviceLoginAuth2")
        .into_iter()
        .filter(|r| r.path == "/pass/serviceLoginAuth2")
        .collect()
}

/// A client pointed at the mock, with the mock's own host allowlisted.
pub(crate) fn client_for(mp: &MockPassport) -> Client {
    let uri = mp.uri();
    let host = Url::parse(&uri)
        .unwrap()
        .host_str()
        .unwrap_or_default()
        .to_string();
    Client::new(Options {
        base: Some(uri),
        passport_hosts: vec![host],
        ..Options::default()
    })
    .unwrap()
}

/// Mount the standard happy-path login: 401 probe with loginUrl → primed
/// login page (plants device cookies) → auth2 success → STS chain → done.
pub(crate) async fn mount_happy_login(mp: &MockPassport) {
    let uri = mp.uri();
    // Probe: 401 with loginUrl (aistudio JSON dialect).
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&uri)),
    )
    .await;

    // Prime: the login page 302s to the account SPA page, planting the
    // device anchor (DEV-008).
    mp.mount_redirect_cookies(
        "GET",
        "/pass/serviceLogin",
        302,
        &format!("{uri}/sign-in"),
        &[
            ("deviceId", "wb_mock-1"),
            ("pass_ua", "web"),
            ("uLocale", "zh_CN"),
            ("JSESSIONID", "js-1"),
        ],
    )
    .await;
    // The SPA page itself: 200, no further redirects.
    mp.mount_json("GET", "/sign-in", 200, "<html>login</html>")
        .await;

    // Auth2: success with location + userId.
    let auth2 = format!(
        r#"&&&START&&&{{"code":0,"location":"{uri}/identity/result/check","userId":10001}}"#
    );
    mp.mount_json_cookies(
        "POST",
        "/pass/serviceLoginAuth2",
        200,
        &auth2,
        &[("passToken", "pt-1"), ("userId", "10001")],
    )
    .await;

    // STS landing chain (live shape): result/check 302 → auth2/end 302 →
    // upstream sts 307 (plants session cookies) → mi/get 200.
    mount_sts_chain(mp, "tok-1", "slh-1", "ph-1").await;
}

/// Mount the live-shaped STS landing chain ending at the upstream
/// `/open-apis/user/mi/get` 200.
pub(crate) async fn mount_sts_chain(mp: &MockPassport, token: &str, slh: &str, ph: &str) {
    let uri = mp.uri();
    mp.mount_redirect(
        "GET",
        "/identity/result/check",
        302,
        &format!("{uri}/pass/serviceLoginAuth2/end"),
    )
    .await;
    mp.mount_redirect(
        "GET",
        "/pass/serviceLoginAuth2/end",
        302,
        &format!("{uri}/sts"),
    )
    .await;
    mp.mount_redirect_cookies(
        "GET",
        "/sts",
        307,
        &format!("{uri}/open-apis/user/mi/get"),
        &[
            ("xiaomichatbot_serviceToken", token),
            ("xiaomichatbot_slh", slh),
            ("xiaomichatbot_ph", ph),
            ("cUserId", "cu-1"),
            ("userId", "10001"),
        ],
    )
    .await;
}

/// Probe + login page (200, plants nothing) — the minimal failure setup.
pub(crate) async fn mount_probe_and_prime(mp: &MockPassport) {
    let uri = mp.uri();
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&uri)),
    )
    .await;
    mp.mount_json("GET", "/pass/serviceLogin", 200, "<html>plain</html>")
        .await;
}

/// Probe + primed login page + an auth2 body of choice (failure vectors).
pub(crate) async fn mount_probe_and_auth2(mp: &MockPassport, auth2_body: String) {
    mount_probe_and_prime(mp).await;
    mp.mount_json("POST", "/pass/serviceLoginAuth2", 200, &auth2_body)
        .await;
}

#[tokio::test]
async fn login_001_context_probe_parses_loginurl_params() {
    let mp = MockPassport::start().await;
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&mp.uri())),
    )
    .await;
    let c = client_for(&mp);
    let lctx = c.fetch_login_context().await.unwrap();
    assert_eq!(lctx.callback, "https://aistudio/sts");
    assert_eq!(lctx.sid, "xiaomichatbot");
    assert_eq!(lctx.qs, "abc");
    assert_eq!(lctx.sign, "s1");
    assert_eq!(lctx.service_param, "{}");
    assert_eq!(lctx.group, "DEFAULT");
}

#[tokio::test]
async fn login_002_password_auth_form_and_session() {
    let mp = MockPassport::start().await;
    mount_happy_login(&mp).await;
    let c = client_for(&mp);
    let sess = c.login("18800005616", "password").await.unwrap();

    // The auth2 POST: form-encoded with the Go field set; hash = MD5 hex
    // upper, no salt.
    let posts = auth2_posts(&mp);
    assert_eq!(posts.len(), 1);
    let p = &posts[0];
    assert_eq!(p.method, "POST");
    assert_eq!(
        p.header("content-type"),
        "application/x-www-form-urlencoded"
    );
    assert!(p.body.contains("user=18800005616"));
    assert!(p.body.contains("hash=5F4DCC3B5AA765D61D8327DEB882CF99"));
    assert!(p.body.contains("policyName=miaccount"));
    assert!(p.body.contains("captCode="));
    assert!(p.body.contains("_json=true"));
    assert!(p.body.contains("sid=xiaomichatbot"));
    assert!(p.body.contains("qs=abc"));
    assert!(p.body.contains("_sign=s1"));
    assert!(p.body.contains("_group=DEFAULT"));

    // Session: cookie order + quoting (LOGIN-006).
    assert_eq!(
        sess.cookie,
        "cUserId=cu-1; userId=10001; serviceToken=tok-1; \
         xiaomichatbot_serviceToken=\"tok-1\"; xiaomichatbot_slh=\"slh-1\"; \
         xiaomichatbot_ph=\"ph-1\""
    );
    assert_eq!(sess.ph, "ph-1");
    assert_eq!(sess.user_id, "10001");
    assert_eq!(sess.pass_token, "pt-1");
    // STS chain walked to the end (three hops + final page), not just the
    // first redirect: /open-apis/user/mi/get was hit twice — once as the
    // login-context probe, once as the chain's landing page.
    assert_eq!(mp.recorded_for("/identity/result/check").len(), 1);
    assert_eq!(mp.recorded_for("/pass/serviceLoginAuth2/end").len(), 1);
    assert_eq!(mp.recorded_for("/sts").len(), 1);
    assert_eq!(mp.recorded_for("/open-apis/user/mi/get").len(), 2);
}

#[tokio::test]
async fn login_004_captcha_demand_surfaces_account_action() {
    let mp = MockPassport::start().await;
    mount_probe_and_auth2(
        &mp,
        r#"&&&START&&&{"code":70005,"captchaUrl":"https://acct.example/captcha"}"#.into(),
    )
    .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::AccountAction { reason, url, .. } => {
            assert!(reason.contains("captcha"), "{reason}");
            assert!(url.contains("captcha"));
        }
        other => panic!("expected AccountAction, got {other:?}"),
    }
}

#[tokio::test]
async fn login_005_otp_demand_code_nonzero_surfaces_account_action() {
    let mp = MockPassport::start().await;
    mount_probe_and_auth2(
        &mp,
        r#"&&&START&&&{"code":70016,"notificationUrl":"https://x/identity/start"}"#.into(),
    )
    .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::AccountAction { reason, url, .. } => {
            assert!(reason.contains("device verification"), "{reason}");
            assert!(!url.is_empty());
        }
        other => panic!("expected AccountAction, got {other:?}"),
    }
}

#[tokio::test]
async fn login_009_nonzero_code_no_location_no_flags() {
    let mp = MockPassport::start().await;
    mount_probe_and_auth2(
        &mp,
        r#"&&&START&&&{"code":70003,"description":"denied"}"#.into(),
    )
    .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::Protocol(m) => assert!(m.contains("code=70003") && m.contains("denied")),
        other => panic!("expected Protocol, got {other:?}"),
    }
}

#[tokio::test]
async fn login_008_location_host_allowlist_refuses_evil() {
    let mp = MockPassport::start().await;
    // Auth2 succeeds (code=0, userId, passToken planted as live) but points
    // the STS location at a hostile host: the chain walk refuses
    // (open-redirect guard).
    mount_probe_and_prime(&mp).await;
    mp.mount_json_cookies(
        "POST",
        "/pass/serviceLoginAuth2",
        200,
        r#"&&&START&&&{"code":0,"location":"https://evil.example/sts","userId":10001}"#,
        &[("passToken", "pt-1"), ("userId", "10001")],
    )
    .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::Protocol(m) => assert!(m.contains("not allowlisted"), "{m}"),
        other => panic!("expected Protocol, got {other:?}"),
    }
}

#[tokio::test]
async fn rec_003_auth2_5xx_not_parsed_as_login_result() {
    let mp = MockPassport::start().await;
    mount_probe_and_prime(&mp).await;
    mp.mount_json("POST", "/pass/serviceLoginAuth2", 502, "proxy error page")
        .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::Transport(m) => assert!(m.contains("status 502"), "{m}"),
        other => panic!("expected Transport, got {other:?}"),
    }
}

#[tokio::test]
async fn rec_003_auth2_429_not_parsed_as_login_result() {
    let mp = MockPassport::start().await;
    mount_probe_and_prime(&mp).await;
    mp.mount_json("POST", "/pass/serviceLoginAuth2", 429, "slow down")
        .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::Transport(m) => assert!(m.contains("status 429"), "{m}"),
        other => panic!("expected Transport, got {other:?}"),
    }
}

#[tokio::test]
async fn dev_001_stored_device_presented_on_auth2() {
    let mp = MockPassport::start().await;
    // Probe + a quiet prime page (plants nothing, so the stored device is
    // what rides on the POST) + auth2 error body — the vector only asserts
    // the POST's Cookie header.
    mount_probe_and_auth2(&mp, r#"&&&START&&&{"code":70003,"description":"x"}"#.into()).await;
    let mut device = Device::new();
    device.insert("deviceId".into(), "wb_stored-1".into());
    device.insert("pass_ua".into(), "web".into());
    device.insert("uLocale".into(), "zh_CN".into());
    device.insert("cUserId".into(), "cu-9".into());
    let c = client_for(&mp).with_device(&device);
    let _ = c.login("188", "pw").await;
    let posts = auth2_posts(&mp);
    assert_eq!(posts.len(), 1);
    // Every stored device cookie rides on the POST.
    assert!(posts[0].cookie.contains("deviceId=wb_stored-1"));
    assert!(posts[0].cookie.contains("pass_ua=web"));
    assert!(posts[0].cookie.contains("uLocale=zh_CN"));
    assert!(posts[0].cookie.contains("cUserId=cu-9"));
}

#[tokio::test]
async fn dev_002_no_device_stored_none_invented() {
    let mp = MockPassport::start().await;
    mount_probe_and_prime(&mp).await;
    mp.mount_json(
        "POST",
        "/pass/serviceLoginAuth2",
        200,
        r#"&&&START&&&{"code":70003,"description":"x"}"#,
    )
    .await;
    let c = client_for(&mp);
    let _ = c.login("188", "pw").await;
    let posts = auth2_posts(&mp);
    assert_eq!(posts.len(), 1);
    assert!(!posts[0].cookie.contains("deviceId="));
    assert!(!posts[0].cookie.contains("pass_ua="));
}

#[tokio::test]
async fn dev_003_device_harvested_from_login_no_session_cookies() {
    let mp = MockPassport::start().await;
    mount_happy_login(&mp).await;
    let c = client_for(&mp);
    let sess = c.login("188", "pw").await.unwrap();
    // The primed page planted deviceId/pass_ua/uLocale — the session's
    // device carries them and NO session cookie.
    assert_eq!(
        sess.device.get("deviceId").map(String::as_str),
        Some("wb_mock-1")
    );
    assert_eq!(sess.device.get("pass_ua").map(String::as_str), Some("web"));
    assert_eq!(
        sess.device.get("uLocale").map(String::as_str),
        Some("zh_CN")
    );
    for k in ["serviceToken", "passToken", "xiaomichatbot_ph"] {
        assert!(
            !sess.device.contains_key(k),
            "session cookie {k} leaked into device"
        );
    }
}

#[tokio::test]
async fn dev_008_login_page_primed_before_auth2_with_navigation_headers() {
    let mp = MockPassport::start().await;
    mount_happy_login(&mp).await;
    let c = client_for(&mp);
    c.login("188", "pw").await.unwrap();
    // Exactly one navigation of the login page.
    let primes = mp.recorded_for("/pass/serviceLogin");
    assert_eq!(primes.len(), 1);
    let p = &primes[0];
    // Chrome's document navigation headers (DEV-008/010).
    assert_eq!(p.header("sec-fetch-mode"), "navigate");
    assert_eq!(p.header("Upgrade-Insecure-Requests"), "1");
    assert!(p.header("accept").starts_with("text/html"));
    assert!(p.header("referer").starts_with("http"));
    // The auth2 POST carries the anchor the page issued.
    let posts = auth2_posts(&mp);
    assert!(posts[0].cookie.contains("deviceId=wb_mock-1"));
    assert!(posts[0].cookie.contains("pass_ua=web"));
    assert!(posts[0].cookie.contains("uLocale=zh_CN"));
    assert!(posts[0].cookie.contains("JSESSIONID=js-1"));
}

#[tokio::test]
async fn dev_010_auth2_form_post_header_set() {
    let mp = MockPassport::start().await;
    mount_happy_login(&mp).await;
    let c = client_for(&mp);
    c.login("188", "pw").await.unwrap();
    let p = &mp.recorded_for("/pass/serviceLoginAuth2")[0];
    assert_eq!(
        p.header("content-type"),
        "application/x-www-form-urlencoded"
    );
    assert_eq!(p.header("sec-fetch-mode"), "navigate");
    assert_eq!(p.header("sec-fetch-site"), "same-origin");
    assert_eq!(p.header("sec-fetch-dest"), "document");
    assert_eq!(p.header("Upgrade-Insecure-Requests"), "1");
    assert!(!p.header("origin").is_empty());
    assert!(!p.header("referer").is_empty());
    assert!(!p.header("sec-ch-ua").is_empty());
    assert_eq!(p.header("accept-language"), "zh-CN,zh;q=0.9");
    assert!(!p.header("user-agent").is_empty());
    assert!(p.header("accept").starts_with("text/html"));
}

// DEV-011 is mounted + asserted in tests_otp.rs (it needs the OTP chain);
// no placeholder test here — a constant assertion buys nothing.

#[tokio::test]
async fn dev_007_device_merge_semantics() {
    let mut stored = Device::new();
    stored.insert("deviceId".into(), "wb-1".into());
    stored.insert("uLocale".into(), "zh_CN".into());
    let mut fresh = Device::new();
    fresh.insert("deviceId".into(), "wb-2".into());
    fresh.insert("sdkVersion".into(), "3.9".into());
    let merged = jar::merge_device(&stored, &fresh);
    // Fresh wins, silent kept.
    assert_eq!(merged.get("deviceId").map(String::as_str), Some("wb-2"));
    assert_eq!(merged.get("uLocale").map(String::as_str), Some("zh_CN"));
    assert_eq!(merged.get("sdkVersion").map(String::as_str), Some("3.9"));
    // Stored never aliased.
    assert_eq!(stored.get("deviceId").map(String::as_str), Some("wb-1"));
    // Both empty → empty.
    assert!(jar::merge_device(&Device::new(), &Device::new()).is_empty());
}

#[tokio::test]
async fn renew_001_002_sts_from_passttoken_with_device_cookies() {
    let mp = MockPassport::start().await;
    let uri = mp.uri();
    // Probe answers a loginUrl for the renewal.
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&uri)),
    )
    .await;
    // serviceLogin answers the STS location (XSSI).
    mp.mount_json(
        "GET",
        "/pass/serviceLogin",
        200,
        &format!(r#"&&&START&&&{{"code":0,"location":"{uri}/sts"}}"#),
    )
    .await;
    // STS chain plants a rotated passToken (DEV-006).
    mp.mount_redirect_cookies(
        "GET",
        "/sts",
        307,
        &format!("{uri}/open-apis/user/mi/get"),
        &[
            ("xiaomichatbot_serviceToken", "tok-2"),
            ("xiaomichatbot_ph", "ph-2"),
            ("userId", "10001"),
            ("passToken", "pt-rotated"),
        ],
    )
    .await;

    let mut device = Device::new();
    device.insert("deviceId".into(), "wb_stored-1".into());
    let c = client_for(&mp).with_device(&device);
    let sess = c.renew("10001", "pt-1").await.unwrap();

    // The serviceLogin hop carries userId + passToken + device (DEV-005).
    let sls = mp.recorded_for("/pass/serviceLogin");
    assert_eq!(sls.len(), 1);
    assert!(sls[0].cookie.contains("userId=10001"));
    assert!(sls[0].cookie.contains("passToken=pt-1"));
    assert!(sls[0].cookie.contains("deviceId=wb_stored-1"));

    // Rotated passToken wins (DEV-006).
    assert_eq!(sess.pass_token, "pt-rotated");
    assert!(sess.cookie.contains("serviceToken=tok-2"));
    assert_eq!(sess.ph, "ph-2");
}

#[tokio::test]
async fn renew_003_passtoken_dead_70016() {
    let mp = MockPassport::start().await;
    let uri = mp.uri();
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&uri)),
    )
    .await;
    mp.mount_json(
        "GET",
        "/pass/serviceLogin",
        200,
        r#"&&&START&&&{"code":70016,"description":"expired"}"#,
    )
    .await;
    let c = client_for(&mp);
    let err = c.renew("10001", "pt-1").await.unwrap_err();
    match err {
        LoginError::PassTokenDead(d) => assert!(d.contains("expired"), "{d}"),
        other => panic!("expected PassTokenDead, got {other:?}"),
    }
}

#[tokio::test]
async fn renew_009_validate_shapes() {
    // 200 + business code != 0 → Auth.
    let mp = MockPassport::start().await;
    mp.mount_json("GET", "/open-apis/user/mi/get", 200, r#"{"code":1}"#)
        .await;
    let c = client_for(&mp);
    let err = c
        .validate("userId=10001; serviceToken=tok")
        .await
        .unwrap_err();
    match err {
        LoginError::Auth(m) => assert!(m.contains("business code 1"), "{m}"),
        other => panic!("expected Auth, got {other:?}"),
    }

    // 401 → Auth (is_auth_expired true).
    let mp2 = MockPassport::start().await;
    mp2.mount_json("GET", "/open-apis/user/mi/get", 401, "{}")
        .await;
    let c2 = client_for(&mp2);
    let err2 = c2
        .validate("userId=10001; serviceToken=tok")
        .await
        .unwrap_err();
    assert!(err2.is_auth_expired());

    // Clean 200 code=0 passes.
    let mp3 = MockPassport::start().await;
    mp3.mount_json("GET", "/open-apis/user/mi/get", 200, r#"{"code":0}"#)
        .await;
    let c3 = client_for(&mp3);
    c3.validate("userId=10001; serviceToken=tok").await.unwrap();
}
