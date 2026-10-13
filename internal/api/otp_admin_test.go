package api

import (
	"context"
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
	"github.com/yg/mimo2api/internal/testutil"
)

// otpAdminStack wires a full api.Server whose only account has a passToken
// but no cached session — the exact shape the hourly renew job picks up: it
// renews, the renewal comes back dead (70016), and the job's single password
// Login runs against the MockPassport's serviceLoginAuth2. This stack scripts
// the OTP path: the passport's serviceLoginAuth2 answers the live-captured
// code=0 OTP demand, and the MockUpstream's 401 probe carries the
// MockPassport's loginUrl, so a dead-token renewal falls through to
// Login → ErrOTPPending → challenge registered (SDD §2.2 amendment).
type otpAdminStack struct {
	ts       *httptest.Server
	apiSrv   *Server
	pool     *pool.Pool
	store    *accounts.Store
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
}

func newOTPAdminStack(t *testing.T) *otpAdminStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOTPZero, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie) // the OTP-minted session serves after submit

	acct := accounts.Account{
		ID:        "acc-otp-1",
		Enabled:   true,
		Phone:     "13800000001",
		Password:  "pw",
		UserID:    "123456",
		PassToken: "rt-dead-1",
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
	apiSrv := New(Options{
		APIKeys:  []string{"sk-admin-test"},
		Pool:     p,
		Accounts: store,
		Client:   client,
	})
	hs := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(hs.Close)
	return &otpAdminStack{ts: hs, apiSrv: apiSrv, pool: p, store: store, passport: passport, upstream: up}
}

// do performs an authenticated HTTP request against the stack.
func (s *otpAdminStack) do(method, path, apiKey, body string) (*http.Response, string) {
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

func (s *otpAdminStack) pendingList(apiKey string) []pendingOTP {
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

// seedOTPChallenge runs one synchronous renew round against the account with
// a scripted-dead passToken (70016): the job's Renew fails dead, its single
// password Login hits the OTP-zero scripted auth2, and the challenge lands
// in the store. The request path (chat/audio) never triggers a login under
// the hourly-job ruling — this is the job's flow, exercised synchronously.
func (s *otpAdminStack) seedOTPChallenge(t *testing.T) pendingOTP {
	t.Helper()
	// Script the renewal dead (70016) for this one synchronous round, then
	// restore the default (code=0 + location) so later renewals succeed.
	s.passport.SetServiceLoginResponse(`&&&START&&&{"code":70016,"description":"登录验证失败"}`)
	defer s.passport.SetServiceLoginResponse("")
	s.apiSrv.renewRound(context.Background())
	list := s.pendingList("sk-admin-test")
	if len(list) != 1 {
		t.Fatalf("pending after the job round = %d, want 1", len(list))
	}
	return list[0]
}

// ADOTP-001: full admin flow — the renew job's login hits the OTP demand and
// disables the account; pending lists the challenge (key-protected); wrong
// code is rejected with the challenge retained; the correct code completes
// the login, persists the session, and re-enables the account; a follow-up
// chat request succeeds on the OTP-minted session.
func TestAdminOTPFullFlow(t *testing.T) {
	s := newOTPAdminStack(t)
	s.upstream.Script(testutil.TestPH, testutil.ScriptStep{Body: testutil.FixtureSSEBasic})

	// 1. The job's login on a dead passToken hits the OTP demand: the account
	// is disabled while the code is outstanding.
	ch := s.seedOTPChallenge(t)
	if s.pool.StateOf("acc-otp-1") != pool.StateDisabled {
		t.Errorf("pool state = %s, want disabled while OTP pending", s.pool.StateOf("acc-otp-1"))
	}

	// 2. Pending list: one challenge, key-protected.
	if got := s.pendingList(""); got != nil {
		t.Fatalf("pending without key must 401, got %v", got)
	}
	list := s.pendingList("sk-admin-test")
	if len(list) != 1 {
		t.Fatalf("pending = %d, want 1", len(list))
	}
	if ch.Method != "Phone" || ch.Notify != "188****5616" || ch.AccountID != "acc-otp-1" {
		t.Errorf("challenge = %+v", ch)
	}
	if ch.ExpiresAt < time.Now().Unix() {
		t.Errorf("expiry in the past: %d", ch.ExpiresAt)
	}

	// 3. Wrong code: rejected, challenge retained.
	s.passport.OTPWrongCode = true
	resp, body := s.do(http.MethodPost, "/v1/admin/otp/submit", "sk-admin-test",
		`{"id":"`+ch.ID+`","code":"000000"}`)
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong code = %d %q, want 400", statusOf(resp), body)
	}
	if !strings.Contains(body, "otp_rejected") {
		t.Errorf("body = %q, want otp_rejected", body)
	}
	if len(s.pendingList("sk-admin-test")) != 1 {
		t.Fatal("challenge must be retained after a wrong code")
	}

	// 4. Correct code: login completes, session persisted, account enabled.
	s.passport.OTPWrongCode = false
	resp, body = s.do(http.MethodPost, "/v1/admin/otp/submit", "sk-admin-test",
		`{"id":"`+ch.ID+`","code":"123456"}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("submit = %d %q, want 200", statusOf(resp), body)
	}
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, "acc-otp-1") {
		t.Errorf("body = %q, want ok + account", body)
	}
	if got := s.pendingList("sk-admin-test"); len(got) != 0 {
		t.Errorf("pending after submit = %d, want 0", len(got))
	}
	if s.pool.StateOf("acc-otp-1") != pool.StateAvailable {
		t.Errorf("pool state = %s, want available after submit", s.pool.StateOf("acc-otp-1"))
	}
	acct, ok := s.store.Get("acc-otp-1")
	if !ok || acct.Session == nil || acct.Session.Cookie == "" {
		t.Fatalf("session not persisted: %+v", acct)
	}
	if acct.PassToken != "rt-mock-1" || acct.UserID != "123456" {
		t.Errorf("passToken/userId not persisted: %q/%q", acct.PassToken, acct.UserID)
	}

	// 5. Follow-up chat: served on the OTP-minted session.
	resp, body = s.do(http.MethodPost, "/v1/chat/completions", "sk-admin-test",
		`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hello"}]}`)
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("chat after OTP = %d %q, want 200", statusOf(resp), body)
	}
	if !strings.Contains(body, "Hello world") {
		t.Errorf("body = %q, want fixture content", body)
	}
}

// ADOTP-002: submit validation — unknown id 404, malformed body 400.
func TestAdminOTPSubmitValidation(t *testing.T) {
	s := newOTPAdminStack(t)
	resp, body := s.do(http.MethodPost, "/v1/admin/otp/submit", "sk-admin-test", `{"id":"otp-nope","code":"1"}`)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id = %d %q, want 404", statusOf(resp), body)
	}
	resp, body = s.do(http.MethodPost, "/v1/admin/otp/submit", "sk-admin-test", `{`)
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body = %d %q, want 400", statusOf(resp), body)
	}
	resp, body = s.do(http.MethodPost, "/v1/admin/otp/submit", "sk-admin-test", `{"id":"","code":""}`)
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty fields = %d %q, want 400", statusOf(resp), body)
	}
}

// ADOTP-003: pending is auth-protected like every /v1 route.
func TestAdminOTPPendingAuth(t *testing.T) {
	s := newOTPAdminStack(t)
	resp, _ := s.do(http.MethodGet, "/v1/admin/otp/pending", "wrong-key", "")
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key = %d, want 401", statusOf(resp))
	}
}

// ADOTP-004: expired challenges vanish from pending (safe cleanup).
func TestAdminOTPExpirySweep(t *testing.T) {
	s := newOTPAdminStack(t)
	// Seed one challenge through the renew job's login path.
	s.seedOTPChallenge(t)
	list := s.pendingList("sk-admin-test")
	if len(list) != 1 {
		t.Fatalf("pending = %d, want 1", len(list))
	}
	// Expire it behind the store's back, then re-list.
	s.apiSrv.otp.mu.Lock()
	for _, p := range s.apiSrv.otp.items {
		p.Challenge.ExpiresAt = time.Now().Add(-time.Second)
	}
	s.apiSrv.otp.mu.Unlock()
	if got := s.pendingList("sk-admin-test"); len(got) != 0 {
		t.Errorf("pending after expiry = %d, want 0", len(got))
	}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return -1
	}
	return resp.StatusCode
}
