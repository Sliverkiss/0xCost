package api

// Asynchronous credential recovery (ADR 0014).
//
// The 12-hour keepalive can find the credential revoked; a live conversation can
// find it too. Both must hand the account to one background password re-login —
// single flight, no implicit SMS, and the identity harvested on the way is kept
// even when the login stops at a human gate.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

type recoveryStack struct {
	ts       *testing.T
	srv      *Server
	pool     *pool.Pool
	store    *accounts.Store
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
	hs       *http.Client
	base     string
}

// newRecoveryStack builds the stack with an account whose credential is dead:
// no cached session, a stored-but-revoked passToken, phone+password on file.
func newRecoveryStack(t *testing.T, auth2 string, otpAuto bool) *recoveryStack {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(auth2, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)

	acct := accounts.Account{
		ID:        "acc-recovery-1",
		Enabled:   true,
		Phone:     "13800000001",
		Password:  "pw",
		UserID:    "1000000001",
		PassToken: "V1:revoked-1",
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
		Base:           up.URL(),
		PassportHosts:  []string{passport.Host()},
		DisableOTPAuto: !otpAuto,
	})
	srv := New(Options{
		APIKeys:  []string{"sk-admin-test"},
		Pool:     p,
		Accounts: store,
		Sessions: session.New(session.Options{}),
		Client:   client,
		// These in-package stacks exercise the streaming path; the deployed
		// default is non-streaming (ADR 0016).
		AllowStream: true,
	})
	return &recoveryStack{ts: t, srv: srv, pool: p, store: store, passport: passport, upstream: up}
}

// waitForSession polls the store until the account has a session (or the
// deadline passes), because the recovery runs in the background by design. The
// bound is intentionally loose: polling an asynchronous goroutine with a tight
// deadline turns machine load into a flaky failure.
func (s *recoveryStack) waitForSession(d time.Duration) *accounts.Account {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		acct, _ := s.store.Get("acc-recovery-1")
		if acct.Session != nil && acct.Session.Cookie != "" {
			return &acct
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// REC-001: a dead credential is recovered by one background password login, and
// concurrent triggers collapse into that one attempt.
func TestRecoveryHealsRevokedCredential(t *testing.T) {
	s := newRecoveryStack(t, testutil.FixtureXSSILoginOK, true)

	if !s.srv.RecoverCredentialAsync("acc-recovery-1", "test: revocation") {
		t.Fatal("first recovery was not started")
	}
	if s.srv.RecoverCredentialAsync("acc-recovery-1", "test: duplicate trigger") {
		t.Error("a second recovery started while one was in flight — one IP, two password logins, is what risk control reacts to")
	}

	acct := s.waitForSession(3 * time.Second)
	if acct == nil {
		t.Fatal("recovery did not persist a session")
	}
	if acct.PassToken == "" || acct.PassToken == "V1:revoked-1" {
		t.Errorf("passToken = %q, want the rotated credential the login returned", acct.PassToken)
	}
	if got := s.pool.StateOf("acc-recovery-1"); got == pool.StateDisabled {
		t.Errorf("account state = %v after a successful recovery, want it selectable again", got)
	}
	snap := s.srv.recovery.Snapshot()
	if n, _ := snap["attempts"].(map[string]int)["acc-recovery-1"]; n != 1 {
		t.Errorf("attempts = %d, want exactly 1", n)
	}
}

// REC-002: when the login stops at a human gate, the recovery keeps the device
// identity it harvested, takes the account out of rotation with a reason, and
// dispatches nothing — an SMS is only ever spent on purpose (ADR 0013).
func TestRecoveryStopsAtHumanGate(t *testing.T) {
	s := newRecoveryStack(t, testutil.FixtureXSSILoginOTPZero, false)

	if !s.srv.RecoverCredentialAsync("acc-recovery-1", "test: human gate") {
		t.Fatal("recovery was not started")
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if s.pool.StateOf("acc-recovery-1") == pool.StateDisabled {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.pool.StateOf("acc-recovery-1"); got != pool.StateDisabled {
		t.Fatalf("account state = %v, want disabled (the operator must see it)", got)
	}
	if s.pool.PermanentOf("acc-recovery-1") {
		t.Error("the account was disabled permanently; a human can still fix it")
	}
	acct, _ := s.store.Get("acc-recovery-1")
	if len(acct.Device) == 0 {
		t.Error("the device identity the chain harvested was not persisted — the next attempt would be new hardware again")
	}
	if acct.Session != nil {
		t.Error("a session was persisted although the login never completed")
	}
}

// REC-003: a transient failure backs off, so a broken credential cannot be
// hammered once per sweep or once per request.
func TestRecoveryBacksOffAfterFailure(t *testing.T) {
	s := newRecoveryStack(t, testutil.FixtureXSSILoginOK, true)
	s.passport.SetAuth2Response(testutil.FixtureXSSILoginOK, http.StatusInternalServerError)

	if !s.srv.RecoverCredentialAsync("acc-recovery-1", "test: transient") {
		t.Fatal("recovery was not started")
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if snap := s.srv.recovery.Snapshot(); snap["attempts"].(map[string]int)["acc-recovery-1"] > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.srv.RecoverCredentialAsync("acc-recovery-1", "test: too soon") {
		t.Error("a second attempt started inside the cooldown")
	}
	snap := s.srv.recovery.Snapshot()
	if n := snap["attempts"].(map[string]int)["acc-recovery-1"]; n != 1 {
		t.Errorf("attempts = %d, want 1 (the second trigger must be refused)", n)
	}
	if msg, _ := snap["last_error"].(map[string]string)["acc-recovery-1"]; msg == "" {
		t.Error("the failure was not recorded — an operator would see nothing")
	}
}

// REC-004: dead-credential detection is what routes an account to the recovery
// instead of leaving it to the next 12-hour sweep.
func TestIsDeadCredential(t *testing.T) {
	cases := []struct {
		msg  string
		dead bool
	}{
		{"passToken expired (code 70016): 登录验证失败", true},
		{"renewal serviceLogin: code 70016", true},
		{"login requires slider captcha; manual re-login needed", true},
		{"new-device OTP verification required (code=20024 )", true},
		{"renewal probe: dial tcp 1.2.3.4:443: connect: connection refused", false},
		{"renewal response missing location", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = &testError{c.msg}
		}
		if got := IsDeadCredential(err); got != c.dead {
			t.Errorf("IsDeadCredential(%q) = %v, want %v", c.msg, got, c.dead)
		}
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// REC-005: the conversation path does not block on a login. It schedules the
// recovery and answers with an account action the caller can retry.
func TestCredsSchedulesRecoveryAndFailsFast(t *testing.T) {
	s := newRecoveryStack(t, testutil.FixtureXSSILoginOK, true)
	// The renewal (which creds tries before any password login) reports the
	// credential as revoked; the mock's own STS body would say otherwise.
	s.passport.SetServiceLoginResponse(testutil.FixtureXSSISTS70016)

	_, _, err := s.srv.creds(context.Background(), "acc-recovery-1")
	if err == nil {
		t.Fatal("creds returned no error for a revoked credential")
	}
	var action *mimo.ErrAccountAction
	if !errors.As(err, &action) {
		t.Fatalf("error = %v (%T), want ErrAccountAction so the caller retries instead of blocking", err, err)
	}
	if !strings.Contains(action.Reason, "retry") {
		t.Errorf("reason = %q, want it to tell the caller to retry", action.Reason)
	}
	if !s.srv.recovery.InFlight("acc-recovery-1") {
		t.Error("no recovery was scheduled — the account would stay dead until the next sweep")
	}
	// Let the scheduled login finish before the mocks are torn down, so the test
	// leaves no goroutine logging against a closed server.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && s.srv.recovery.InFlight("acc-recovery-1") {
		time.Sleep(5 * time.Millisecond)
	}
}
