//! TDD OTP vector replay (docs/TDD.md §3.7): OTP-001..011, OTP-CHAIN-001,
//! DEV-004/009/011. All against the MockPassport — the OTP/consent/admin
//! paths are never executed live.

use super::{auth2_posts, client_for, login_url, mount_happy_login, mount_sts_chain};
use crate::login::mockpass::MockPassport;
use crate::login::{Client, Device, LoginError, Options};

/// Mount the code=0 OTP demand (live-captured 2026-10-08 shape:
/// securityStatus=128, empty location, notificationUrl) on auth2.
async fn mount_otp_demand_auth2(mp: &MockPassport) {
    let uri = mp.uri();
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&uri)),
    )
    .await;
    // Prime page: 200, plants a fresh deviceId (the device-precedence test
    // in DEV-004 relies on Set-Cookie winning over the replayed value). The
    // navigation carries no query; the fallback test's session-flow GET on
    // the same path carries `_json=true` and mounts its own answer.
    mp.mount_json_qp_missing_cookies(
        "GET",
        "/pass/serviceLogin",
        "_json",
        200,
        "<html>plain</html>",
        &[("deviceId", "wb_mock-2")],
    )
    .await;
    // Auth2: code=0 + empty location + notificationUrl = new-device OTP.
    mp.mount_json(
        "POST",
        "/pass/serviceLoginAuth2",
        200,
        &format!(
            r#"&&&START&&&{{"code":0,"securityStatus":128,"location":"","notificationUrl":"{uri}/identity/result/check"}}"#
        ),
    )
    .await;
}

/// Mount the identity chain the notificationUrl leads into: result/check →
/// (302, once) → the authStart identity entry, then identity/list, the
/// verify trigger, and the send ticket. The redirect matches only the
/// first hit: after SubmitOTP the same URL is the landing chain start,
/// which the submit mounts answer (wiremock drops an exhausted
/// up_to_n_times mock from matching).
async fn mount_identity_chain(mp: &MockPassport, flag: &str, notify: &str, masked: &str) {
    let uri = mp.uri();
    // Notification chain: result/check 302 → authStart page carrying
    // sid + context (the "legacy/simple shape" of startVerification).
    mp.mount_redirect_once(
        "GET",
        "/identity/result/check",
        302,
        &format!("{uri}/fe/service/identity/authStart?sid=xiaomichatbot&context=ctx-otp"),
    )
    .await;
    mp.mount_json(
        "GET",
        "/fe/service/identity/authStart",
        200,
        "<html></html>",
    )
    .await;

    // identity/list: flag 4/8 (+ masked notify field per the vector).
    mp.mount_json(
        "GET",
        "/identity/list",
        200,
        &format!(r#"{{"flag":{flag},"{notify}":"{masked}"}}"#),
    )
    .await;

    // Verify trigger: the live shape carries the masked destination, but the
    // vectors exercise the notify field from identity/list — mount a bare
    // trigger so the list value survives.
    mp.mount_json("GET", "/identity/auth/verifyPhone", 200, r#"{}"#)
        .await;
    mp.mount_json("GET", "/identity/auth/verifyEmail", 200, r#"{}"#)
        .await;

    // Send ticket: XSSI-prefixed success.
    mp.mount_json(
        "POST",
        "/identity/auth/sendPhoneTicket",
        200,
        r#"&&&START&&&{"code":0}"#,
    )
    .await;
    mp.mount_json(
        "POST",
        "/identity/auth/sendEmailTicket",
        200,
        r#"&&&START&&&{"code":0}"#,
    )
    .await;
}

/// Mount the submit side: verify{Method} POST → landing chain (STS). The
/// landing start reuses /identity/result/check after the identity-chain
/// redirect on it is exhausted (mounted once).
async fn mount_submit_chain(mp: &MockPassport, method: &str, ok: bool) {
    let uri = mp.uri();
    let verify_body = if ok {
        format!(r#"{{"code":0,"location":"{uri}/identity/result/check"}}"#)
    } else {
        r#"{"code":70002,"description":"wrong code"}"#.to_string()
    };
    mp.mount_json(
        "POST",
        &format!("/identity/auth/verify{method}"),
        200,
        &verify_body,
    )
    .await;
    if ok {
        // The verify location is the landing chain start; mount it as a
        // fresh STS chain (a different token proves the walk happened).
        mp.mount_redirect("GET", "/identity/result/check", 302, &format!("{uri}/sts2"))
            .await;
        mp.mount_redirect_cookies(
            "GET",
            "/sts2",
            307,
            &format!("{uri}/open-apis/user/mi/get"),
            &[
                ("xiaomichatbot_serviceToken", "tok-otp"),
                ("xiaomichatbot_slh", "slh-otp"),
                ("xiaomichatbot_ph", "ph-otp"),
                ("cUserId", "cu-otp"),
                ("userId", "10001"),
                ("passToken", "pt-otp"),
            ],
        )
        .await;
    }
}

#[tokio::test]
async fn otp_001_code_zero_demand_auto_starts_challenge() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::OtpPending {
            challenge, notify, ..
        } => {
            assert!(challenge.id.starts_with("otp-"));
            assert_eq!(challenge.method.as_str(), "Phone");
            assert_eq!(notify, "188****5616");
            assert!(!challenge.expired());
        }
        other => panic!("expected OtpPending, got {other:?}"),
    }
    // identity/list → verifyPhone → sendPhoneTicket all hit (OTP-001).
    assert_eq!(mp.recorded_for("/identity/list").len(), 1);
    assert_eq!(mp.recorded_for("/identity/auth/verifyPhone").len(), 1);
    assert_eq!(mp.recorded_for("/identity/auth/sendPhoneTicket").len(), 1);
}

#[tokio::test]
async fn otp_002_flag8_email_method() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "8", "notifyEmail", "a***@x.com").await;
    let c = client_for(&mp);
    match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => {
            assert_eq!(challenge.method.as_str(), "Email");
        }
        other => panic!("expected OtpPending, got {other:?}"),
    }
    assert_eq!(mp.recorded_for("/identity/auth/verifyEmail").len(), 1);
    assert_eq!(mp.recorded_for("/identity/auth/sendEmailTicket").len(), 1);
}

#[tokio::test]
async fn otp_003_data_wrapped_flag_merged() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    let uri = mp.uri();
    mp.mount_redirect(
        "GET",
        "/identity/result/check",
        302,
        &format!("{uri}/fe/service/identity/authStart?sid=xiaomichatbot&context=ctx-otp"),
    )
    .await;
    mp.mount_json(
        "GET",
        "/fe/service/identity/authStart",
        200,
        "<html></html>",
    )
    .await;
    // flag arrives data-wrapped (OTP-003).
    mp.mount_json(
        "GET",
        "/identity/list",
        200,
        r#"{"data":{"flag":8,"notify":"d***@x.com"}}"#,
    )
    .await;
    mp.mount_json("GET", "/identity/auth/verifyEmail", 200, r#"{}"#)
        .await;
    mp.mount_json(
        "POST",
        "/identity/auth/sendEmailTicket",
        200,
        r#"&&&START&&&{"code":0}"#,
    )
    .await;
    let c = client_for(&mp);
    match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending {
            challenge, notify, ..
        } => {
            assert_eq!(challenge.method.as_str(), "Email");
            // notify from data.
            assert_eq!(notify, "d***@x.com");
        }
        other => panic!("expected OtpPending, got {other:?}"),
    }
}

#[tokio::test]
async fn otp_004_send_ticket_form_and_mandatory_xhr_header() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let c = client_for(&mp);
    let _ = c.login("188", "pw").await;
    let sends = mp.recorded_for("/identity/auth/sendPhoneTicket");
    assert_eq!(sends.len(), 1);
    let s = &sends[0];
    // Form: retry=0 icode= _json=true (OTP-004).
    assert!(s.body.contains("retry=0"));
    assert!(s.body.contains("icode="));
    assert!(s.body.contains("_json=true"));
    // X-Requested-With is MANDATORY (email send 66108s without it).
    assert_eq!(s.header("X-Requested-With"), "XMLHttpRequest");
}

#[tokio::test]
async fn otp_005_verify_submit_form_and_trust() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    mount_submit_chain(&mp, "Phone", true).await;
    let c = client_for(&mp);
    let challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    let sess = c.submit_otp(&challenge, "123456").await.unwrap();
    let verifies = mp.recorded_for("/identity/auth/verifyPhone");
    // The trigger GET + the submit POST both hit verifyPhone; the POST is
    // the one with a form body.
    let posts: Vec<_> = verifies
        .iter()
        .filter(|r| r.method == "POST" && !r.body.is_empty())
        .collect();
    assert_eq!(posts.len(), 1);
    let p = posts[0];
    // _flag, ticket=<code>, trust=true, _json=true + X-Requested-With.
    assert!(p.body.contains("_flag=4"));
    assert!(p.body.contains("ticket=123456"));
    assert!(p.body.contains("trust=true"));
    assert!(p.body.contains("_json=true"));
    assert_eq!(p.header("X-Requested-With"), "XMLHttpRequest");

    // OTP-007 (same chain): full Session — userId, passToken, STS cookie, ph.
    assert_eq!(sess.user_id, "10001");
    assert_eq!(sess.pass_token, "pt-otp");
    assert!(sess.cookie.contains("serviceToken=tok-otp"));
    assert!(sess.cookie.contains("xiaomichatbot_ph=\"ph-otp\""));
    assert_eq!(sess.ph, "ph-otp");
}

#[tokio::test]
async fn otp_006_wrong_code_error_names_passport_code() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    mount_submit_chain(&mp, "Phone", false).await;
    let c = client_for(&mp);
    let challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    let err = c.submit_otp(&challenge, "000000").await.unwrap_err();
    match err {
        LoginError::Protocol(m) => {
            assert!(m.contains("code=70002"), "{m}");
            assert!(m.contains("wrong code"), "{m}");
        }
        other => panic!("expected Protocol, got {other:?}"),
    }
}

#[tokio::test]
async fn otp_008_challenge_expiry_refused() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let c = client_for(&mp);
    let mut challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    // Force the challenge past its 10-minute window.
    challenge.expires_at = 0;
    let err = c.submit_otp(&challenge, "123456").await.unwrap_err();
    match err {
        LoginError::Protocol(m) => assert!(m.contains("expired"), "{m}"),
        other => panic!("expected Protocol, got {other:?}"),
    }
}

#[tokio::test]
async fn otp_009_auto_disabled_zero_identity_calls() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    let uri = mp.uri();
    let host = url::Url::parse(&uri)
        .unwrap()
        .host_str()
        .unwrap_or_default()
        .to_string();
    let c = Client::new(Options {
        base: Some(uri),
        passport_hosts: vec![host],
        disable_otp_auto: true,
        ..Options::default()
    })
    .unwrap();
    match c.login("188", "pw").await.unwrap_err() {
        LoginError::AccountAction { reason, .. } => {
            assert!(reason.contains("new-device"), "{reason}");
        }
        other => panic!("expected AccountAction, got {other:?}"),
    }
    // Zero identity calls.
    assert!(mp.recorded_for("/identity").is_empty());
}

#[tokio::test]
async fn otp_010_notificationurl_host_guard() {
    let mp = MockPassport::start().await;
    // Auth2 with a notificationUrl pointing at a hostile host.
    mp.mount_json(
        "GET",
        "/open-apis/user/mi/get",
        401,
        &format!(r#"{{"loginUrl":"{}"}}"#, login_url(&mp.uri())),
    )
    .await;
    mp.mount_json("GET", "/pass/serviceLogin", 200, "<html>plain</html>")
        .await;
    mp.mount_json(
        "POST",
        "/pass/serviceLoginAuth2",
        200,
        r#"&&&START&&&{"code":0,"securityStatus":128,"location":"","notificationUrl":"https://evil.example/identity"}"#,
    )
    .await;
    let c = client_for(&mp);
    let err = c.login("188", "pw").await.unwrap_err();
    match err {
        LoginError::AccountAction { cause, .. } => {
            // Auto-start failed with the allowlist mention wrapped inside.
            let cause = cause.map(|b| b.to_string()).unwrap_or_default();
            assert!(cause.contains("not allowlisted"), "{cause}");
        }
        other => panic!("expected AccountAction, got {other:?}"),
    }
    assert!(mp.recorded_for("/identity").is_empty());
}

#[tokio::test]
async fn otp_011_empty_code_rejected_without_passport() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let c = client_for(&mp);
    let challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    let before = mp.recorded().len();
    let err = c.submit_otp(&challenge, "   ").await.unwrap_err();
    assert!(err.to_string().contains("empty"));
    // No new passport traffic.
    assert_eq!(mp.recorded().len(), before);
}

#[tokio::test]
async fn otp_chain_001_landing_chain_walked_no_service_login_fallback() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    mount_submit_chain(&mp, "Phone", true).await;
    let c = client_for(&mp);
    let challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    let pre_submit = mp.recorded().len();
    let sess = c.submit_otp(&challenge, "123456").await.unwrap();

    // Zero /pass/serviceLogin calls after the challenge (no fallback) — the
    // landing chain alone planted everything (OTP-CHAIN-001). The login's
    // own prime GET before this point is expected.
    let sls: Vec<_> = mp
        .recorded()
        .into_iter()
        .skip(pre_submit)
        .filter(|r| r.path == "/pass/serviceLogin")
        .collect();
    assert!(
        sls.is_empty(),
        "serviceLogin fallback hit: {:?}",
        sls.iter().map(|r| r.path.clone()).collect::<Vec<_>>()
    );

    // Cookie carries cUserId, userId, both serviceToken spellings, quoted
    // slh/ph; ph unquoted in Session.ph.
    assert!(sess.cookie.contains("cUserId=cu-otp"));
    assert!(sess.cookie.contains("userId=10001"));
    assert!(sess.cookie.contains("serviceToken=tok-otp"));
    assert!(sess
        .cookie
        .contains("xiaomichatbot_serviceToken=\"tok-otp\""));
    assert!(sess.cookie.contains("xiaomichatbot_slh=\"slh-otp\""));
    assert!(sess.cookie.contains("xiaomichatbot_ph=\"ph-otp\""));
    assert_eq!(sess.ph, "ph-otp");
}

#[tokio::test]
async fn dev_004_verification_chain_stays_on_one_device() {
    let mp = MockPassport::start().await;
    // The prime page mints a deviceId that differs from the stored one —
    // Set-Cookie wins over the replayed value (browser semantics).
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let mut device = Device::new();
    device.insert("deviceId".into(), "wb_stored-9".into());
    let c = client_for(&mp).with_device(&device);
    let challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    // /identity/list presents a deviceId and the challenge carries the same
    // one (the page's Set-Cookie value, not the stored).
    let lists = mp.recorded_for("/identity/list");
    assert_eq!(lists.len(), 1);
    assert!(lists[0].cookie.contains("deviceId=wb_mock-2"));
    assert_eq!(
        challenge.jar.get("deviceId").map(String::as_str),
        Some("wb_mock-2")
    );
}

#[tokio::test]
async fn dev_009_stored_device_sticky_when_page_mints_only_when_absent() {
    let mp = MockPassport::start().await;
    // A page that mints deviceId only when none is presented: if a cookie
    // arrives, it answers Set-Cookie-free.
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
    mp.mount_json(
        "POST",
        "/pass/serviceLoginAuth2",
        200,
        r#"&&&START&&&{"code":70003,"description":"x"}"#,
    )
    .await;
    let mut device = Device::new();
    device.insert("deviceId".into(), "wb_stored-1".into());
    device.insert("uLocale".into(), "zh_CN".into());
    let c = client_for(&mp).with_device(&device);
    let _ = c.login("188", "pw").await;
    // The stored deviceId is presented on Auth2; the client fabricates no
    // identity of its own.
    let posts = auth2_posts(&mp);
    assert_eq!(posts.len(), 1);
    assert!(posts[0].cookie.contains("deviceId=wb_stored-1"));
    assert!(posts[0].cookie.contains("uLocale=zh_CN"));
    // And the prime navigation presented it too (sticky across hops).
    let primes = mp.recorded_for("/pass/serviceLogin");
    assert!(primes[0].cookie.contains("deviceId=wb_stored-1"));
}

#[tokio::test]
async fn dev_011_identity_hops_send_spa_xhr_set() {
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let c = client_for(&mp);
    let _ = c.login("188", "pw").await;
    // Every /identity/* call carries X-Requested-With, sec-fetch-mode: cors,
    // sec-ch-ua, JSON Accept (DEV-011).
    for r in mp.recorded_for("/identity") {
        assert_eq!(
            r.header("X-Requested-With"),
            "XMLHttpRequest",
            "{:?}",
            r.path
        );
        assert_eq!(r.header("sec-fetch-mode"), "cors", "{:?}", r.path);
        assert!(!r.header("sec-ch-ua").is_empty(), "{:?}", r.path);
        assert_eq!(r.header("accept"), "application/json, text/plain, */*");
    }
    // And the aistudio surface does NOT carry X-Requested-With (the two
    // surfaces never merge).
    for r in mp.recorded_for("/open-apis") {
        assert!(r.header("X-Requested-With").is_empty(), "{:?}", r.path);
        assert_eq!(r.header("accept"), "*/*");
    }
}

#[tokio::test]
async fn otp_error_fallback_service_login_completion() {
    // A passport variant whose landing location is a bare intermediate:
    // stsExchange fails (no serviceToken at the first chain), and the
    // explicit serviceLogin hop completes the login.
    let mp = MockPassport::start().await;
    mount_otp_demand_auth2(&mp).await;
    mount_identity_chain(&mp, "4", "notify", "188****5616").await;
    let uri = mp.uri();
    // Submit answers a location whose chain plants NO serviceToken (bare
    // intermediate) — the fallback path.
    mp.mount_json(
        "POST",
        "/identity/auth/verifyPhone",
        200,
        &format!(r#"{{"code":0,"location":"{uri}/bare"}}"#),
    )
    .await;
    mp.mount_json("GET", "/bare", 200, "<html>bare</html>")
        .await;
    // The serviceLogin hops: the first carries tokens in the body, the
    // second offers the STS location. Session-flow GETs carry _json=true
    // (distinct from the queryless prime navigation).
    mp.mount_json_qp(
        "GET",
        "/pass/serviceLogin",
        "_json",
        "true",
        200,
        &format!(
            r#"&&&START&&&{{"code":0,"userId":10001,"passToken":"pt-fb","location":"{uri}/sts2"}}"#
        ),
    )
    .await;
    mp.mount_redirect_cookies(
        "GET",
        "/sts2",
        307,
        &format!("{uri}/open-apis/user/mi/get"),
        &[
            ("xiaomichatbot_serviceToken", "tok-fb"),
            ("xiaomichatbot_ph", "ph-fb"),
            ("userId", "10001"),
        ],
    )
    .await;

    let c = client_for(&mp);
    let challenge = match c.login("188", "pw").await.unwrap_err() {
        LoginError::OtpPending { challenge, .. } => challenge,
        other => panic!("expected OtpPending, got {other:?}"),
    };
    let sess = c.submit_otp(&challenge, "123456").await.unwrap();
    // The fallback serviceLogin was actually used.
    assert!(mp
        .recorded_for("/pass/serviceLogin")
        .iter()
        .any(|r| r.query.contains("sid=xiaomichatbot")));
    assert!(sess.cookie.contains("serviceToken=tok-fb"));
}

/// Sanity: the happy login mounts still work from this file's context
/// (shared harness intact).
#[tokio::test]
async fn harness_happy_login_usable() {
    let mp = MockPassport::start().await;
    mount_happy_login(&mp).await;
    let c = client_for(&mp);
    let sess = c.login("188", "pw").await.unwrap();
    assert!(sess.cookie.contains("serviceToken=tok-1"));
    let _ = mount_sts_chain;
}
