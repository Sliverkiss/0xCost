package api

// RENEW-005 / RENEW-006 (TDD §3.4, SDD §7.1): the transparent refresh-retry
// on a mid-life 401. The production path lived in chat.go without a test that
// pinned either outcome; these two close that hole.

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

// newRefreshStack wires the OTP-admin stack with an account that has BOTH a
// cached session (so the first attempt uses it, no renewal up front) and a
// stored passToken (so a mid-life 401 can be refreshed).
func newRefreshStack(t *testing.T, passToken string) *otpAdminStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)

	acct := accounts.Account{
		ID:        "acc-refresh-1",
		Enabled:   true,
		Phone:     "13800000001",
		Password:  "pw",
		UserID:    "1000000001",
		PassToken: passToken,
		Session: &accounts.Session{
			Cookie:    testutil.TestCookie,
			PH:        testutil.TestPH,
			FetchedAt: time.Now().Unix(),
		},
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
	return &otpAdminStack{ts: hs, apiSrv: apiSrv, pool: p, store: store, passport: passport, upstream: up}
}

// RENEW-005: a 401 mid-life (expired session on the upstream, account still
// healthy) must be repaired invisibly — exactly one renewal, exactly one
// retry, and the client still sees a clean 200 SSE stream.
func TestTransparentRefreshRetryOn401(t *testing.T) {
	s := newRefreshStack(t, "V1:rt-live-1")
	// First attempt: 401. Second attempt (after the renewal): a normal stream.
	s.upstream.Script(testutil.TestPH,
		testutil.ScriptStep{StatusCode: http.StatusUnauthorized},
		testutil.ScriptStep{Body: testutil.FixtureSSEBasic},
	)

	resp, body := s.do(http.MethodPost, "/v1/chat/completions", "sk-admin-test",
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chat after refresh = %v %q, want a clean 200", resp, body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("stream did not complete: %q", body)
	}
	if hits := s.upstream.ChatHits(); hits != 2 {
		t.Errorf("upstream chat attempts = %d, want exactly 2 (original + one retry)", hits)
	}
	if n := len(s.passport.ServiceLoginCalls); n != 1 {
		t.Errorf("renewals = %d, want exactly 1", n)
	}
	if n := len(s.passport.Auth2Calls); n != 0 {
		t.Errorf("auth2 calls = %d — a refresh must never fall back to the password login", n)
	}
	acct, ok := s.store.Get("acc-refresh-1")
	if !ok {
		t.Fatal("account vanished")
	}
	if acct.Session == nil || acct.Session.Cookie == "" {
		t.Fatal("refreshed session was not persisted")
	}
	if acct.PassToken != "V1:rt-live-1" {
		t.Errorf("passToken changed to %q during the refresh", acct.PassToken)
	}
	if st := s.pool.StateOf("acc-refresh-1"); st != pool.StateAvailable {
		t.Errorf("pool state = %s, want the account still in rotation after a successful refresh", st)
	}
}

// RENEW-006: when the refresh itself fails (dead passToken, code=70016) the
// account leaves rotation and the client gets the mapped 401 — never a retry
// loop against a dead session.
func TestRefreshFailureDisablesAndMapsError(t *testing.T) {
	s := newRefreshStack(t, "V1:dead-token")
	// The real passport answers with the XSSI prefix; Renew classifies 70016
	// only when the body parses (parseXSSI), so the fixture must be exact.
	s.passport.SetServiceLoginResponse(`&&&START&&&{"code":70016,"description":"登录验证失败"}`)
	// A single scripted 401: if the pipeline wrongly retried after a failed
	// refresh, the second hit would be visible in ChatHits().
	s.upstream.Script(testutil.TestPH, testutil.ScriptStep{StatusCode: http.StatusUnauthorized})

	resp, body := s.do(http.MethodPost, "/v1/chat/completions", "sk-admin-test",
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if resp == nil {
		t.Fatal("no response")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d (%s), want 401", resp.StatusCode, body)
	}
	var e struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("error body %q is not the §7.4 shape: %v", body, err)
	}
	if e.Error.Type != "invalid_request_error" || e.Error.Code != "upstream_auth_expired" {
		t.Errorf("error = %s/%s, want invalid_request_error/upstream_auth_expired", e.Error.Type, e.Error.Code)
	}
	if n := len(s.passport.ServiceLoginCalls); n != 1 {
		t.Errorf("renewal attempts = %d, want exactly 1", n)
	}
	if hits := s.upstream.ChatHits(); hits != 1 {
		t.Errorf("upstream chat attempts = %d, want 1 (no retry after a failed refresh)", hits)
	}
	if st := s.pool.StateOf("acc-refresh-1"); st != pool.StateDisabled {
		t.Errorf("pool state = %s, want disabled (dead passToken needs an operator)", st)
	}
	// The disabled state + reason live in the pool at runtime; main.go syncs
	// them back into accounts.json (SDD §7.5), so that is what to assert here.
	state, _, reason := s.pool.RuntimeState("acc-refresh-1")
	if state != pool.StateDisabled || reason == "" {
		t.Errorf("runtime state = %s / %q, want disabled with an operator-actionable reason", state, reason)
	}
}
