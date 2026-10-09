package mimo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/testutil"
)

// renewEnv boots the probe server (401 + loginUrl pointing at the
// MockPassport) and the MockPassport. The probe is what the client's
// renewal step 1 hits (SDD §2.3).
func renewEnv(t *testing.T) (*Client, *testutil.MockPassport) {
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

// RENEW-001: renewal probe — the 401 body's loginUrl is extracted for
// renewal, and the probe request carries the auth cookie (SDD §2.3).
func TestRenewalProbe(t *testing.T) {
	client, passport := renewEnv(t)
	sess, err := client.Renew(context.Background(), "123456", "rt-old")
	if err != nil {
		t.Fatal(err)
	}
	// The serviceLogin call carried the auth cookie (SDD §2.3 step 2).
	if len(passport.ServiceLoginCalls) != 1 {
		t.Fatalf("serviceLogin calls = %d, want 1", len(passport.ServiceLoginCalls))
	}
	cookie := passport.ServiceLoginCalls[0]
	if !strings.Contains(cookie, "userId=123456") || !strings.Contains(cookie, "passToken=rt-old") {
		t.Errorf("renewal cookie = %q, want userId + passToken", cookie)
	}
	if sess.Cookie == "" || sess.PH == "" {
		t.Errorf("session incomplete: %+v", sess)
	}
}

// RENEW-002: STS from passToken — location followed, fresh cookies cached
// (SDD §2.3).
//
// passToken precedence (changed 2026-10-09, same rule the OTP/login path has
// always used): the credential the landing chain plants wins, because a rotated
// token is strictly fresher than the one we presented; a chain that plants none
// leaves the stored value untouched (DEV-006b). What must never happen is a
// renewal that blanks or loses the credential.
func TestRenewSTSFromPassToken(t *testing.T) {
	client, passport := renewEnv(t)
	sess, err := client.Renew(context.Background(), "123456", "rt-old")
	if err != nil {
		t.Fatal(err)
	}
	if len(passport.StsCalls) != 1 {
		t.Fatalf("sts follows = %d, want 1", len(passport.StsCalls))
	}
	if !strings.Contains(sess.Cookie, "st-mock-1") {
		t.Errorf("fresh serviceToken not in cookie: %q", sess.Cookie)
	}
	if sess.PassToken != "rt-mock-1" {
		t.Errorf("passToken = %q, want the chain-planted rt-mock-1 (rotation wins over the presented rt-old)", sess.PassToken)
	}
}

// RENEW-003: passToken dead (code 70016) → ErrPassTokenDead, no infinite
// retry (SDD §2.3).
func TestRenewPassTokenDead(t *testing.T) {
	client, passport := renewEnv(t)
	passport.SetServiceLoginResponse(testutil.FixtureXSSISTS70016)
	_, err := client.Renew(context.Background(), "123456", "rt-dead")
	var dead *ErrPassTokenDead
	if !errors.As(err, &dead) {
		t.Fatalf("err = %v, want ErrPassTokenDead", err)
	}
	if len(passport.ServiceLoginCalls) != 1 {
		t.Errorf("serviceLogin calls = %d, want 1 (no retry loop)", len(passport.ServiceLoginCalls))
	}
	if len(passport.StsCalls) != 0 {
		t.Errorf("sts follows = %d, want 0 after dead passToken", len(passport.StsCalls))
	}
}

// RENEW-007 (client half): the 6h sweep refreshes only invalid sessions —
// the sweep loop itself is wired in cmd (wave 7); the client contract is
// Validate(cookie) == nil ⇒ no renewal needed, auth-expired ⇒ renew.
func TestRenewSweepOnlyInvalid(t *testing.T) {
	up := testutil.NewMockUpstream()
	defer up.Close()
	up.AddAccount("cGgtbW9jaw==", `userId=123456; serviceToken=st-mock-1`)
	client := New(Options{Base: up.URL()})

	// Valid session: Validate passes, no renewal.
	if err := client.Validate(context.Background(), `userId=123456; serviceToken=st-mock-1`); err != nil {
		t.Fatalf("valid session failed Validate: %v", err)
	}
	// Unknown cookie: Validate fails with an auth error (needs renewal).
	err := client.Validate(context.Background(), `userId=999; serviceToken=st-bogus`)
	if !IsAuthExpiredErr(err) {
		t.Fatalf("invalid session: err = %v, want auth-expired", err)
	}
}

// RENEW-009: session validation business-code check — HTTP 200 with a
// non-zero business code is invalid (200 ≠ healthy).
func TestRenewValidationBusinessCode(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 but business code != 0.
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"code":40001,"description":"stale"}`)
	}))
	defer up.Close()
	client := New(Options{Base: up.URL})
	err := client.Validate(context.Background(), "userId=1; serviceToken=x")
	if !IsAuthExpiredErr(err) {
		t.Fatalf("err = %v, want auth-expired (business code non-zero)", err)
	}
}

// TestRenewProbeFallback: when the probe yields no loginUrl, the client
// constructs the official renewal URL (session.js:149-153 fallback). The
// constructed URL points at account.xiaomi.com — unreachable in tests —
// so the contract pinned here is: clean failure, not a panic, not a
// silent success.
func TestRenewProbeFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":401}`) // no loginUrl
	}))
	defer up.Close()
	client := New(Options{Base: up.URL, PassportHosts: []string{"account.xiaomi.com"}})
	if _, err := client.Renew(context.Background(), "123456", "rt"); err == nil {
		t.Fatal("unreachable constructed loginUrl must fail")
	}
}

// TestValidateTransportError: transport failure during Validate is a
// plain error, not auth-expired (probe sickness ≠ session death — the
// pool treats those differently, SDD §4.5).
func TestValidateTransportError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom") // hijack: transport error
	}))
	upURL := up.URL
	up.Close() // dead listener
	client := New(Options{Base: upURL})
	err := client.Validate(context.Background(), "userId=1; serviceToken=x")
	if err == nil {
		t.Fatal("dead server must error")
	}
	if IsAuthExpiredErr(err) {
		t.Errorf("transport error misclassified as auth-expired: %v", err)
	}
}
