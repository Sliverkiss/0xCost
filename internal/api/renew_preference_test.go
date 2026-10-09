package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/session"
	"github.com/yg/mimo2api/internal/testutil"
)

// renewStack is the ADOTP-001 wiring with an account that has a stored
// identity (user_id + pass_token) but NO cached session.
type renewStack struct {
	*otpAdminStack
}

// newRenewStack builds the stack; passToken/userID seed the account.
func newRenewStack(t *testing.T, userID, passToken string) *renewStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	// A renewal must never need the password path: script auth2 so that any
	// accidental Login() would be visible as an auth2 call.
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)

	acct := accounts.Account{
		ID:        "acc-renew-1",
		Enabled:   true,
		Phone:     "13800000001",
		Password:  "pw",
		UserID:    userID,
		PassToken: passToken,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	f, _ := json.MarshalIndent(accounts.File{Accounts: []accounts.Account{acct}}, "", "  ")
	if err := os.WriteFile(path, f, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New([]accounts.Account{acct}, pool.Options{}, nil, nil)
	client := mimo.New(mimo.Options{
		Base:          up.URL(),
		PassportHosts: []string{passport.Host()},
	})
	apiSrv := New(Options{
		APIKeys:  []string{"sk-admin-test"},
		Pool:     p,
		Accounts: store,
		Sessions: session.New(session.Options{}),
		Client:   client,
	})
	hs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(hs.Close)
	return &renewStack{&otpAdminStack{ts: hs, apiSrv: apiSrv, pool: p, store: store, passport: passport, upstream: up}}
}

// RENEW-PREF-001: with a stored passToken but no cached session, the gateway
// must re-mint the session by renewal — never by the password login, which
// re-runs the verification chain and costs a human an OTP (TDD LIVE-006).
func TestRenewBeforePasswordLogin(t *testing.T) {
	s := newRenewStack(t, "1000000001", "V1:rt-live-1")

	resp, body := s.do(http.MethodPost, "/v1/chat/completions", "sk-admin-test",
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}]}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chat after renewal = %v %q, want 200", resp, body)
	}
	if n := len(s.passport.Auth2Calls); n != 0 {
		t.Errorf("auth2 (password login) called %d times — renewal should have covered it", n)
	}
	if n := len(s.passport.ServiceLoginCalls); n == 0 {
		t.Error("no renewal serviceLogin call recorded")
	}
	acct, ok := s.store.Get("acc-renew-1")
	if !ok {
		t.Fatal("account vanished")
	}
	if acct.Session == nil || acct.Session.Cookie == "" {
		t.Fatal("renewed session was not persisted")
	}
	if !strings.Contains(acct.Session.Cookie, "serviceToken=") {
		t.Errorf("persisted cookie %q has no service token", acct.Session.Cookie)
	}
	if acct.Session.PH == "" {
		t.Error("persisted session has no ph")
	}
	// A renewal must never lose or blank the renewal identity. The passToken
	// itself may legitimately be rotated by the landing chain (device.go /
	// TDD DEV-006): what matters is that the account still holds one.
	if acct.UserID != "1000000001" {
		t.Errorf("userId changed during renewal: %q", acct.UserID)
	}
	if acct.PassToken == "" {
		t.Error("renewal blanked the stored passToken")
	}
	if acct.Session.FetchedAt == 0 || time.Since(time.Unix(acct.Session.FetchedAt, 0)) > time.Minute {
		t.Errorf("fetched_at = %d, want a fresh timestamp", acct.Session.FetchedAt)
	}
}

// RENEW-PREF-002: when the stored passToken is dead, renewal fails and the
// password login is attempted — the account ends up disabled with an
// operator-action reason rather than silently serving a stale session.
func TestRenewFallbackToPasswordLogin(t *testing.T) {
	s := newRenewStack(t, "1000000001", "V1:dead-token")
	// Make renewal fail: the passport answers serviceLogin with a dead-token
	// error instead of an STS location.
	// Exact passport shape (XSSI prefix required by parseXSSI) so the failure
	// is classified as a dead passToken rather than a parse error.
	s.passport.SetServiceLoginResponse(`&&&START&&&{"code":70016,"description":"登录验证失败"}`)

	resp, body := s.do(http.MethodPost, "/v1/chat/completions", "sk-admin-test",
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}]}`)
	if resp == nil || resp.StatusCode == http.StatusOK {
		t.Fatalf("chat with a dead passToken = %v %q, want a failure", resp, body)
	}
	if n := len(s.passport.Auth2Calls); n == 0 {
		t.Error("expected the password login fallback to run after the failed renewal")
	}
	if st := s.pool.StateOf("acc-renew-1"); st == pool.StateAvailable {
		t.Errorf("pool state = %s, want the account out of rotation", st)
	}
}
