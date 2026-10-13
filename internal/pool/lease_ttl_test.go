package pool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ADR 0019: a lease is capacity for ONE request, and the slot reaper is the
// backstop that stops a wedged request from pinning an account. The request path
// itself bounds each turn with a context deadline; these cases cover the slot
// bookkeeping around it.

// LEASE-TTL-000 (ADR 0036): the slot window moves with upstream activity. A
// lease held far longer than its window but still being touched is NOT reaped —
// "the request is slow" and "the request is wedged" must not look the same to
// the reaper — and the same lease IS reclaimed once the activity stops.
func TestSlotWindowMovesWithActivity(t *testing.T) {
	p, _ := newPool(1, Options{SlotTTL: 40 * time.Millisecond})
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	cancelled := make(chan struct{})
	l.ArmExpiry(func() { close(cancelled) })

	// Four windows of wall clock with activity in every one of them.
	for i := 0; i < 8; i++ {
		time.Sleep(20 * time.Millisecond)
		l.Touch()
		if n := p.ReapExpiredSlots(); n != 0 {
			t.Fatalf("reaped %d slots at sweep %d while the lease was active, want 0", n, i)
		}
	}
	select {
	case <-cancelled:
		t.Fatal("a live lease was aborted")
	default:
	}
	if p.InFlight() != 1 {
		t.Fatalf("in-flight %d after four windows of activity, want 1 (the slot is held)", p.InFlight())
	}

	// Now go quiet: the same lease expires one window later.
	time.Sleep(60 * time.Millisecond)
	if n := p.ReapExpiredSlots(); n != 1 {
		t.Fatalf("reaped %d slots after the activity stopped, want 1", n)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the reaper must abort the upstream call of a silent lease (ArmExpiry)")
	}
}

// LEASE-TTL-001: a slot held past its TTL is force-released, its in-flight
// upstream call is aborted through ArmExpiry, and the account is usable again.
func TestReapExpiredSlotsFreesTheAccount(t *testing.T) {
	p, _ := newPool(1, Options{SlotTTL: 40 * time.Millisecond})
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	cancelled := make(chan struct{})
	l.ArmExpiry(func() { close(cancelled) })

	if n := p.ReapExpiredSlots(); n != 0 {
		t.Fatalf("reaped %d slots before the TTL elapsed, want 0", n)
	}
	if p.InFlight() != 1 {
		t.Fatalf("in-flight %d while the slot is young, want 1", p.InFlight())
	}

	time.Sleep(60 * time.Millisecond)
	if n := p.ReapExpiredSlots(); n != 1 {
		t.Fatalf("reaped %d slots after the TTL, want 1", n)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the reaper must abort the upstream call the slot was holding (ArmExpiry)")
	}
	if p.InFlight() != 0 {
		t.Fatalf("in-flight %d after the reap, want 0", p.InFlight())
	}

	// The account serves again, and the reaped lease's own Release is a no-op.
	l2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after the reap: %v", err)
	}
	if p.InFlight() != 1 {
		t.Fatalf("in-flight %d after re-acquiring, want 1", p.InFlight())
	}
	l2.Release()
	l.Release() // idempotent: must not free a slot it no longer holds
	if p.InFlight() != 0 {
		t.Fatalf("in-flight %d after both releases, want 0", p.InFlight())
	}
}

// LEASE-TTL-002: a normal request releases long before the TTL, and the reaper
// must never touch a lease that is already back.
func TestReaperIgnoresReleasedSlots(t *testing.T) {
	p, _ := newPool(1, Options{SlotTTL: 30 * time.Millisecond})
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	fired := false
	l.ArmExpiry(func() { fired = true })
	l.Release()

	time.Sleep(50 * time.Millisecond)
	if n := p.ReapExpiredSlots(); n != 0 {
		t.Fatalf("reaped %d released slots, want 0", n)
	}
	if fired {
		t.Fatal("the reaper invoked the cancel of a lease that was already released")
	}
}

// LEASE-TTL-003: no TTL configured (TTL=0) means no reaper — the documented
// off-switch, so a deployment cannot accidentally lose capacity accounting.
func TestReaperIsOffWithoutTTL(t *testing.T) {
	p, _ := newPool(1, Options{})
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()
	time.Sleep(20 * time.Millisecond)
	if n := p.ReapExpiredSlots(); n != 0 {
		t.Fatalf("reaped %d slots with no TTL configured, want 0", n)
	}
	if p.InFlight() != 1 {
		t.Fatalf("in-flight %d, want the slot still held", p.InFlight())
	}
}

// LEASE-TTL-004: the reap is reported with the account and the age, because a
// silently reclaimed slot is exactly the kind of failure this project logs (the
// operator has to be able to see that requests are outliving their TTL).
func TestReapLogsTheAccountAndAge(t *testing.T) {
	var logs []string
	p, _ := newPool(1, Options{
		SlotTTL: 20 * time.Millisecond,
		Logf:    func(format string, args ...any) { logs = append(logs, format) },
	})
	if _, err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if n := p.ReapExpiredSlots(); n != 1 {
		t.Fatalf("reaped %d slots, want 1", n)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "reaped slot account=%s") {
		t.Fatalf("reap log = %v, want one line naming the account", logs)
	}
}

// LEASE-TTL-005: a lease carries the TTL it was issued with, so changing the
// configuration cannot retroactively expire (or resurrect) slots already held.
func TestLeaseCarriesItsOwnTTL(t *testing.T) {
	p, _ := newPool(1, Options{SlotTTL: 5 * time.Second})
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()
	if l.ttl != 5*time.Second {
		t.Fatalf("lease ttl = %v, want the pool's SlotTTL", l.ttl)
	}
	if l.issued.IsZero() {
		t.Fatal("lease has no issue time: the reaper cannot age it")
	}
	if l.Released() {
		t.Fatal("a freshly acquired lease reports itself released")
	}
}
