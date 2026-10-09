package mimo

// Coverage-gap tests for internal/mimo (TDD §6.2 floor: 80%). These pin the
// exported diagnostics wrappers, the resolution helpers the redirect chain
// depends on, and the error types' contract (message + errors.Is/As), rather
// than padding statement counts.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestErrorTypeContracts: every typed error renders its documented message
// and keeps the wrapping contract that callers rely on.
func TestErrorTypeContracts(t *testing.T) {
	pending := &ErrOTPPending{Challenge: &OTPChallenge{Notify: "+86 182******16"}}
	if got := pending.Error(); !strings.Contains(got, "+86 182******16") {
		t.Errorf("ErrOTPPending with notify = %q, want the masked destination", got)
	}
	if got := (&ErrOTPPending{}).Error(); got != "new-device OTP verification required" {
		t.Errorf("ErrOTPPending without notify = %q", got)
	}
	if got := (&ErrContentFilter{Reason: "sensitive_query"}).Error(); !strings.Contains(got, "sensitive_query") {
		t.Errorf("ErrContentFilter = %q", got)
	}
	if got := (&ErrUpstreamStream{Message: "boom"}).Error(); !strings.Contains(got, "boom") {
		t.Errorf("ErrUpstreamStream = %q", got)
	}
	if got := (&UpstreamError{StatusCode: 429, Body: "slow down"}).Error(); got != "upstream 429: slow down" {
		t.Errorf("UpstreamError = %q", got)
	}
	if got := (&ErrPassTokenDead{Description: "登录验证失败"}).Error(); !strings.Contains(got, "70016") {
		t.Errorf("ErrPassTokenDead = %q, want it to name code 70016", got)
	}
	// AuthError keeps the cause reachable and advertises auth-expiry.
	inner := errors.New("session gone")
	wrapped := AuthError(inner)
	if got := wrapped.Error(); got != "auth expired" {
		t.Errorf("authError = %q, want the stable short form", got)
	}
	if !errors.Is(wrapped, inner) {
		t.Error("AuthError dropped its cause — errors.Is(inner) must hold")
	}
	if !IsAuthExpiredErr(wrapped) {
		t.Error("IsAuthExpiredErr(AuthError(x)) = false")
	}
	if IsAuthExpiredErr(inner) {
		t.Error("IsAuthExpiredErr(plain error) = true")
	}
}

// TestAbsURLTable: Location resolution for the hand-walked chain.
func TestAbsURLTable(t *testing.T) {
	cases := []struct {
		base, ref, want string
	}{
		{"https://account.xiaomi.com/pass2/redirect?a=1", "https://aistudio.xiaomimimo.com/sts?sign=x", "https://aistudio.xiaomimimo.com/sts?sign=x"},
		{"https://account.xiaomi.com/pass2/redirect", "/pass2/userinfo/check?callback=abc", "https://account.xiaomi.com/pass2/userinfo/check?callback=abc"},
		{"https://account.xiaomi.com/a/b/c", "d", "https://account.xiaomi.com/a/b/d"},
		{"https://account.xiaomi.com/a", "//other.xiaomi.com/x", "https://other.xiaomi.com/x"},
		{"https://account.xiaomi.com/a", "?q=1", "https://account.xiaomi.com/a?q=1"},
		{"https://account.xiaomi.com/a", "", ""},
	}
	for _, c := range cases {
		if got := absURL(c.base, c.ref); got != c.want {
			t.Errorf("absURL(%q, %q) = %q, want %q", c.base, c.ref, got, c.want)
		}
	}
	// A malformed base must not panic; the raw reference is the fallback.
	if got := absURL("://bad", "ref"); got != "ref" {
		t.Errorf("absURL with a bad base = %q, want the raw ref", got)
	}
}

// TestAbsPassportURLTable: relative passport URLs anchor to the first
// allowlisted host (the mock in tests, account.xiaomi.com in production).
func TestAbsPassportURLTable(t *testing.T) {
	c := New(Options{Base: "https://aistudio.xiaomimimo.com", PassportHosts: []string{"passport.test"}})
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"https://account.xiaomi.com/pass/sts?sign=1", "https://account.xiaomi.com/pass/sts?sign=1"},
		{"/pass/serviceLogin?sid=x", "https://passport.test/pass/serviceLogin?sid=x"},
		{"pass/serviceLogin?sid=x", "https://passport.test/pass/serviceLogin?sid=x"},
	}
	for _, tc := range cases {
		if got := c.absPassportURL(tc.in); got != tc.want {
			t.Errorf("absPassportURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Without an allowlist the production default applies.
	def := New(Options{Base: "https://aistudio.xiaomimimo.com"})
	if got := def.absPassportURL("/pass/x"); got != "https://account.xiaomi.com/pass/x" {
		t.Errorf("default absPassportURL = %q, want account.xiaomi.com", got)
	}
}

// TestFindTokensTree: the tree scan must find tokens anywhere in a nested
// body (string, number, case-insensitive keys) and never invent one.
func TestFindTokensTree(t *testing.T) {
	body := []byte(`{"a":{"b":[{"PassToken":"V1:tok","UserId":1000000001}]}}`)
	pt, uid := findTokens(body)
	if pt != "V1:tok" || uid != "1000000001" {
		t.Errorf("findTokens = (%q,%q), want (V1:tok,1000000001) — a numeric userId must be rendered", pt, uid)
	}
	// XSSI prefix is stripped by parseLooseJSON.
	if pt, uid := findTokens([]byte("&&&START&&&\n{\"passToken\":\"x\",\"userId\":\"9\"}")); pt != "x" || uid != "9" {
		t.Errorf("findTokens with XSSI prefix = (%q,%q)", pt, uid)
	}
	// Non-scalar values are ignored; invalid JSON yields nothing.
	if pt, uid := findTokens([]byte(`{"passToken":{"nested":"no"},"userId":true}`)); pt != "" || uid != "" {
		t.Errorf("findTokens must ignore non-scalar values, got (%q,%q)", pt, uid)
	}
	if pt, uid := findTokens([]byte("not json")); pt != "" || uid != "" {
		t.Errorf("findTokens on garbage = (%q,%q), want empty", pt, uid)
	}
}

// TestCodeCheckTable: the reference rule — code absent/-1/0 pass, anything
// else fails and names the code plus whichever description field exists.
func TestCodeCheckTable(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want string
	}{
		{"nil map", nil, ""},
		{"no code", map[string]any{"desc": "x"}, ""},
		{"zero", map[string]any{"code": float64(0)}, ""},
		{"minus one", map[string]any{"code": float64(-1)}, ""},
		{"desc", map[string]any{"code": float64(21301), "desc": "认证失败"}, "code=21301 认证失败"},
		{"description", map[string]any{"code": float64(70002), "description": "验证码错误"}, "code=70002 验证码错误"},
		{"no description", map[string]any{"code": float64(20024)}, "code=20024"},
	}
	for _, c := range cases {
		err := codeCheck(c.m, "probe")
		switch {
		case c.want == "":
			if err != nil {
				t.Errorf("%s: codeCheck = %v, want nil", c.name, err)
			}
		default:
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: codeCheck = %v, want it to contain %q", c.name, err, c.want)
			}
		}
	}
}

// TestFetchLoginContextParsesProbe: the exported diagnostic wrapper must
// return the probe's loginUrl parameters (SDD §2.2 step 1).
func TestFetchLoginContextParsesProbe(t *testing.T) {
	client, passport := otpEnv(t)
	lc, err := client.FetchLoginContext(context.Background())
	if err != nil {
		t.Fatalf("FetchLoginContext: %v", err)
	}
	if lc.SID != "xiaomichatbot" {
		t.Errorf("SID = %q, want xiaomichatbot", lc.SID)
	}
	if lc.Callback == "" || !strings.Contains(lc.Callback, "sts") {
		t.Errorf("Callback = %q, want the STS callback", lc.Callback)
	}
	if lc.LoginURL == "" {
		t.Error("LoginURL empty — the probe's loginUrl was not captured")
	}
	_ = passport
}

// TestHTTPDoFollowsRedirectsAndManualDoesNot: the manual variant is the
// primitive the consent/landing chain walks depend on — it must surface the
// intermediate 3xx and its Location/Set-Cookie instead of following it.
func TestHTTPDoFollowsRedirectsAndManualDoesNot(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "landed", Value: "1", Path: "/"})
		_, _ = w.Write([]byte("final"))
	}))
	defer final.Close()
	mid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "hop", Value: "1", Path: "/"})
		http.Redirect(w, r, final.URL+"/done", http.StatusFound)
	}))
	defer mid.Close()

	c := New(Options{Base: final.URL, PassportHosts: []string{"127.0.0.1"}})
	ctx := context.Background()

	// Manual: stops at the 302 and exposes Location.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, mid.URL+"/start", nil)
	resp, err := c.HTTPDoManual(req)
	if err != nil {
		t.Fatalf("HTTPDoManual: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("HTTPDoManual status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/done") {
		t.Errorf("HTTPDoManual Location = %q, want the target", loc)
	}
	if resp.Header.Get("Set-Cookie") == "" {
		t.Error("HTTPDoManual lost the intermediate Set-Cookie")
	}

	// Following: lands on the final body.
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, mid.URL+"/start", nil)
	resp2, err := c.HTTPDo(req2)
	if err != nil {
		t.Fatalf("HTTPDo: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("HTTPDo status = %d, want 200 after following", resp2.StatusCode)
	}
}
