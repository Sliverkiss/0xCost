package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/testutil"
)

// newPool builds an n-account pool with a fake clock (TDD §1.5: no sleeps).
func newPool(n int, opts Options) (*Pool, *testutil.FakeClock) {
	fc := testutil.NewFakeClock()
	opts.Now = fc.Now
	var accts []accounts.Account
	for i := 1; i <= n; i++ {
		accts = append(accts, accounts.Account{ID: fmt.Sprintf("a%d", i), Enabled: true})
	}
	return New(accts, opts), fc
}

// POOL-001: round-robin fairness — 3 accounts, 30 sequential acquires land
// 10±1 on each (SDD §4.4).
func TestRoundRobinFairness(t *testing.T) {
	p, _ := newPool(3, Options{})
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

// PASSFAIL (owner ruling 2026-10): upstream failures never touch pool state.
// A pool that has served traffic keeps every account available — there is no
// code path left that cools an account down.
func TestFailuresKeepAccountAvailable(t *testing.T) {
	p, _ := newPool(2, Options{})
	for i := 0; i < 5; i++ {
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i+1, err)
		}
		l.Release()
	}
	s := p.Summarize()
	if s.Available != 2 || s.Disabled != 0 {
		t.Errorf("summary = %+v, want 2 available 0 disabled", s)
	}
}

// NOSLOT-001 (owner ruling 2026-10-14, "并发槽砍了"): there is no concurrency
// cap anymore. Sequential acquires on a one-account pool all succeed — the
// account is simply selected every time, and Release is a no-op that must
// never make a later Acquire fail.
func TestAcquireHasNoConcurrencyCap(t *testing.T) {
	p, _ := newPool(1, Options{})
	for i := 0; i < 10; i++ {
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v (a second concurrent request must never be refused locally)", i+1, err)
		}
		l.Release()
	}
}

// NOSLOT-002: concurrent acquires all succeed and all name the same (only)
// account — the upstream decides what it will serve, not us.
func TestConcurrentAcquiresAllSucceed(t *testing.T) {
	p, _ := newPool(1, Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if l.ID != "a1" {
				errs <- fmt.Errorf("lease = %s, want a1", l.ID)
			}
			l.Release()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// POOL-011: empty pool or all-disabled pool → 503 no_accounts (SDD §7.4).
func TestNoAccounts(t *testing.T) {
	p, _ := newPool(0, Options{})
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("empty pool: err = %v, want ErrNoAccounts", err)
	}
	p2 := New([]accounts.Account{{ID: "x", Enabled: false}}, Options{Now: testutil.NewFakeClock().Now})
	if _, err := p2.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("all-disabled pool: err = %v, want ErrNoAccounts", err)
	}
}

// POOL-012: 451 disables with "account restricted"; 461 disables
// permanently (SDD §4.6).
func TestDisabledStates(t *testing.T) {
	p, _ := newPool(2, Options{})

	// 451: restricted, non-permanent.
	p.ReportDisabled("a1", "account restricted", false)
	if st := p.StateOf("a1"); st != StateDisabled {
		t.Fatalf("451 state = %s, want disabled", st)
	}
	p.mu.Lock()
	reason := p.byID["a1"].reason
	p.mu.Unlock()
	if reason != "account restricted" {
		t.Errorf("451 reason = %q", reason)
	}

	// 461: permanent.
	p.ReportDisabled("a2", "permanent block (461)", true)
	p.mu.Lock()
	perm := p.byID["a2"].permanent
	p.mu.Unlock()
	if !perm {
		t.Error("461 must be recorded as permanent")
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("all-disabled: err = %v, want ErrNoAccounts", err)
	}
}

// POOL-013: disabled state persists across restart via the accounts file
// (SDD §7.5). With cooldown gone, only disabled survives a restart.
func TestDisabledPersistsAcrossRestart(t *testing.T) {
	p, _ := newPool(2, Options{})
	p.ReportDisabled("a2", "account restricted", false)

	p.mu.Lock()
	reason := p.byID["a2"].reason
	p.mu.Unlock()
	accts := []accounts.Account{
		{ID: "a1", Enabled: true},
		{ID: "a2", Enabled: true, State: "disabled", DisabledReason: reason},
	}
	p2 := New(accts, Options{Now: testutil.NewFakeClock().Now})
	if st := p2.StateOf("a2"); st != StateDisabled {
		t.Errorf("restarted a2 state = %s, want disabled", st)
	}
	if st := p2.StateOf("a1"); st != StateAvailable {
		t.Errorf("restarted a1 state = %s, want available", st)
	}
	if _, err := p2.Acquire(context.Background()); err != nil {
		t.Errorf("restarted pool must still serve via a1: %v", err)
	}
}

// Concurrent acquire/release under -race (TDD §1.5).
func TestConcurrentAcquireRelease(t *testing.T) {
	p, _ := newPool(3, Options{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l, err := p.Acquire(context.Background())
				if err != nil {
					continue
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
