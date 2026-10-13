package api

// NORENEW-001..003 (owner ruling, 2026-10-14 — "request paths never renew or
// log in; a scheduled hourly job owns session freshness"):
//
// A chat/audio request resolves the stored session and nothing else. When the
// session is missing, or the upstream reports it dead (401), the request
// fails — the error passes through to the client, exactly like the 429
// pass-through ruling. No Renew, no password Login, no background recovery
// starts from a request. The hourly renew job is the only thing that may
// touch the passport.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/testutil"
)

// noRenewStack wires the full stack with a mock passport whose call counts
// are the assertion surface: any renewal or login attempt from the request
// path shows up as a ServiceLoginCalls/Auth2Calls count, which these tests
// assert stays 0.
type noRenewStack struct {
	srv      *Server
	pool     *pool.Pool
	store    *accounts.Store
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
	ts       *httptest.Server
}

// newNoRenewStack builds the stack. The account starts with the given session
// cookie ("" = no stored session).
func newNoRenewStack(t *testing.T, sessionCookie, passToken string) *noRenewStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)

	acct := accounts.Account{
		ID:        "acc-norenew-1",
		Enabled:   true,
		Phone:     "13800000001",
		Password:  "pw",
		UserID:    "1000000001",
		PassToken: passToken,
	}
	if sessionCookie != "" {
		acct.Session = &accounts.Session{Cookie: sessionCookie, PH: testutil.TestPH, FetchedAt: time.Now().Unix()}
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
	p := pool.New([]accounts.Account{acct}, pool.Options{})
	client := mimo.New(mimo.Options{
		Base:          up.URL(),
		PassportHosts: []string{passport.Host()},
	})
	srv := New(Options{
		APIKeys:  []string{"sk-norenew-test"},
		Pool:     p,
		Accounts: store,
		Client:   client,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &noRenewStack{srv: srv, pool: p, store: store, passport: passport, upstream: up, ts: ts}
}

func (s *noRenewStack) chat() (status int, body string) {
	req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-norenew-test")
	resp, err := s.ts.Client().Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// NORENEW-001: a mid-life upstream 401 passes through to the client (mapped
// 401) with ZERO passport traffic — no Renew, no Login, no recovery. The
// account is not disabled by the request path: the hourly job owns repairs.
func TestRequestPathNeverRenewsOn401(t *testing.T) {
	s := newNoRenewStack(t, testutil.TestCookie, "V1:rt-live-1")
	// The stored session cookie is valid on the mock upstream, so script the
	// account's chat to answer 401: this is the "upstream says the session is
	// dead" case.
	s.upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusUnauthorized})

	status, body := s.chat()
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d (%s), want 401 passed through", status, body)
	}
	if !strings.Contains(body, "upstream_auth_expired") {
		t.Errorf("body = %q, want the mapped upstream_auth_expired error", body)
	}
	if n := len(s.passport.ServiceLoginCalls); n != 0 {
		t.Errorf("renewal calls from the request path = %d, want 0 (the hourly job owns renewal)", n)
	}
	if n := s.passport.Auth2CallCount(); n != 0 {
		t.Errorf("password logins from the request path = %d, want 0 (the hourly job owns login)", n)
	}
	// No retry against the upstream either: the failure is final, pass-through.
	if hits := s.upstream.ChatHits(); hits != 1 {
		t.Errorf("upstream chat hits = %d, want 1 (no refresh-retry)", hits)
	}
	// The account stays available: a dead session is the job's to fix, and the
	// request path must not disable it (same reasoning as the 429 ruling).
	if st := s.pool.StateOf("acc-norenew-1"); st != pool.StateAvailable {
		t.Errorf("pool state = %v, want available (the request path does not disable)", st)
	}
}

// NORENEW-002: no stored session → the request fails; it never logs in. The
// account has a passToken and phone+password on file, the exact shape the old
// creds() would have used to run the full chain.
func TestRequestPathNeverLoginsWithoutSession(t *testing.T) {
	s := newNoRenewStack(t, "", "V1:rt-live-1")

	status, _ := s.chat()
	if status == http.StatusOK {
		t.Fatal("chat succeeded without any stored session — where did the cookie come from?")
	}
	if n := len(s.passport.ServiceLoginCalls); n != 0 {
		t.Errorf("renewal calls from the request path = %d, want 0", n)
	}
	if n := s.passport.Auth2CallCount(); n != 0 {
		t.Errorf("password logins from the request path = %d, want 0", n)
	}
}

// NORENEW-003: a burst of requests against a session-less account runs zero
// logins even when they race — the request path has nothing left to race on.
func TestRequestPathBurstRunsZeroLogins(t *testing.T) {
	s := newNoRenewStack(t, "", "V1:rt-live-1")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.chat()
		}()
	}
	wg.Wait()
	if n := s.passport.Auth2CallCount(); n != 0 {
		t.Errorf("password logins for a burst of 4 = %d, want 0", n)
	}
	if n := len(s.passport.ServiceLoginCalls); n != 0 {
		t.Errorf("renewal calls for a burst of 4 = %d, want 0", n)
	}
}

// NORENEW-004: creds() itself — the shared resolver — answers with the stored
// session and nothing else. A second call returns the same pair; no counter
// anywhere moves.
func TestCredsReadsStoreOnly(t *testing.T) {
	s := newNoRenewStack(t, testutil.TestCookie, "V1:rt-live-1")

	for i := 0; i < 2; i++ {
		cookie, ph, err := s.srv.creds(context.Background(), "acc-norenew-1")
		if err != nil {
			t.Fatalf("creds call %d: %v", i+1, err)
		}
		if cookie != testutil.TestCookie || ph != testutil.TestPH {
			t.Fatalf("creds call %d = %q/%q, want the stored session", i+1, cookie, ph)
		}
	}
	if n := len(s.passport.ServiceLoginCalls); n != 0 {
		t.Errorf("renewal calls = %d, want 0", n)
	}
	if n := s.passport.Auth2CallCount(); n != 0 {
		t.Errorf("password logins = %d, want 0", n)
	}
}
