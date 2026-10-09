package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/testutil"
)

// newPool builds an n-account pool with a fake clock (TDD §1.5: no sleeps).
func newPool(n int, opts Options, probe ProbeFunc, renew RenewFunc) (*Pool, *testutil.FakeClock) {
	fc := testutil.NewFakeClock()
	opts.Now = fc.Now
	var accts []accounts.Account
	for i := 1; i <= n; i++ {
		accts = append(accts, accounts.Account{ID: fmt.Sprintf("a%d", i), Enabled: true})
	}
	return New(accts, opts, probe, renew), fc
}

// POOL-001: round-robin fairness — 3 accounts, 30 sequential acquires land
// 10±1 on each (SDD §4.4).
func TestRoundRobinFairness(t *testing.T) {
	p, _ := newPool(3, Options{}, nil, nil)
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		counts[l.ID]++
		l.Release()
	}
	if len(counts) != 3 {
		t.Fatalf("distinct accounts used = %d, want 3 (%v)", len(counts), counts)
	}
	for id, n := range counts {
		if n < 9 || n > 11 {
			t.Errorf("account %s got %d acquires, want 10±1", id, n)
		}
	}
}

// POOL-002: a 429 puts the account into cooldown until now+60s and excludes
// it from selection (SDD §4.5).
func TestCooldownOn429(t *testing.T) {
	p, fc := newPool(2, Options{}, nil, nil)
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := l.ID
	l.Release()

	p.Report429(first)
	if st := p.StateOf(first); st != StateCooldown {
		t.Fatalf("state = %s, want cooldown", st)
	}
	want := fc.Now().Add(60 * time.Second)
	if got := p.CooldownUntilOf(first); !got.Equal(want) {
		t.Errorf("cooldown until = %v, want %v (now+60s)", got, want)
	}

	// The cooling account must not be selected while the other is healthy.
	l2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	if l2.ID == first {
		t.Error("cooling account was selected")
	}

	// Single-account pool: acquiring the only (cooling) account fails closed.
	p1, _ := newPool(1, Options{}, nil, nil)
	p1.Report429("a1")
	_, err = p1.Acquire(context.Background())
	var parked *ErrParked
	if !errors.As(err, &parked) {
		t.Fatalf("single cooling account: err = %v, want ErrParked", err)
	}
	if parked.RetryAfter != 60*time.Second {
		t.Errorf("Retry-After = %v, want 60s", parked.RetryAfter)
	}
	if parked.RetryAfterSeconds() < 1 {
		t.Errorf("RetryAfterSeconds = %d, want >= 1", parked.RetryAfterSeconds())
	}
}

// POOL-003: the 429 cooldown ladder — 60s doubling to the 10m cap
// (60/120/240/480/600/600, SDD §4.5).
func TestCooldownLadder429(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	ladder := []time.Duration{60, 120, 240, 480, 600, 600}
	for i, want := range ladder {
		p.Report429("a1")
		got := p.CooldownUntilOf("a1").Sub(fc.Now())
		if got != want*time.Second {
			t.Errorf("ladder step %d: cooldown = %v, want %v", i+1, got, want*time.Second)
		}
	}
}

// POOL-004: a success resets the ladder — two 429s then a success, the next
// 429 cools for the base 60s again (SDD §4.5).
func TestSuccessResetsLadder(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	p.Report429("a1")
	p.Report429("a1")
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 120*time.Second {
		t.Fatalf("second 429 cooldown = %v, want 120s", got)
	}
	p.ReportSuccess("a1")
	p.Report429("a1")
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 60*time.Second {
		t.Errorf("post-reset cooldown = %v, want 60s", got)
	}
}

// POOL-005: the 5xx/transport ladder starts at 30s and doubles (30/60/120,
// SDD §4.5).
func TestCooldownLadder5xx(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	ladder := []time.Duration{30, 60, 120}
	for i, want := range ladder {
		p.Report5xx("a1")
		got := p.CooldownUntilOf("a1").Sub(fc.Now())
		if got != want*time.Second {
			t.Errorf("5xx ladder step %d: cooldown = %v, want %v", i+1, got, want*time.Second)
		}
	}
}

// POOL-006: recovery probe gate — an expired cooldown alone does NOT make an
// account selectable; a probe must pass first (SDD §4.5).
func TestProbeGate(t *testing.T) {
	healthy := false
	probe := func(context.Context, string) error {
		if healthy {
			return nil
		}
		return errors.New("upstream 503")
	}
	p, fc := newPool(1, Options{}, probe, nil)
	p.Report429("a1")
	fc.Advance(61 * time.Second) // cooldown expired

	// Expired but unprobed: still parked.
	_, err := p.Acquire(context.Background())
	var parked *ErrParked
	if !errors.As(err, &parked) {
		t.Fatalf("expired-but-unprobed: err = %v, want ErrParked", err)
	}

	// Probe sick: stays cooling (re-armed at the 5xx base).
	if n := p.ProbeSweep(context.Background()); n != 0 {
		t.Errorf("sweep recovered %d, want 0", n)
	}
	if _, err := p.Acquire(context.Background()); !errors.As(err, &parked) {
		t.Fatalf("after sick probe: err = %v, want ErrParked", err)
	}

	// Probe OK: available again.
	fc.Advance(31 * time.Second) // past the re-armed cooldown
	healthy = true
	if n := p.ProbeSweep(context.Background()); n != 1 {
		t.Fatalf("sweep recovered %d, want 1", n)
	}
	if st := p.StateOf("a1"); st != StateAvailable {
		t.Fatalf("state after healthy probe = %s, want available", st)
	}
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after recovery: %v", err)
	}
	l.Release()
}

// POOL-007: a 401 probe triggers exactly one STS renewal — success flips the
// account to available; failure disables it (SDD §4.5).
func TestProbeAuthRenewalSuccess(t *testing.T) {
	renewCalls := 0
	probe := func(context.Context, string) error { return AuthError(errors.New("401")) }
	renew := func(context.Context, string) error {
		renewCalls++
		return nil
	}
	p, fc := newPool(1, Options{}, probe, renew)
	p.Report429("a1")
	fc.Advance(61 * time.Second)
	if n := p.ProbeSweep(context.Background()); n != 1 {
		t.Fatalf("sweep recovered %d, want 1", n)
	}
	if renewCalls != 1 {
		t.Errorf("renew calls = %d, want exactly 1", renewCalls)
	}
	if st := p.StateOf("a1"); st != StateAvailable {
		t.Errorf("state = %s, want available", st)
	}
}

// POOL-007 (failure branch): renewal failure disables the account and the
// pool then fails closed with no_accounts.
func TestProbeAuthRenewalFailure(t *testing.T) {
	renewCalls := 0
	probe := func(context.Context, string) error { return AuthError(errors.New("401")) }
	renew := func(context.Context, string) error {
		renewCalls++
		return errors.New("passToken expired")
	}
	p, fc := newPool(1, Options{}, probe, renew)
	p.Report429("a1")
	fc.Advance(61 * time.Second)
	if n := p.ProbeSweep(context.Background()); n != 0 {
		t.Fatalf("sweep recovered %d, want 0", n)
	}
	if renewCalls != 1 {
		t.Errorf("renew calls = %d, want exactly 1", renewCalls)
	}
	if st := p.StateOf("a1"); st != StateDisabled {
		t.Errorf("state = %s, want disabled", st)
	}
	_, _, reason := p.RuntimeState("a1")
	if !strings.Contains(reason, "renewal failed") {
		t.Errorf("disabled reason = %q, want renewal-failure text", reason)
	}
	_, err := p.Acquire(context.Background())
	if !errors.Is(err, ErrNoAccounts) {
		t.Errorf("disabled-only pool: err = %v, want ErrNoAccounts", err)
	}
}

// POOL-008: all accounts cooling → fail-closed 429 pool_parked with
// Retry-After = nearest recovery (SDD §7.4).
func TestAllCoolingParked(t *testing.T) {
	p, fc := newPool(2, Options{}, nil, nil)
	p.Report429("a1") // until t0+60
	fc.Advance(10 * time.Second)
	p.Report429("a2") // until t0+10+60 = t0+70
	// Nearest recovery: a1 at t0+60, i.e. 50s from now.
	_, err := p.Acquire(context.Background())
	var parked *ErrParked
	if !errors.As(err, &parked) {
		t.Fatalf("err = %v, want ErrParked", err)
	}
	if parked.RetryAfter != 50*time.Second {
		t.Errorf("Retry-After = %v, want 50s (nearest recovery)", parked.RetryAfter)
	}
}

// POOL-009: per-account in-flight semaphore — max_inflight=2, no third
// concurrent claim (SDD §4.4).
func TestInflightSemaphore(t *testing.T) {
	p, _ := newPool(1, Options{QueueWait: 50 * time.Millisecond}, nil, nil)
	l1, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Acquire(context.Background())
	var busy *ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("third concurrent acquire: err = %v, want ErrBusy", err)
	}
	if busy.RetryAfterSeconds() < 1 {
		t.Errorf("RetryAfterSeconds = %d, want >= 1", busy.RetryAfterSeconds())
	}
	l2.Release()
	l3, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l3.Release()
	l1.Release()
}

// POOL-010: queue-wait timeout → 429 pool_busy; the waiter is released and
// the slot accounting is intact afterwards (SDD §4.4).
func TestQueueWaitTimeout(t *testing.T) {
	p, _ := newPool(1, Options{QueueWait: 80 * time.Millisecond}, nil, nil)
	l1, _ := p.Acquire(context.Background())
	l2, _ := p.Acquire(context.Background())

	start := time.Now()
	_, err := p.Acquire(context.Background())
	elapsed := time.Since(start)
	var busy *ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if elapsed < 70*time.Millisecond {
		t.Errorf("busy returned after %v, want ~queue wait", elapsed)
	}

	// Waiter released: after freeing one slot, acquires work and the
	// semaphore still caps at 2.
	l1.Release()
	l3, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_, err = p.Acquire(context.Background())
	if !errors.As(err, &busy) {
		t.Errorf("post-timeout cap: err = %v, want ErrBusy (no slot leak)", err)
	}
	l3.Release()
	l2.Release()
}

// POOL-011: empty pool or all-disabled pool → 503 no_accounts (SDD §7.4).
func TestNoAccounts(t *testing.T) {
	p, _ := newPool(0, Options{}, nil, nil)
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("empty pool: err = %v, want ErrNoAccounts", err)
	}
	p2 := New([]accounts.Account{{ID: "x", Enabled: false}}, Options{Now: testutil.NewFakeClock().Now}, nil, nil)
	if _, err := p2.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("all-disabled pool: err = %v, want ErrNoAccounts", err)
	}
}

// POOL-012: 451 disables with "account restricted"; 461 disables
// permanently and is never probed (SDD §4.6).
func TestDisabledStates(t *testing.T) {
	var probed []string
	probe := func(_ context.Context, id string) error {
		probed = append(probed, id)
		return nil
	}
	p, fc := newPool(2, Options{}, probe, nil)

	// 451: restricted, non-permanent.
	p.ReportDisabled("a1", "account restricted", false)
	if st := p.StateOf("a1"); st != StateDisabled {
		t.Fatalf("451 state = %s, want disabled", st)
	}
	_, _, reason := p.RuntimeState("a1")
	if reason != "account restricted" {
		t.Errorf("451 reason = %q", reason)
	}

	// 461: permanent — never probed even after its cooldown would expire.
	p.Report429("a2")
	fc.Advance(61 * time.Second)
	p.ReportDisabled("a2", "permanent block (461)", true)
	p.mu.Lock()
	perm := p.byID["a2"].permanent
	p.mu.Unlock()
	if !perm {
		t.Error("461 must be recorded as permanent")
	}
	if n := p.ProbeSweep(context.Background()); n != 0 {
		t.Errorf("sweep recovered %d, want 0 (disabled accounts never probed)", n)
	}
	for _, id := range probed {
		if id == "a1" || id == "a2" {
			t.Errorf("disabled account %s was probed", id)
		}
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("all-disabled: err = %v, want ErrNoAccounts", err)
	}
}

// POOL-013: cooldown state persists across restart via the accounts file
// (SDD §7.5).
func TestCooldownPersistsAcrossRestart(t *testing.T) {
	p, fc := newPool(2, Options{}, nil, nil)
	p.Report429("a1")
	p.ReportDisabled("a2", "account restricted", false)

	state, until, reason := p.RuntimeState("a1")
	accts := []accounts.Account{
		{ID: "a1", Enabled: true, State: string(state), CooldownUntil: until},
		{ID: "a2", Enabled: true, State: "disabled", DisabledReason: reason},
	}
	p2 := New(accts, Options{Now: fc.Now}, nil, nil)
	if st := p2.StateOf("a1"); st != StateCooldown {
		t.Errorf("restarted a1 state = %s, want cooldown", st)
	}
	if got := p2.CooldownUntilOf("a1"); !got.Equal(time.Unix(until, 0)) {
		t.Errorf("restarted cooldown until = %v, want %v", got, time.Unix(until, 0))
	}
	if st := p2.StateOf("a2"); st != StateDisabled {
		t.Errorf("restarted a2 state = %s, want disabled", st)
	}
	var parked *ErrParked
	if _, err := p2.Acquire(context.Background()); !errors.As(err, &parked) {
		t.Errorf("restarted pool: err = %v, want ErrParked (cooldown honored)", err)
	}
}

// POOL-014: duplicate enabled ids rejected at load (accounts layer).
func TestDuplicateIDsRejected(t *testing.T) {
	f := accounts.File{Accounts: []accounts.Account{
		{ID: "dup", Enabled: true},
		{ID: "dup", Enabled: true},
	}}
	path := filepath.Join(t.TempDir(), "accounts.json")
	data, _ := json.Marshal(f)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := accounts.Load(path)
	var dup *accounts.ErrDuplicateID
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want ErrDuplicateID", err)
	}
	if dup.ID != "dup" {
		t.Errorf("duplicate id = %q, want dup", dup.ID)
	}
}

// POOL-015: selection score prefers idle accounts — a busy account (slot
// held) is skipped in favor of an idle one (SDD §4.4).
func TestSelectionPrefersIdle(t *testing.T) {
	p, _ := newPool(2, Options{}, nil, nil)
	l1, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	l2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	if l2.ID == l1.ID {
		t.Errorf("both acquires landed on %s; second should prefer the idle account", l1.ID)
	}
}

// POOL-016: context cancel while queued returns without holding a slot —
// 100 iterations under -race leave the semaphore intact (TDD §1.5).
func TestContextCancelNoLeak(t *testing.T) {
	p, _ := newPool(1, Options{QueueWait: 10 * time.Second}, nil, nil)
	l1, _ := p.Acquire(context.Background())
	l2, _ := p.Acquire(context.Background())

	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			_, err := p.Acquire(ctx)
			errCh <- err
		}()
		time.Sleep(time.Millisecond) // let the waiter block
		cancel()
		if err := <-errCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: err = %v, want context.Canceled", i, err)
		}
	}

	l1.Release()
	l3, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after cancels: %v (slot leaked?)", err)
	}
	l3.Release()
	l2.Release()
}

// Sticky acquisition: target won when available, fallback otherwise
// (SDD §4.3).
func TestAcquireSticky(t *testing.T) {
	p, _ := newPool(2, Options{}, nil, nil)
	l, ok, err := p.AcquireSticky(context.Background(), "a1")
	if err != nil || !ok || l.ID != "a1" {
		t.Fatalf("sticky hit: lease=%v ok=%v err=%v", l, ok, err)
	}
	l.Release()

	// Unknown target: fallback to a general acquire.
	l2, ok2, err := p.AcquireSticky(context.Background(), "ghost")
	if err != nil || ok2 {
		t.Fatalf("sticky unknown: lease=%v ok=%v err=%v (want fallback)", l2, ok2, err)
	}
	l2.Release()

	// Cooling target: fallback to another account.
	p.Report429("a1")
	l3, ok3, err := p.AcquireSticky(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Release()
	if ok3 {
		t.Error("cooling sticky target must not be won")
	}
	if l3.ID != "a2" {
		t.Errorf("fallback lease = %s, want a2", l3.ID)
	}
}

// Concurrent acquire/release under -race (TDD §1.5).
func TestConcurrentAcquireRelease(t *testing.T) {
	p, _ := newPool(3, Options{QueueWait: 2 * time.Second}, nil, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l, err := p.Acquire(context.Background())
				if err != nil {
					continue // parked/busy under contention is fine here
				}
				p.RecordFirstByte(l.ID, 10*time.Millisecond)
				p.ReportSuccess(l.ID)
				l.Release()
			}
		}()
	}
	wg.Wait()
	s := p.Summarize()
	if s.Total != 3 || s.Available != 3 {
		t.Errorf("post-stress summary = %+v, want 3 available", s)
	}
}
