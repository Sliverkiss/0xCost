package pool

// ① Failure attribution (TDD §3.5.2 POOLATTR-*, spec docs/specs/account-pool.md §4,
// SDD §4.5.1).
//
// The pool used to have one counter and one ladder for 5xx, transport failures and
// stalls alike ("Transport errors are treated the same", pool.go:392): a local
// network blip escalated an account's cooldown exactly like an upstream 5xx, and
// nothing recorded which it had been. These cases pin the split: each class owns
// its counter and its ladder, no class escalates another, the cause of a cooldown
// is recorded, and the upstream's own Retry-After is never consulted.

import (
	"testing"
	"time"
)

// POOLATTR-001: a transport failure starts its own ladder. Three 5xx failures put
// the 5xx ladder at 120s; the transport failure that follows must cool for its own
// base (15s), not for the 5xx ladder's current step, and must not touch the 5xx
// counter.
func TestTransportFailureHasOwnLadder(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	for i := 0; i < 3; i++ {
		p.Report5xx("a1")
	}
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 120*time.Second {
		t.Fatalf("after three 5xx: cooldown = %v, want 120s", got)
	}
	p.ReportTransport("a1")
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 15*time.Second {
		t.Errorf("transport cooldown = %v, want 15s (its own base, not the 5xx ladder)", got)
	}
	f := p.FailuresOf("a1")
	if f[ClassTransport] != 1 {
		t.Errorf("transport failures = %d, want 1", f[ClassTransport])
	}
	if f[ClassUpstream5xx] != 3 {
		t.Errorf("5xx failures = %d, want 3 (unchanged by the transport failure)", f[ClassUpstream5xx])
	}
}

// POOLATTR-002: counters are per class — one failure of each kind leaves each
// counter at 1, so no class can escalate another by accumulation.
func TestFailureCountersArePerClass(t *testing.T) {
	p, _ := newPool(1, Options{}, nil, nil)
	p.Report5xx("a1")
	p.ReportTransport("a1")
	p.Report429("a1")
	f := p.FailuresOf("a1")
	for _, class := range []FailureClass{ClassTransport, ClassUpstream5xx, ClassThrottle429} {
		if f[class] != 1 {
			t.Errorf("class %s = %d, want 1", class, f[class])
		}
	}
	if len(f) != 3 {
		t.Errorf("FailuresOf reported %d classes, want the three known ones", len(f))
	}
}

// POOLATTR-003: the transport ladder doubles on its own and caps at 5 minutes
// (15/30/60/…/300), never at the 10-minute shared cap.
func TestTransportLadderAndCap(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	ladder := []time.Duration{15, 30, 60, 120, 240, 300, 300}
	for i, want := range ladder {
		p.ReportTransport("a1")
		if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != want*time.Second {
			t.Errorf("transport ladder step %d: cooldown = %v, want %v", i+1, got, want*time.Second)
		}
	}
}

// POOLATTR-004: the existing ladders are unchanged — 429 keeps 60s → 10min and
// 5xx keeps 30s → 10min (POOL-003/005 must keep passing).
func TestExistingLaddersUnchanged(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	for i, want := range []time.Duration{60, 120, 240} {
		p.Report429("a1")
		if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != want*time.Second {
			t.Errorf("429 step %d = %v, want %v", i+1, got, want*time.Second)
		}
	}
	p2, fc2 := newPool(1, Options{}, nil, nil)
	for i, want := range []time.Duration{30, 60, 120} {
		p2.Report5xx("a1")
		if got := p2.CooldownUntilOf("a1").Sub(fc2.Now()); got != want*time.Second {
			t.Errorf("5xx step %d = %v, want %v", i+1, got, want*time.Second)
		}
	}
}

// POOLATTR-005: success clears every class counter, so the next failure of any
// class starts at that class's base again.
func TestSuccessClearsEveryClass(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	p.Report5xx("a1")
	p.ReportTransport("a1")
	p.Report429("a1")
	p.ReportSuccess("a1")
	for class, n := range p.FailuresOf("a1") {
		if n != 0 {
			t.Errorf("class %s = %d after success, want 0", class, n)
		}
	}
	_ = fc
	p.ReportTransport("a1")
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 15*time.Second {
		t.Errorf("post-success transport cooldown = %v, want 15s (base again)", got)
	}
}

// POOLATTR-006: a successful renewal clears the failure counters but must not
// release a cooldown — cooldowns are time-owned, and clearing them here would hand
// an unhealthy account straight back (the reference's resetFailures lesson).
func TestRenewalResetClearsCountersNotCooldown(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	p.Report5xx("a1")
	p.ReportTransport("a1")
	until := p.CooldownUntilOf("a1")
	p.ResetFailures("a1")
	for class, n := range p.FailuresOf("a1") {
		if n != 0 {
			t.Errorf("class %s = %d after renewal reset, want 0", class, n)
		}
	}
	if got := p.CooldownUntilOf("a1"); !got.Equal(until) {
		t.Errorf("cooldown moved by a renewal reset: %v → %v (time-owned, must not move)", until, got)
	}
	// The last report was a transport failure, so the live cooldown is its 15s
	// step — the point here is that the reset did not move it at all.
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 15*time.Second {
		t.Errorf("cooldown = %v, want the live 15s transport step", got)
	}
}

// POOLATTR-007: a cooldown records its cause — class, upstream status and when —
// so an operator can see why an account is cooling (REQ-5, rendered by §4.9).
func TestCooldownRecordsItsCause(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	p.ReportFailure("a1", ClassUpstream5xx, 503)
	class, status, since := p.CooldownInfoOf("a1")
	if class != ClassUpstream5xx {
		t.Errorf("class = %q, want %q", class, ClassUpstream5xx)
	}
	if status != 503 {
		t.Errorf("status = %d, want 503 (the upstream's own status)", status)
	}
	if !since.Equal(fc.Now()) {
		t.Errorf("since = %v, want %v", since, fc.Now())
	}
	p.ReportTransport("a1")
	class, status, _ = p.CooldownInfoOf("a1")
	if class != ClassTransport || status != 0 {
		t.Errorf("after a transport failure: class=%q status=%d, want transport/0 (the latest cause)", class, status)
	}
}

// POOLATTR-008: the upstream's own Retry-After is never consulted. The pool's API
// takes no duration from a response, so a 429 carrying "Retry-After: 3000" must
// still cool for the ladder's 60s (owner decision, spec §10.2).
func TestLadderIgnoresUpstreamRetryAfter(t *testing.T) {
	p, fc := newPool(1, Options{}, nil, nil)
	p.ReportFailure("a1", ClassThrottle429, 429)
	if got := p.CooldownUntilOf("a1").Sub(fc.Now()); got != 60*time.Second {
		t.Errorf("cooldown = %v, want the 60s ladder step regardless of any Retry-After", got)
	}
}
