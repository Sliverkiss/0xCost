package api

import (
	"encoding/json"
	"io"
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

// consentAdminStack wires the full api.Server the way otp_admin_test.go's
// newOTPAdminStack does, but with the MiMo client built
// DisableCrossRegionAgree=true — precisely the configuration that leaves the
// account disabled with no API path forward until this endpoint exists.
type consentAdminStack struct {
	ts       *httptest.Server
	pool     *pool.Pool
	store    *accounts.Store
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
}

func newConsentAdminStack(t *testing.T, acct accounts.Account) *consentAdminStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)

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
		Base:                    up.URL(),
		PassportHosts:           []string{passport.Host()},
		DisableCrossRegionAgree: true, // the dead-end this endpoint closes
	})
	apiSrv := New(Options{
		APIKeys:  []string{"sk-admin-test"},
		Pool:     p,
		Accounts: store,
		Sessions: session.New(session.Options{}),
		Client:   client,
		// These in-package stacks exercise the streaming path; the deployed
		// default is non-streaming (ADR 0016).
		AllowStream: true,
	})
	hs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(hs.Close)
	return &consentAdminStack{ts: hs, pool: p, store: store, passport: passport, upstream: up}
}

// do performs an authenticated HTTP request against the stack.
func (s *consentAdminStack) do(method, path, apiKey, body string) (*http.Response, string) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, s.ts.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := s.ts.Client().Do(req)
	if err != nil {
		return nil, ""
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (s *consentAdminStack) pendingList(apiKey string) []pendingOTP {
	resp, body := s.do(http.MethodGet, "/v1/admin/otp/pending", apiKey, "")
	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Pending []pendingOTP `json:"pending"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Pending
}

// freshConsentAccount is a login-ready account with no cached session.
func freshConsentAccount() accounts.Account {
	return accounts.Account{ID: "acc-consent-1", Enabled: true, Phone: "13800000001", Password: "pw"}
}

// CONSENT-API-001: the endpoint sits behind the same Bearer auth as the OTP
// admin routes — no key and a wrong key are both 401, and nothing is attempted.
func TestConsentAgreeAuth(t *testing.T) {
	s := newConsentAdminStack(t, freshConsentAccount())
	for _, key := range []string{"", "wrong-key"} {
		resp, body := s.do(http.MethodPost, "/v1/admin/consent/agree", key, `{"account_id":"acc-consent-1"}`)
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q = %d %q, want 401", key, statusOf(resp), body)
		}
	}
	if len(s.passport.ConsentCalls) != 0 || len(s.passport.Auth2Calls) != 0 {
		t.Errorf("unauthorized request reached the login flow (consent=%d auth2=%d)",
			len(s.passport.ConsentCalls), len(s.passport.Auth2Calls))
	}
}

// CONSENT-API-002: missing/blank account_id (and malformed bodies) → 400.
func TestConsentAgreeValidation(t *testing.T) {
	s := newConsentAdminStack(t, freshConsentAccount())
	for _, body := range []string{`{`, `{}`, `{"account_id":""}`, `{"account_id":"   "}`} {
		resp, out := s.do(http.MethodPost, "/v1/admin/consent/agree", "sk-admin-test", body)
		if resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q = %d %q, want 400", body, statusOf(resp), out)
		}
	}
}

// CONSENT-API-003: unknown account id → 404 with the error envelope.
func TestConsentAgreeUnknownAccount(t *testing.T) {
	s := newConsentAdminStack(t, freshConsentAccount())
	resp, body := s.do(http.MethodPost, "/v1/admin/consent/agree", "sk-admin-test", `{"account_id":"nope"}`)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id = %d %q, want 404", statusOf(resp), body)
	}
	if !strings.Contains(body, "account_not_found") {
		t.Errorf("body = %q, want the account_not_found envelope", body)
	}
}

// CONSENT-API-004: an account already holding a usable session short-circuits
// to 200 {"session":"existing"} and never runs a consent/login attempt.
func TestConsentAgreeExistingSession(t *testing.T) {
	acct := freshConsentAccount()
	acct.Session = &accounts.Session{Cookie: testutil.TestCookie, PH: testutil.TestPH, FetchedAt: time.Now().Unix()}
	s := newConsentAdminStack(t, acct)

	resp, body := s.do(http.MethodPost, "/v1/admin/consent/agree", "sk-admin-test", `{"account_id":"acc-consent-1"}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("existing session = %d %q, want 200", statusOf(resp), body)
	}
	if !strings.Contains(body, `"session":"existing"`) || !strings.Contains(body, "acc-consent-1") {
		t.Errorf("body = %q, want session=existing + account", body)
	}
	if len(s.passport.ConsentCalls) != 0 || len(s.passport.Auth2Calls) != 0 {
		t.Errorf("login attempted for an account with a usable session (consent=%d auth2=%d)",
			len(s.passport.ConsentCalls), len(s.passport.Auth2Calls))
	}
}

// CONSENT-API-005: the consent override runs — it POSTs the agreement despite
// the client flag, reaches the new-device OTP demand, disables the account, and
// exposes the challenge through GET /v1/admin/otp/pending.
func TestConsentAgreeReachesOTPPending(t *testing.T) {
	s := newConsentAdminStack(t, freshConsentAccount())
	s.passport.SetConsentChain(true)

	resp, body := s.do(http.MethodPost, "/v1/admin/consent/agree", "sk-admin-test", `{"account_id":"acc-consent-1"}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("consent = %d %q, want 200", statusOf(resp), body)
	}
	if !strings.Contains(body, `"status":"otp_pending"`) || !strings.Contains(body, "acc-consent-1") {
		t.Fatalf("body = %q, want status=otp_pending + account", body)
	}
	// The agreement POST was actually issued despite DisableCrossRegionAgree.
	if len(s.passport.ConsentCalls) != 1 {
		t.Fatalf("consent calls = %d, want 1", len(s.passport.ConsentCalls))
	}
	c := s.passport.ConsentCalls[0]
	if c["userId"] != "123456" || c["status"] != "1" || c["needClose"] != "false" {
		t.Errorf("consent form = %v, want userId/status=1/needClose=false", c)
	}
	// The account waits (disabled) until the operator enters the code.
	if s.pool.StateOf("acc-consent-1") != pool.StateDisabled {
		t.Errorf("pool state = %s, want disabled while OTP pending", s.pool.StateOf("acc-consent-1"))
	}
	// The challenge is visible through the existing admin pending list.
	list := s.pendingList("sk-admin-test")
	if len(list) != 1 {
		t.Fatalf("pending = %d, want 1", len(list))
	}
	got := list[0]
	if got.AccountID != "acc-consent-1" || got.Method != "Phone" || got.Notify != "188****5616" || got.ID == "" {
		t.Errorf("pending challenge = %+v", got)
	}
	// The inline otp object mirrors the pending-list shape.
	var out struct {
		Status  string     `json:"status"`
		Account string     `json:"account"`
		OTP     pendingOTP `json:"otp"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Status != "otp_pending" || out.Account != "acc-consent-1" ||
		out.OTP.ID != got.ID || out.OTP.Notify != got.Notify || out.OTP.Method != got.Method ||
		out.OTP.AccountID != got.AccountID {
		t.Errorf("inline otp = %+v, want it to mirror the pending entry %+v", out.OTP, got)
	}
}

// CONSENT-API-006: a disabled account whose stale session cookie is still on
// file must still be servable — that combination is the very case an operator
// calls this endpoint for. (A healthy account with a session is the separate
// short-circuit case, CONSENT-API-004.)
func TestConsentAgreeDisabledWithStaleSessionRetries(t *testing.T) {
	acct := freshConsentAccount()
	acct.Session = &accounts.Session{Cookie: testutil.TestCookie, PH: testutil.TestPH, FetchedAt: time.Now().Unix()}
	s := newConsentAdminStack(t, acct)
	s.passport.SetConsentChain(true) // the chain the override must walk
	s.pool.ReportDisabled("acc-consent-1", "session dead after refresh", false)

	resp, body := s.do(http.MethodPost, "/v1/admin/consent/agree", "sk-admin-test", `{"account_id":"acc-consent-1"}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("disabled account = %d %q, want 200", statusOf(resp), body)
	}
	if strings.Contains(body, `"session":"existing"`) {
		t.Errorf("body = %q — a disabled account must not be short-circuited", body)
	}
	if len(s.passport.ConsentCalls) != 1 {
		t.Errorf("consent calls = %d, want 1: the override must actually run", len(s.passport.ConsentCalls))
	}
	if got := s.pool.StateOf("acc-consent-1"); got != pool.StateDisabled {
		t.Errorf("pool state = %s, want still disabled while the OTP code is outstanding", got)
	}
}
