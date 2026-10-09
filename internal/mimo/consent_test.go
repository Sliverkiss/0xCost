package mimo

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// UC-001: the live-verified cross-region chain — /pass2/redirect →
// userinfo/start (bizType=UserCross) → /fe/service/userCross → agreement POST
// → userinfo/check → identity/authStart. The flow must agree (status=1), then
// run the identity steps against the *post-consent* context, never the stale
// context of the original notificationUrl.
func TestConsentChainAgreesThenVerifies(t *testing.T) {
	client, passport := otpEnv(t)
	passport.SetConsentChain(true)

	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	if len(passport.ConsentCalls) != 1 {
		t.Fatalf("consent calls = %d, want 1", len(passport.ConsentCalls))
	}
	c := passport.ConsentCalls[0]
	if c["userId"] != "123456" || c["status"] != "1" || c["needClose"] != "false" {
		t.Errorf("consent form = %v, want userId/status=1/needClose=false", c)
	}
	if c["_xrw"] != "XMLHttpRequest" {
		t.Errorf("consent X-Requested-With = %q, want XMLHttpRequest", c["_xrw"])
	}
	if !strings.Contains(c["_referer"], "/fe/service/userCross") {
		t.Errorf("consent Referer = %q, want the userCross page", c["_referer"])
	}
	list := findIdentityCall(passport, "/identity/list", "")
	if list == nil {
		t.Fatal("identity/list not called after consent")
	}
	if !strings.Contains(list.Query, "context=ctx-uc-1") {
		t.Errorf("identity/list query = %q, want the post-consent context ctx-uc-1", list.Query)
	}
	if pending.Challenge == nil || pending.Challenge.SID != "xiaomichatbot" {
		t.Errorf("challenge = %+v, want sid xiaomichatbot", pending.Challenge)
	}
}

// UC-002: with auto-agree disabled the demand is surfaced, not accepted.
func TestConsentDisabledSurfacesError(t *testing.T) {
	client, passport := otpEnv(t)
	passport.SetConsentChain(true)
	client.disableCrossRegionAgree = true

	_, err := client.Login(context.Background(), "13800000001", "pw")
	if !errors.Is(err, ErrCrossRegionConsent) {
		t.Fatalf("err = %v, want ErrCrossRegionConsent (through the account-action wrapper)", err)
	}
	var action *ErrAccountAction
	if !errors.As(err, &action) || !strings.Contains(action.Reason, "cross-region") {
		t.Fatalf("err = %v, want an ErrAccountAction naming the cross-region demand", err)
	}
	if len(passport.ConsentCalls) != 0 {
		t.Errorf("consent POSTed %d times with auto-agree disabled", len(passport.ConsentCalls))
	}
	if findIdentityCall(passport, "/identity/list", "") != nil {
		t.Error("identity flow ran despite the consent demand being unresolved")
	}
}

// UC-003: agreements are one-shot — a consumed session answers 21301
// 认证失败. The error must be surfaced with the code, and the identity flow
// must not run.
func TestConsentRefusedSurfacesCode(t *testing.T) {
	client, passport := otpEnv(t)
	passport.SetConsentChain(true)
	passport.SetConsentBody("&&&START&&&" + `{"code":21301,"result":"error","desc":"认证失败","description":"认证失败","location":""}` + "\n")

	_, err := client.Login(context.Background(), "13800000001", "pw")
	if err == nil || !strings.Contains(err.Error(), "21301") {
		t.Fatalf("err = %v, want a 21301 refusal", err)
	}
	if findIdentityCall(passport, "/identity/list", "") != nil {
		t.Error("identity flow ran after a refused consent")
	}
}

// UC-004: code 0 without a location is a protocol drift, not a silent
// success.
func TestConsentWithoutLocation(t *testing.T) {
	client, passport := otpEnv(t)
	passport.SetConsentChain(true)
	passport.SetConsentBody("&&&START&&&" + `{"code":0,"result":"ok","desc":"成功"}` + "\n")

	_, err := client.Login(context.Background(), "13800000001", "pw")
	if err == nil || !strings.Contains(err.Error(), "no location") {
		t.Fatalf("err = %v, want a missing-location error", err)
	}
}

// UC-005: the chain's hosts stay allowlisted (open-redirect guard) — a
// notificationUrl pointing anywhere else is refused before any request.
func TestConsentChainHostNotAllowlisted(t *testing.T) {
	client, passport := otpEnv(t)
	passport.SetConsentChain(true)
	passport.SetAuth2Response("&&&START&&&"+
		`{"notificationUrl":"https://evil.example.com/pass2/redirect?sid=xiaomichatbot","code":0,"location":"","description":"ok"}`+"\n", 0)

	_, err := client.Login(context.Background(), "13800000001", "pw")
	if err == nil || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("err = %v, want an allowlist refusal", err)
	}
}

// UC-006: authStartParams follows the /pass2/userinfo/check?callback=
// indirection the consent flow uses.
func TestAuthStartParamsIndirection(t *testing.T) {
	inner := "https://account.xiaomi.com/fe/service/identity/authStart?sid=xiaomichatbot&context=ctx-abc"
	outer := "https://account.xiaomi.com/pass2/userinfo/check?callback=" + url.QueryEscape(inner)
	sid, cx := authStartParams(outer)
	if sid != "xiaomichatbot" || cx != "ctx-abc" {
		t.Fatalf("authStartParams = (%q,%q), want (xiaomichatbot,ctx-abc)", sid, cx)
	}
	// A direct authStart URL is accepted as-is.
	if sid, cx := authStartParams(inner); sid != "xiaomichatbot" || cx != "ctx-abc" {
		t.Fatalf("direct authStart: (%q,%q)", sid, cx)
	}
	if _, cx := authStartParams("https://account.xiaomi.com/fe/service/userCross?userId=1"); cx != "" {
		t.Fatalf("consent page must not look like an authStart entry (context=%q)", cx)
	}
}

// UC-007: the consent chain is recognised by the bizType marker on an
// intermediate Location (the page URL itself carries only userId).
func TestIsConsentChain(t *testing.T) {
	hops := []hopRecord{
		{Status: 302, URL: "https://account.xiaomi.com/pass2/redirect?sid=passport",
			Location: "https://account.xiaomi.com/pass2/userinfo/start?bizType=UserCross&userId=2652042633"},
	}
	if uid, ok := isConsentChain(hops, "https://account.xiaomi.com/fe/service/userCross?needClose=false&userId=2652042633"); !ok || uid != "2652042633" {
		t.Fatalf("isConsentChain = (%q,%v), want (2652042633,true)", uid, ok)
	}
	if _, ok := isConsentChain(nil, "https://account.xiaomi.com/fe/service/identity/authStart?context=x"); ok {
		t.Fatal("plain identity entry misclassified as consent")
	}
}

// OTP-CHAIN-001: the post-verification landing chain must be walked to its
// end — the xiaomichatbot_* session cookies are planted at the STS hop, so a
// single-hop follow (the pre-fix behaviour) misses them. The composed cookie
// also carries the browser's full set (live capture 2026-10-09).
func TestOTPSessionFromLandingChain(t *testing.T) {
	client, passport := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	sess, err := client.SubmitOTP(context.Background(), pending.Challenge, "654321")
	if err != nil {
		t.Fatal(err)
	}
	if len(passport.ServiceLoginCalls) != 0 {
		t.Errorf("serviceLogin called %d times — the landing chain was not walked to the end", len(passport.ServiceLoginCalls))
	}
	for _, want := range []string{
		`cUserId=cu-mock-1`,
		`userId=123456`,
		`serviceToken=st-mock-1`,
		`xiaomichatbot_serviceToken="st-mock-1"`,
		`xiaomichatbot_slh="slh-mock-1"`,
		`xiaomichatbot_ph="cGgtbW9jaw=="`,
	} {
		if !strings.Contains(sess.Cookie, want) {
			t.Errorf("cookie %q missing %q", sess.Cookie, want)
		}
	}
	if sess.PH != "cGgtbW9jaw==" {
		t.Errorf("ph = %q, want the unquoted value", sess.PH)
	}
	if sess.PassToken != "rt-mock-1" || sess.UserID != "123456" {
		t.Errorf("session identity = (%q,%q), want (rt-mock-1,123456)", sess.PassToken, sess.UserID)
	}
}
