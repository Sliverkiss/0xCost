package api

// Single-flight around credential resolution (BUGS-FOUND #2, sweep 2026-10).
//
// When an account has no cached session, every concurrent request holding a
// lease on it independently runs Login → new-device OTP → one challenge per
// request in the otp store. The renew-failure path already single-flights via
// RecoverCredentialAsync (ADR 0014); the login path had nothing. These tests
// pin the fix: one login per account per burst, losers reuse the winner's
// session or its pending challenge.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testutil"
)

// gateStack wires a Server whose only account has no cached session (login on
// demand). auth2 is the scripted serviceLoginAuth2 answer, so a Login either
// completes (FixtureXSSILoginOK) or auto-starts an OTP challenge
// (FixtureXSSILoginOTPZero). The mock passport counts the password logins
// (Auth2CallCount) and records the started challenges (/identity/list calls).
type gateStack struct {
	srv      *Server
	pool     *pool.Pool
	store    *accounts.Store
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
}

func newGateStack(t *testing.T, auth2 string) *gateStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(auth2, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)

	acct := accounts.Account{
		ID:       "acc-gate-1",
		Enabled:  true,
		Phone:    "13800000001",
		Password: "pw",
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
	p := pool.New([]accounts.Account{acct}, pool.Options{MaxInflight: 2}, nil, nil)
	client := mimo.New(mimo.Options{
		Base:          up.URL(),
		PassportHosts: []string{passport.Host()},
	})
	srv := New(Options{
		APIKeys:  []string{"sk-gate-test"},
		Pool:     p,
		Accounts: store,
		Client:   client,
	})
	return &gateStack{srv: srv, pool: p, store: store, passport: passport, upstream: up}
}

// chatPOST fires one chat request against the stack; the response body is
// drained but not asserted — the outcome is decided by the scripted fixture,
// and these tests assert what happened upstream, not the HTTP status.
func (s *gateStack) chatPOST(hs *httptest.Server) {
	body := `{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-gate-test")
	resp, err := hs.Client().Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// countIdentityLists counts started OTP challenges: each StartOTP performs
// exactly one /identity/list. Snapshot under the passport mutex (the recovery
// goroutine may still be appending).
func countIdentityLists(p *testutil.MockPassport) int {
	n := 0
	for _, c := range p.IdentityCallsSnapshot() {
		if c.Path == "/identity/list" {
			n++
		}
	}
	return n
}

// CREDS-001 (BUGS-FOUND #2): a burst of concurrent chats on a session-less
// account runs ONE login chain — one auth2 password post, at most one
// challenge registered. The pool admits two concurrent leases per account
// (MaxInflight=2 here), which is the exact window the bug lives in: requests
// that already hold leases when the first failure lands.
func TestCredsBurstRunsOneLogin(t *testing.T) {
	s := newGateStack(t, testutil.FixtureXSSILoginOTPZero)
	hs := httptest.NewServer(s.srv.Handler())
	t.Cleanup(hs.Close)

	before := s.passport.Auth2CallCount()

	var wg sync.WaitGroup
	const burst = 2
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.chatPOST(hs)
		}()
	}
	wg.Wait()

	logins := s.passport.Auth2CallCount() - before
	if logins != 1 {
		t.Fatalf("a burst of %d concurrent chats on a session-less account caused %d login chains; the per-account gate must collapse them into one", burst, logins)
	}
	challenges := countIdentityLists(s.passport)
	if challenges > 1 {
		t.Fatalf("challenges registered = %d, want at most 1 (one per login, and the losers must not start another)", challenges)
	}
}

// CREDS-002: the losers of the race see the winner's pending challenge —
// exactly one entry lands in the otp store, owned by the account, so the
// operator has one code to type, not one per concurrent request.
func TestCredsBurstLosersSeePendingChallenge(t *testing.T) {
	s := newGateStack(t, testutil.FixtureXSSILoginOTPZero)
	hs := httptest.NewServer(s.srv.Handler())
	t.Cleanup(hs.Close)

	var wg sync.WaitGroup
	const burst = 2
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.chatPOST(hs)
		}()
	}
	wg.Wait()

	pending := s.srv.otp.list()
	if len(pending) != 1 {
		t.Fatalf("otp store holds %d challenges, want exactly 1", len(pending))
	}
	if pending[0].AccountID != "acc-gate-1" {
		t.Errorf("challenge account = %q, want acc-gate-1", pending[0].AccountID)
	}
}

// CREDS-003: when the login completes, the winner's persisted session is
// reused by the losers — one login chain per burst, the session persisted,
// and (via the upstream mock) every request served on it.
func TestCredsBurstSharesWinnerSession(t *testing.T) {
	s := newGateStack(t, testutil.FixtureXSSILoginOK)
	hs := httptest.NewServer(s.srv.Handler())
	t.Cleanup(hs.Close)

	before := s.passport.Auth2CallCount()

	var wg sync.WaitGroup
	const burst = 2
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.chatPOST(hs)
		}()
	}
	wg.Wait()

	logins := s.passport.Auth2CallCount() - before
	if logins != 1 {
		t.Fatalf("concurrent chats on a session-less account caused %d login chains, want 1", logins)
	}
	acct, ok := s.store.Get("acc-gate-1")
	if !ok || acct.Session == nil || acct.Session.Cookie == "" {
		t.Fatalf("the winner's session was not persisted: %+v", acct)
	}
}

// CREDS-004: the foreground login and a background recovery (ADR 0014) share
// the same per-account gate — the recovery goroutine (which holds no lease)
// must not run a second login while a foreground creds is in flight.
func TestCredsSerializesWithRecovery(t *testing.T) {
	s := newGateStack(t, testutil.FixtureXSSILoginOTPZero)

	before := s.passport.Auth2CallCount()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, _ = s.srv.creds(context.Background(), "acc-gate-1")
	}()
	go func() {
		defer wg.Done()
		_ = s.srv.recoverCredential(context.Background(), "acc-gate-1")
	}()
	wg.Wait()

	logins := s.passport.Auth2CallCount() - before
	if logins != 1 {
		t.Fatalf("foreground creds + background recovery caused %d login chains, want 1 (one shared gate per account)", logins)
	}
}
