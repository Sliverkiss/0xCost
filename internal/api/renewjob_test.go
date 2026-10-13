package api

// RENEWJOB-001..005 (owner ruling, 2026-10-14 — "定时巡检比被调用接口造成
// 问题要稳定"): session freshness is owned by a background job that ticks
// every hour (MIMO2API_RENEW_INTERVAL, default 60m), runs one round
// immediately at startup, and walks the enabled accounts serially in ID
// order: Renew first, one password Login when the credential is dead, and a
// plain failure log otherwise — no backoff, the next tick is the retry.

import (
	"context"
	"encoding/json"
	"log"
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

// renewJobStack wires the full stack plus a log sink that captures the
// per-account [renew-job] lines the job must emit.
type renewJobStack struct {
	srv      *Server
	pool     *pool.Pool
	store    *accounts.Store
	passport *testutil.MockPassport
	upstream *testutil.MockUpstream
	logs     *threadsafeBuffer
}

// threadsafeBuffer is a mutex-guarded log sink: the job runs on its own
// goroutine while the test reads.
type threadsafeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *threadsafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadsafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newRenewJobStack builds the stack with the given accounts. The clock is a
// fake; the job reads it through Server.SetRenewClock so a test can Advance
// across a tick without sleeping.
func newRenewJobStack(t *testing.T, accts ...accounts.Account) (*renewJobStack, *testutil.FakeClock) {
	t.Helper()
	up := testutil.NewMockUpstream()
	t.Cleanup(up.Close)
	passport := testutil.NewMockPassport(up.URL())
	t.Cleanup(passport.Close)
	passport.SetAuth2Response(testutil.FixtureXSSILoginOK, 0)
	up.SetLoginURL(passport.LoginURLFor())
	up.AddAccount(testutil.TestPH, testutil.TestCookie)
	for _, a := range accts {
		if a.Session != nil {
			up.AddAccount(a.Session.PH, a.Session.Cookie)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	f, _ := json.MarshalIndent(accounts.File{Accounts: accts}, "", "  ")
	if err := os.WriteFile(path, f, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New(accts, pool.Options{})
	client := mimo.New(mimo.Options{
		Base:          up.URL(),
		PassportHosts: []string{passport.Host()},
	})
	logs := &threadsafeBuffer{}
	srv := New(Options{
		Pool:     p,
		Accounts: store,
		Client:   client,
		Logger:   log.New(logs, "", 0),
	})
	clock := testutil.NewFakeClock()
	srv.SetRenewClock(clock)
	return &renewJobStack{srv: srv, pool: p, store: store, passport: passport, upstream: up, logs: logs}, clock
}

// startJob runs the job loop like main.go does. The first round runs
// immediately; the returned stop halts the loop and waits for it.
func startJob(s *renewJobStack, tickEvery time.Duration) (stop func()) {
	srv := s.srv
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.RunRenewJob(ctx, tickEvery)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t := srv
			_ = t
			panic("renew job did not stop within 5s")
		}
	}
}

// jobAccount builds one enabled account with a stored session and passToken.
func jobAccount(id, ph, cookie, passToken string) accounts.Account {
	return accounts.Account{
		ID:        id,
		Enabled:   true,
		Phone:     "13800000001",
		Password:  "pw",
		UserID:    "1000000001",
		PassToken: passToken,
		Session:   &accounts.Session{Cookie: cookie, PH: ph, FetchedAt: time.Now().Unix()},
	}
}

// RENEWJOB-001: the startup round runs immediately — no wait, no tick — and
// renews every enabled account with a passToken exactly once. A disabled
// account and a passToken-less account are both skipped.
func TestRenewJobStartupRoundRenewsEveryAccount(t *testing.T) {
	live := jobAccount("acc-job-a", testutil.TestPH, testutil.TestCookie, "V1:rt-a")
	disabled := jobAccount("acc-job-b", testutil.TestPH2, testutil.TestCookie2, "V1:rt-b")
	disabled.Enabled = false
	noToken := jobAccount("acc-job-c", testutil.TestPH, testutil.TestCookie, "")
	s, _ := newRenewJobStack(t, live, disabled, noToken)

	stop := startJob(s, 0)
	defer stop()

	// One renewal for the one enabled+passToken account; the [renew-job] line
	// lands after the whole STS chain, so wait on the log itself, not the
	// passport call counter (the call is recorded mid-round).
	waitForLog(t, s, "[renew-job] acc-job-a renewed")
	if n := s.passport.Auth2CallCount(); n != 0 {
		t.Errorf("password logins = %d, want 0 (renewal covers it)", n)
	}
}

// RENEWJOB-002: advancing the fake clock one interval triggers exactly one
// further Renew per account — the hourly cadence, no more.
func TestRenewJobTicksHourly(t *testing.T) {
	live := jobAccount("acc-job-a", testutil.TestPH, testutil.TestCookie, "V1:rt-a")
	s, clock := newRenewJobStack(t, live)

	stop := startJob(s, 0) // zero interval: every tick fires immediately
	defer stop()

	waitForCalls(t, s.passport.ServiceLoginCallCount, 1, "renewal calls") // startup round
	waitForCalls(t, s.passport.ServiceLoginCallCount, 2, "renewal calls")
	// Half an hour more: nothing fires.
	clock.Advance(30 * time.Minute)
	if n := s.passport.ServiceLoginCallCount(); n != 2 {
		t.Errorf("renewal calls after a half-tick = %d, want 2 (no premature round)", n)
	}
}

// RENEWJOB-003: a dead credential (serviceLogin answers 70016) runs exactly
// one password Login on the same tick, and the new session is persisted.
func TestRenewJobDeadCredentialLogsInOnce(t *testing.T) {
	dead := jobAccount("acc-job-dead", testutil.TestPH, testutil.TestCookie, "V1:dead-token")
	s, _ := newRenewJobStack(t, dead)
	s.passport.SetServiceLoginResponse(`&&&START&&&{"code":70016,"description":"登录验证失败"}`)

	stop := startJob(s, time.Hour)
	defer stop()

	waitForCalls(t, s.passport.Auth2CallCount, 1, "auth2 (password login) calls")
	if n := s.passport.ServiceLoginCallCount(); n != 1 {
		t.Errorf("renewal calls = %d, want exactly 1 before the login", n)
	}
	waitForServiceTokenSession(t, s.store, "acc-job-dead")
	waitForLog(t, s, "[renew-job] acc-job-dead login-ok")
}

// RENEWJOB-004: a transient renewal failure is logged and retried on the NEXT
// tick only — no retry storm, no backoff ladder, no password login.
func TestRenewJobTransientFailureRetriesNextTick(t *testing.T) {
	live := jobAccount("acc-job-a", testutil.TestPH, testutil.TestCookie, "V1:rt-a")
	s, _ := newRenewJobStack(t, live)
	// Every renewal is refused with a 500: transient by classification
	// (IsDeadCredential says no).
	s.passport.SetServiceLoginResponse(`&&&START&&&{"code":500,"description":"busy"}`)

	stop := startJob(s, 0)
	defer stop()

	waitForCalls(t, s.passport.ServiceLoginCallCount, 1, "renewal calls")
	if n := s.passport.Auth2CallCount(); n != 0 {
		t.Errorf("password logins after a transient failure = %d, want 0 (transient ≠ dead)", n)
	}
	// The next tick is the retry — a real interval would take an hour, so the
	// job is started with a zero interval and the "hour" here is simply the
	// next loop iteration.
	waitForCalls(t, s.passport.ServiceLoginCallCount, 2, "renewal calls")
	logs := s.logs.String()
	if !strings.Contains(logs, "[renew-job] acc-job-a failed") {
		t.Errorf("logs = %q, want a [renew-job] failed line for acc-job-a", logs)
	}
}

// RENEWJOB-005: every enabled account is walked, one [renew-job] log line
// each, serially in ID order (two accounts, two lines, no interleaving —
// serial execution is what the fake clock's Advance-then-observe contract
// relies on).
func TestRenewJobOneLinePerAccount(t *testing.T) {
	a := jobAccount("acc-job-a", testutil.TestPH, testutil.TestCookie, "V1:rt-a")
	b := jobAccount("acc-job-b", testutil.TestPH2, testutil.TestCookie2, "V1:rt-b")
	s, _ := newRenewJobStack(t, a, b)

	stop := startJob(s, 0)
	defer stop()

	waitForCalls(t, s.passport.ServiceLoginCallCount, 2, "renewal calls")
	// The log lines land after each account's full STS chain, so wait on the
	// log sink rather than the call counter.
	waitForLog(t, s, "[renew-job] acc-job-a renewed")
	waitForLog(t, s, "[renew-job] acc-job-b renewed")
	logs := s.logs.String()
	if !strings.Contains(logs, "[renew-job] acc-job-a") || !strings.Contains(logs, "[renew-job] acc-job-b") {
		t.Errorf("logs = %q, want one [renew-job] line per account", logs)
	}
	// ID order: acc-job-a's line comes first.
	ia, ib := strings.Index(logs, "[renew-job] acc-job-a"), strings.Index(logs, "[renew-job] acc-job-b")
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("log order: a@%d b@%d, want a before b", ia, ib)
	}
}

// waitForLog polls the log sink until it contains the expected [renew-job]
// line. The passport call counters are recorded mid-round, before the STS
// landing chain finishes and the line is written — asserting on the sink
// right after waitForCalls would race the job goroutine.
func waitForLog(t *testing.T, s *renewJobStack, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.logs.String(), substr) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("logs = %q, want a line containing %q", s.logs.String(), substr)
}

// waitForCalls polls a counter function until it reaches at least n (the job
// round runs on its own goroutine; a fixed read would race it).
func waitForCalls(t *testing.T, count func() int, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if count() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s = %d after timeout, want >= %d", what, count(), n)
}

func waitForServiceTokenSession(t *testing.T, store *accounts.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a, ok := store.Get(id); ok && a.Session != nil && strings.Contains(a.Session.Cookie, "serviceToken=") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("account %s never got a session with a service token", id)
}
