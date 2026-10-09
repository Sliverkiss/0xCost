package mimo

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/testutil"
)

// otpEnv: loginEnv + auth2 scripted with the live-captured code=0 OTP
// fixture (FixtureXSSILoginOTPZero). The mock rewrites the
// notificationUrl host to itself, so the full identity flow runs against
// the double.
func otpEnv(t *testing.T) (*Client, *testutil.MockPassport) {
	t.Helper()
	client, passport := loginEnv(t)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	return client, passport
}

// findIdentityCall returns the recorded /identity/* call at path (method
// optional — "" matches any). verify{Phone,Email} appears twice in the
// flow (step-3 GET trigger, step-5 POST submit): pass the method to pick.
func findIdentityCall(p *testutil.MockPassport, path, method string) *testutil.RecordedIdentityCall {
	var out *testutil.RecordedIdentityCall
	for i := range p.IdentityCalls {
		c := &p.IdentityCalls[i]
		if c.Path == path && (method == "" || c.Method == method) {
			out = c
		}
	}
	return out
}

// OTP-001 (Bug 1): code=0 + empty location + notificationUrl →
// ErrOTPPending (auto-started), NOT "no location for STS exchange".
func TestLoginOTPZeroCode(t *testing.T) {
	client, passport := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	ch := pending.Challenge
	if ch == nil || ch.ID == "" || ch.Method != "Phone" {
		t.Fatalf("challenge incomplete: %+v", ch)
	}
	if ch.Notify != "188****5616" {
		t.Errorf("notify = %q, want masked phone", ch.Notify)
	}
	// The identity flow ran: list → verifyPhone (GET trigger) → sendPhoneTicket.
	if findIdentityCall(passport, "/identity/list", "") == nil {
		t.Error("identity/list not called")
	}
	if findIdentityCall(passport, "/identity/auth/verifyPhone", http.MethodGet) == nil {
		t.Error("identity/auth/verifyPhone not called")
	}
	if findIdentityCall(passport, "/identity/auth/sendPhoneTicket", "") == nil {
		t.Error("identity/auth/sendPhoneTicket not called")
	}
}

// OTP-002: flag 8 in identity/list → Email method, verifyEmail/sendEmailTicket.
func TestOTPFlagEmail(t *testing.T) {
	client, passport := otpEnv(t)
	passport.IdentityFlag = 8
	passport.IdentityNotify = "a***@qq.com"
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	if pending.Challenge.Method != "Email" {
		t.Errorf("method = %q, want Email", pending.Challenge.Method)
	}
	if pending.Challenge.Notify != "a***@qq.com" {
		t.Errorf("notify = %q, want a***@qq.com", pending.Challenge.Notify)
	}
	if findIdentityCall(passport, "/identity/auth/verifyEmail", "") == nil {
		t.Error("identity/auth/verifyEmail not called")
	}
	if findIdentityCall(passport, "/identity/auth/sendEmailTicket", "") == nil {
		t.Error("identity/auth/sendEmailTicket not called")
	}
}

// OTP-003: flag nested in list.data (data-wrapped variant) — merged and
// parsed as 8.
func TestOTPFlagDataWrapped(t *testing.T) {
	client, passport := otpEnv(t)
	passport.IdentityListBody = `{"code":0,"data":{"flag":8,"notify":"b***@163.com"}}`
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	if pending.Challenge.Method != "Email" {
		t.Errorf("method = %q, want Email (data-wrapped flag)", pending.Challenge.Method)
	}
	if pending.Challenge.Notify != "b***@163.com" {
		t.Errorf("notify = %q, want b***@163.com", pending.Challenge.Notify)
	}
}

// OTP-004: send{Method}Ticket MUST carry X-Requested-With: XMLHttpRequest
// (without it the real email send always fails 66108, 2026-09-23 capture).
func TestOTPSendTicketHeader(t *testing.T) {
	client, passport := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	call := findIdentityCall(passport, "/identity/auth/sendPhoneTicket", "")
	if call == nil {
		t.Fatal("sendPhoneTicket not called")
	}
	if got := call.Headers.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q, want XMLHttpRequest", got)
	}
	if call.Method != "POST" {
		t.Errorf("method = %q, want POST", call.Method)
	}
	if call.Form["retry"] != "0" || call.Form["_json"] != "true" {
		t.Errorf("send form = %v, want retry=0 _json=true", call.Form)
	}
}

// OTP-005: ticket submit carries X-Requested-With too, plus the code as
// ticket and trust=true.
func TestOTPVerifySubmitForm(t *testing.T) {
	client, passport := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	if _, err := client.SubmitOTP(context.Background(), pending.Challenge, "123456"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	call := findIdentityCall(passport, "/identity/auth/verifyPhone", http.MethodPost)
	if call == nil {
		t.Fatal("verifyPhone (submit) not recorded")
	}
	if got := call.Headers.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q, want XMLHttpRequest", got)
	}
	if call.Form["ticket"] != "123456" || call.Form["trust"] != "true" || call.Form["_flag"] != "4" {
		t.Errorf("verify form = %v, want ticket=123456 trust=true _flag=4", call.Form)
	}
}

// OTP-006: wrong code → error naming the passport code, challenge stays
// usable (retry within the window).
func TestOTPWrongCode(t *testing.T) {
	client, passport := otpEnv(t)
	passport.OTPWrongCode = true
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	_, serr := client.SubmitOTP(context.Background(), pending.Challenge, "000000")
	if serr == nil {
		t.Fatal("wrong code must fail submit")
	}
	if !strings.Contains(serr.Error(), "70002") {
		t.Errorf("err = %v, want passport code mention", serr)
	}
	// Right code afterwards still completes (challenge retained).
	passport.OTPWrongCode = false
	if _, serr = client.SubmitOTP(context.Background(), pending.Challenge, "123456"); serr != nil {
		t.Errorf("retry with correct code failed: %v", serr)
	}
}

// OTP-007: successful full flow — Login auto-starts, SubmitOTP returns a
// complete Session (STS cookies + userId + passToken).
func TestOTPSuccess(t *testing.T) {
	client, _ := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	sess, err := client.SubmitOTP(context.Background(), pending.Challenge, "654321")
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID != "123456" {
		t.Errorf("userID = %q, want 123456", sess.UserID)
	}
	if sess.PassToken != "rt-mock-1" {
		t.Errorf("passToken = %q, want rt-mock-1", sess.PassToken)
	}
	if !strings.Contains(sess.Cookie, `xiaomichatbot_serviceToken="st-mock-1"`) {
		t.Errorf("cookie missing serviceToken: %q", sess.Cookie)
	}
	if sess.PH != "cGgtbW9jaw==" {
		t.Errorf("ph = %q", sess.PH)
	}
}

// OTP-008: expired challenge → SubmitOTP refuses.
func TestOTPExpiry(t *testing.T) {
	client, _ := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	pending.Challenge.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := client.SubmitOTP(context.Background(), pending.Challenge, "123456"); err == nil {
		t.Fatal("expired challenge must be refused")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v, want expired mention", err)
	}
}

// OTP-009: OTPAuto disabled → the code=0 OTP demand surfaces as
// ErrAccountAction (manual re-login), no identity calls.
func TestOTPAutoDisabled(t *testing.T) {
	client, passport := loginEnv(t)
	client.disableOTPAuto = true
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var ea *ErrAccountAction
	if !errors.As(err, &ea) {
		t.Fatalf("err = %v, want ErrAccountAction", err)
	}
	if !strings.Contains(ea.Reason, "OTP") {
		t.Errorf("reason = %q, want OTP mention", ea.Reason)
	}
	if len(passport.IdentityCalls) != 0 {
		t.Errorf("identity calls = %d, want 0 with auto disabled", len(passport.IdentityCalls))
	}
}

// OTP-010: notificationUrl host NOT allowlisted → StartOTP refuses
// (open-redirect guard).
func TestOTPNotificationHostGuard(t *testing.T) {
	client := New(Options{})
	if _, err := client.StartOTP(context.Background(), "https://evil.example/pass2/redirect?sid=x", "xiaomichatbot", nil); err == nil {
		t.Fatal("evil notificationUrl host must be refused")
	} else if !strings.Contains(err.Error(), "allowlisted") {
		t.Errorf("err = %v, want allowlist mention", err)
	}
}

// OTP-011: empty code rejected without hitting the passport.
func TestOTPEmptyCode(t *testing.T) {
	client, passport := otpEnv(t)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var pending *ErrOTPPending
	if !errors.As(err, &pending) {
		t.Fatalf("err = %v, want ErrOTPPending", err)
	}
	before := len(passport.IdentityCalls)
	if _, err := client.SubmitOTP(context.Background(), pending.Challenge, "   "); err == nil {
		t.Fatal("empty code must be refused")
	}
	if len(passport.IdentityCalls) != before {
		t.Errorf("identity calls grew: %d → %d", before, len(passport.IdentityCalls))
	}
}
