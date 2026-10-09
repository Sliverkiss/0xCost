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
	t.Run("ErrParked", func(t *testing.T) {
		e := &ErrParked{RetryAfter: 90 * time.Second}
		if !strings.Contains(e.Error(), "1m30s") {
			t.Errorf("Error() = %q, want nearest recovery in 1m30s", e.Error())
		}
		if e.RetryAfterSeconds() != 90 {
			t.Errorf("RetryAfterSeconds = %d, want 90", e.RetryAfterSeconds())
		}
		if got := (&ErrParked{RetryAfter: 200 * time.Millisecond}).RetryAfterSeconds(); got != 1 {
			t.Errorf("sub-second Retry-After = %d, want clamped 1", got)
		}
	})

	t.Run("ErrBusy", func(t *testing.T) {
		e := &ErrBusy{RetryAfter: 3 * time.Second}
		if !strings.Contains(e.Error(), "3s") {
			t.Errorf("Error() = %q", e.Error())
		}
		if e.RetryAfterSeconds() != 3 {
			t.Errorf("RetryAfterSeconds = %d, want 3", e.RetryAfterSeconds())
		}
	})

	t.Run("errors.As reaches both types from a real acquire", func(t *testing.T) {
		p, _ := newPool(1, Options{QueueWait: 20 * time.Millisecond}, nil, nil)
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

		p1, _ := newPool(1, Options{}, nil, nil)
		p1.Report429("a1")
		_, err = p1.Acquire(context.Background())
		var parked *ErrParked
		if !errors.As(err, &parked) {
			t.Fatalf("err = %v, want *ErrParked", err)
		}
	})

	t.Run("authError unwraps to the inner error", func(t *testing.T) {
		inner := errors.New("401 unauthorized")
		ae := AuthError(inner)
		if ae.Error() != "auth expired" {
			t.Errorf("Error() = %q", ae.Error())
		}
		if !errors.Is(ae, inner) {
			t.Error("Unwrap must expose the inner error to errors.Is")
		}
		var marker interface{ IsAuthExpired() bool }
		if !errors.As(ae, &marker) || !marker.IsAuthExpired() {
			t.Errorf("errors.As must reach the IsAuthExpired marker on %v", ae)
		}
		if !isAuthErr(ae) {
			t.Error("isAuthErr must recognize an AuthError")
		}
		if isAuthErr(errors.New("plain")) {
			t.Error("isAuthErr must reject a plain error")
		}
	})
}

// Enable clears both disabled and cooldown state, resets the failure ladder,
// and makes the account selectable again; unknown ids are a no-op.
func TestEnable(t *testing.T) {
	t.Run("clears disabled state", func(t *testing.T) {
		p, _ := newPool(1, Options{}, nil, nil)
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

	t.Run("clears cooldown and resets the ladder", func(t *testing.T) {
		p, fc := newPool(1, Options{}, nil, nil)
		p.Report429("a1")
		p.Report429("a1") // streak 2 → 120s
		p.Enable("a1")
		if st := p.StateOf("a1"); st != StateAvailable {
			t.Fatalf("state = %s, want available", st)
		}
		if got := p.CooldownUntilOf("a1"); !got.IsZero() {
			t.Errorf("cooldown not cleared: %v", got)
		}
		p.mu.Lock()
		fs, perm := p.byID["a1"].failStreak, p.byID["a1"].permanent
		p.mu.Unlock()
		if fs != 0 || perm {
			t.Errorf("after Enable: failStreak=%d permanent=%v, want 0/false", fs, perm)
		}
		// Ladder restarted: the next 429 cools for the base 60s, not 240s.
		p.Report429("a1")
		if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 60*time.Second {
			t.Errorf("post-Enable 429 cooldown = %v, want 60s (ladder reset)", got)
		}
	})

	t.Run("unknown id is a no-op", func(t *testing.T) {
		p, _ := newPool(1, Options{}, nil, nil)
		p.Enable("ghost") // must not panic
		if st := p.StateOf("a1"); st != StateAvailable {
			t.Errorf("unrelated account changed: %s", st)
		}
	})
}

// IDs returns every member in stable order (introspection).
func TestIDs(t *testing.T) {
	p, _ := newPool(3, Options{}, nil, nil)
	if got := strings.Join(p.IDs(), ","); got != "a1,a2,a3" {
		t.Errorf("IDs = %q, want a1,a2,a3", got)
	}
}

// Summarize counts each state bucket exactly once (healthz contract).
func TestSummarizeStates(t *testing.T) {
	p, _ := newPool(3, Options{}, nil, nil)
	p.Report429("a1")
	p.ReportDisabled("a2", "restricted", false)
	s := p.Summarize()
	if s.Total != 3 || s.Available != 1 || s.Cooldown != 1 || s.Disabled != 1 {
		t.Errorf("summary = %+v, want total3 avail1 cool1 disabled1", s)
	}
}

// Accessors for an unknown account return zero values, not panics.
func TestUnknownAccountAccessors(t *testing.T) {
	p, _ := newPool(1, Options{}, nil, nil)
	if st := p.StateOf("ghost"); st != StateDisabled {
		t.Errorf("StateOf(ghost) = %s, want disabled", st)
	}
	if got := p.CooldownUntilOf("ghost"); !got.IsZero() {
		t.Errorf("CooldownUntilOf(ghost) = %v, want zero", got)
	}
	st, until, reason := p.RuntimeState("ghost")
	if st != StateDisabled || until != 0 || reason != "" {
		t.Errorf("RuntimeState(ghost) = (%s,%d,%q), want zero", st, until, reason)
	}
}

// Release is defensive: nil receivers, unknown ids, and double-release must be
// safe and must not corrupt slot accounting.
func TestReleaseDefensive(t *testing.T) {
	p, _ := newPool(1, Options{}, nil, nil)

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
