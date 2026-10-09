package mimo

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testutil"
)

// loginEnv boots a probe server whose /open-apis/user/mi/get 401 body
// carries the MockPassport's loginUrl, plus the MockPassport itself.
// Returns a Client pointed at the pair.
func loginEnv(t *testing.T) (*Client, *testutil.MockPassport) {
	t.Helper()
	passport := testutil.NewMockPassport("http://upstream.invalid")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/user/mi/get" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"code":401,"loginUrl":%q}`, passport.LoginURLFor())
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() {
		up.Close()
		passport.Close()
	})
	client := New(Options{
		Base:          up.URL,
		PassportHosts: []string{passport.Host()},
	})
	return client, passport
}

// LOGIN-001: login context probe — loginUrl parameters parsed (SDD §2.2).
func TestLoginContextProbe(t *testing.T) {
	client, passport := loginEnv(t)
	lctx, err := client.fetchLoginContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lctx.SID != "xiaomichatbot" {
		t.Errorf("sid = %q, want xiaomichatbot", lctx.SID)
	}
	if lctx.Sign != "sign123" {
		t.Errorf("_sign = %q", lctx.Sign)
	}
	if lctx.Callback == "" || lctx.ServiceParam == "" || lctx.Group == "" {
		t.Errorf("callback/serviceParam/_group not parsed: %+v", lctx)
	}
	_ = passport
}

// LOGIN-002: password auth — form fields, hash =
// MD5(password).hex().toUpperCase(), code:0 + location accepted (SDD §2.2).
func TestPasswordAuth(t *testing.T) {
	client, passport := loginEnv(t)
	sess, err := client.Login(context.Background(), "13800000001", "secret-pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(passport.Auth2Calls) != 1 {
		t.Fatalf("auth2 calls = %d, want 1", len(passport.Auth2Calls))
	}
	form := passport.Auth2Calls[0]
	sum := md5.Sum([]byte("secret-pw"))
	wantHash := strings.ToUpper(fmt.Sprintf("%x", sum))
	if form["hash"] != wantHash {
		t.Errorf("hash = %q, want %q", form["hash"], wantHash)
	}
	if form["user"] != "13800000001" {
		t.Errorf("user = %q", form["user"])
	}
	if form["_json"] != "true" || form["sid"] == "" || form["callback"] == "" || form["_sign"] == "" {
		t.Errorf("required form fields missing: %v", form)
	}
	if sess.UserID != "123456" {
		t.Errorf("userID = %q, want 123456", sess.UserID)
	}
	if sess.PassToken != "rt-mock-1" {
		t.Errorf("passToken = %q, want rt-mock-1", sess.PassToken)
	}
}

// LOGIN-003: XSSI prefix parsing — prefix stripped, JSON parsed; garbage
// before/after rejected.
func TestXSSIParsing(t *testing.T) {
	m, err := parseXSSI([]byte(testutil.FixtureXSSILoginOK))
	if err != nil {
		t.Fatal(err)
	}
	if m["code"].(float64) != 0 {
		t.Errorf("code = %v", m["code"])
	}
	if _, err := parseXSSI([]byte(`{"code":0}`)); err == nil {
		t.Error("body without prefix must be rejected")
	}
	if _, err := parseXSSI([]byte("garbage&&&START&&&{\"code\":0}")); err == nil {
		t.Error("garbage before prefix must be rejected")
	}
	if _, err := parseXSSI([]byte("&&&START&&&not json")); err == nil {
		t.Error("non-JSON after prefix must be rejected")
	}
	// Leading whitespace before the prefix is tolerated.
	if _, err := parseXSSI([]byte("  &&&START&&&{\"code\":0}")); err != nil {
		t.Errorf("whitespace before prefix must be tolerated: %v", err)
	}
}

// LOGIN-004: captcha demand → ErrAccountAction mentioning captcha; no
// retry loop.
func TestLoginCaptcha(t *testing.T) {
	client, passport := loginEnv(t)
	passport.SetAuth2Response(testutil.FixtureXSSILoginCaptcha, 0)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var ea *ErrAccountAction
	if !errors.As(err, &ea) {
		t.Fatalf("err = %v, want ErrAccountAction", err)
	}
	if !strings.Contains(ea.Reason, "captcha") {
		t.Errorf("reason = %q, want captcha mention", ea.Reason)
	}
	if len(passport.Auth2Calls) != 1 {
		t.Errorf("auth2 calls = %d, want 1 (no auto-solve loop)", len(passport.Auth2Calls))
	}
}

// LOGIN-005: OTP/new-device demand → ErrAccountAction mentioning device
// verification.
func TestLoginOTP(t *testing.T) {
	client, passport := loginEnv(t)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTP, 0)
	_, err := client.Login(context.Background(), "13800000001", "pw")
	var ea *ErrAccountAction
	if !errors.As(err, &ea) {
		t.Fatalf("err = %v, want ErrAccountAction", err)
	}
	if !strings.Contains(ea.Reason, "device") {
		t.Errorf("reason = %q, want device-verification mention", ea.Reason)
	}
}

// LOGIN-006: STS exchange — xiaomichatbot_serviceToken, ph, userId
// captured; cookie string assembled per SDD §2.1.
func TestSTSExchange(t *testing.T) {
	passport := testutil.NewMockPassport("http://upstream.invalid")
	defer passport.Close()
	client := New(Options{Base: "http://upstream.invalid", PassportHosts: []string{passport.Host()}})
	sess, err := client.stsExchange(context.Background(), passport.URL()+"/sts?sign=t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sess.Cookie, `xiaomichatbot_serviceToken="st-mock-1"`) {
		t.Errorf("cookie missing quoted serviceToken: %q", sess.Cookie)
	}
	if !strings.Contains(sess.Cookie, `xiaomichatbot_ph="cGgtbW9jaw=="`) {
		t.Errorf("cookie missing quoted ph: %q", sess.Cookie)
	}
	if !strings.Contains(sess.Cookie, "userId=123456") {
		t.Errorf("cookie missing userId: %q", sess.Cookie)
	}
	if !strings.Contains(sess.Cookie, "serviceToken=st-mock-1") {
		t.Errorf("cookie missing plain serviceToken spelling: %q", sess.Cookie)
	}
	if sess.PH != "cGgtbW9jaw==" {
		t.Errorf("ph = %q", sess.PH)
	}
}

// LOGIN-007: the returned Session carries everything the accounts store
// persists (userId, passToken, cookie, ph) (SDD §2.2, §4.7).
func TestLoginSessionFields(t *testing.T) {
	client, _ := loginEnv(t)
	sess, err := client.Login(context.Background(), "13800000001", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID == "" || sess.PassToken == "" || sess.Cookie == "" || sess.PH == "" {
		t.Errorf("session incomplete: %+v", sess)
	}
}

// LOGIN-008: location host allowlist — a location pointing at an unknown
// host is refused (open-redirect guard).
func TestLocationHostAllowlist(t *testing.T) {
	client := New(Options{})
	_, err := client.stsExchange(context.Background(), "https://evil.example/sts?sign=x")
	if err == nil {
		t.Fatal("evil host must be refused")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("err = %v, want allowlist mention", err)
	}
	if _, err := client.parseLoginURL("https://evil.example/pass/serviceLogin?sid=xiaomichatbot"); err == nil {
		t.Fatal("evil loginUrl host must be refused")
	}
}

// LOGIN-009: non-zero code without location → login failure error, no
// partial state.
func TestLoginNonZeroCode(t *testing.T) {
	body := "&&&START&&&" + `{"code":70003,"description":"bad credentials"}` + "\n"
	client, passport := loginEnv(t)
	passport.SetAuth2Response(body, 0)
	sess, err := client.Login(context.Background(), "13800000001", "pw")
	if err == nil {
		t.Fatal("non-zero code must fail login")
	}
	if sess != nil {
		t.Errorf("no partial state on failure: %+v", sess)
	}
	if !strings.Contains(err.Error(), "70003") {
		t.Errorf("err = %v, want code mention", err)
	}
}
