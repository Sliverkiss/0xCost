package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// Additional coverage for internal/pool (TDD §6.2 floor ≥85%).
// ============================================================================

// The exported error types must render usable messages, clamp Retry-After to
// at least 1s, and round-trip through errors.As / errors.Is so the API layer
// can map them.
func TestErrorTypesAndUnwrap(t *testing.T) {
	t.Run("ErrBusy", func(t *testing.T) {
		e := &ErrBusy{RetryAfter: 3 * time.Second}
		if !strings.Contains(e.Error(), "3s") {
			t.Errorf("Error() = %q", e.Error())
		}
		if e.RetryAfterSeconds() != 3 {
			t.Errorf("RetryAfterSeconds = %d, want 3", e.RetryAfterSeconds())
		}
	})

	t.Run("errors.As reaches ErrBusy from a real acquire", func(t *testing.T) {
		p, _ := newPool(1, Options{QueueWait: 20 * time.Millisecond})
		l1, _ := p.Acquire(context.Background())
		l2, _ := p.Acquire(context.Background())
		_, err := p.Acquire(context.Background())
		var busy *ErrBusy
		if !errors.As(err, &busy) {
			t.Fatalf("err = %v, want *ErrBusy", err)
		}
		if busy.RetryAfterSeconds() < 1 {
			t.Errorf("RetryAfterSeconds = %d, want >= 1", busy.RetryAfterSeconds())
		}
		l1.Release()
		l2.Release()
	})
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
// cooldown gone there are exactly two buckets (owner ruling 2026-10): a 429
// leaves the account in the available bucket.
func TestSummarizeStates(t *testing.T) {
	p, _ := newPool(3, Options{})
	p.Report429("a1")
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
	st, reason := p.RuntimeState("ghost")
	if st != StateDisabled || reason != "" {
		t.Errorf("RuntimeState(ghost) = (%s,%q), want zero", st, reason)
	}
}

// Release is defensive: nil receivers, unknown ids, and double-release must be
// safe and must not corrupt slot accounting.
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
