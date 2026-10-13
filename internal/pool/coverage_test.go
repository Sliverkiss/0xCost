package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ============================================================================
// Additional coverage for internal/pool (TDD §6.2 floor ≥85%).
// ============================================================================

// ErrNoAccounts is the pool's only refusal, and it maps cleanly through
// errors.Is so the API layer can turn it into 503 no_accounts.
func TestErrorTypesAndUnwrap(t *testing.T) {
	p, _ := newPool(1, Options{})
	if _, err := p.Acquire(context.Background(), "a1"); !errors.Is(err, ErrNoAccounts) {
		t.Errorf("err = %v, want ErrNoAccounts", err)
	}
}

// Enable clears disabled state and makes the account selectable again;
// unknown ids are a no-op.
func TestEnable(t *testing.T) {
	t.Run("clears disabled state", func(t *testing.T) {
		p, _ := newPool(1, Options{})
		p.ReportDisabled("a1", "account restricted", false)
		if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrNoAccounts) {
			t.Fatalf("disabled-only pool: err = %v, want ErrNoAccounts", err)
		}
		p.Enable("a1")
		if st := p.StateOf("a1"); st != StateAvailable {
			t.Fatalf("state after Enable = %s, want available", st)
		}
		l, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire after Enable: %v", err)
		}
		l.Release()
	})

	t.Run("unknown id is a no-op", func(t *testing.T) {
		p, _ := newPool(1, Options{})
		p.Enable("ghost") // must not panic
		if st := p.StateOf("a1"); st != StateAvailable {
			t.Errorf("unrelated account changed: %s", st)
		}
	})
}

// IDs returns every member in stable order (introspection).
func TestIDs(t *testing.T) {
	p, _ := newPool(3, Options{})
	if got := strings.Join(p.IDs(), ","); got != "a1,a2,a3" {
		t.Errorf("IDs = %q, want a1,a2,a3", got)
	}
}

// Summarize counts each state bucket exactly once (healthz contract). With
// cooldown gone there are exactly two buckets (owner ruling 2026-10).
func TestSummarizeStates(t *testing.T) {
	p, _ := newPool(3, Options{})
	p.ReportDisabled("a2", "restricted", false)
	s := p.Summarize()
	if s.Total != 3 || s.Available != 2 || s.Disabled != 1 {
		t.Errorf("summary = %+v, want total3 avail2 disabled1", s)
	}
}

// Accessors for an unknown account return zero values, not panics.
func TestUnknownAccountAccessors(t *testing.T) {
	p, _ := newPool(1, Options{})
	if st := p.StateOf("ghost"); st != StateDisabled {
		t.Errorf("StateOf(ghost) = %s, want disabled", st)
	}
}

// Release is defensive: nil receivers, unknown ids, and double-release must be
// safe and must never make a later Acquire fail.
func TestReleaseDefensive(t *testing.T) {
	p, _ := newPool(1, Options{})

	var nilLease *Lease
	nilLease.Release() // nil receiver: no panic

	(&Lease{Pool: p, ID: "ghost"}).Release() // unknown id: no-op

	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l.Release() // double release must not block or panic

	// Accounting intact: two sequential acquires still succeed.
	for i := 0; i < 2; i++ {
		li, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d after double-release: %v", i, err)
		}
		li.Release()
	}
}
