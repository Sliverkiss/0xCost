package session

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
	"github.com/yg/mimo2api/internal/testutil"
)

func newTestStore(t *testing.T) (*Store, *testutil.FakeClock) {
	t.Helper()
	fc := testutil.NewFakeClock()
	s := New(Options{TTL: 30 * time.Minute, Max: 500, Now: fc.Now})
	return s, fc
}

func msgs(pairs ...[2]string) []adapter.Msg {
	var out []adapter.Msg
	for _, p := range pairs {
		out = append(out, adapter.Msg{Role: p[0], Content: p[1]})
	}
	return out
}

// save stores a slot whose frontier is exactly the given history — what
// finishTurn does after a completed turn.
func save(s *Store, key, account, conv, dialog string, frontier []adapter.Msg) {
	s.Save(Entry{
		SessionKey:       key,
		AccountID:        account,
		ConversationID:   conv,
		PreviousDialogID: dialog,
		FrontierHash:     adapter.FrontierHash(frontier),
		MsgCount:         adapter.CountNonSystem(frontier),
	})
}

func turn(n int) []adapter.Msg {
	out := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	for i := 2; i <= n; i++ {
		out = append(out, adapter.Msg{Role: "user", Content: "q" + strconv.Itoa(i)})
		out = append(out, adapter.Msg{Role: "assistant", Content: "a" + strconv.Itoa(i)})
	}
	return out
}

// STICKY-002 (keyed): a verified continuation returns the stored slot.
func TestLookupContinuationMatch(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "acc1", "conv1", "dlg1", turn(1))
	got, ok := s.Lookup("key-1", turn(2))
	if !ok {
		t.Fatal("expected hit on continuation")
	}
	if got.AccountID != "acc1" || got.ConversationID != "conv1" || got.PreviousDialogID != "dlg1" {
		t.Errorf("hit = %+v", got)
	}
}

// STICKY-003: replay guard — identical history is a miss.
func TestLookupReplayGuard(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "acc1", "conv1", "dlg1", turn(1))
	if _, ok := s.Lookup("key-1", turn(1)); ok {
		t.Error("identical history must be a miss (regeneration, not continuation)")
	}
}

// STICKY-004 is retired (ADR 0009): with a session-keyed index there is no
// "multiple matches → newest wins" — two sessions are simply two entries. What
// replaces it is that entries never leak across keys.
func TestLookupIsScopedToItsKey(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "acc1", "conv1", "dlg1", turn(1))
	if _, ok := s.Lookup("key-2", turn(2)); ok {
		t.Error("a different session key must not see key-1's entry")
	}
	got, ok := s.Lookup("key-1", turn(2))
	if !ok || got.AccountID != "acc1" {
		t.Errorf("key-1 lookup = %+v ok=%v", got, ok)
	}
}

// SESS-011: verification over the whole frontier — a stored frontier that is
// not a prefix of this history is a miss, even under the right key.
func TestSess011FrontierMismatchIsAMiss(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "acc1", "conv1", "dlg1", turn(1))
	other := msgs([2]string{"user", "different"}, [2]string{"assistant", "a1"},
		[2]string{"user", "q2"}, [2]string{"assistant", "a2"})
	if _, ok := s.Lookup("key-1", other); ok {
		t.Error("a different history must not chain, even with the same length")
	}
}

// SESS-012: editing history near the start invalidates the session — the old
// 5-message/200-rune window would have matched a lookalike.
func TestSess012EditedEarlyTurnIsAMiss(t *testing.T) {
	s, _ := newTestStore(t)
	long := make([]adapter.Msg, 0, 12)
	long = append(long, adapter.Msg{Role: "user", Content: "original first question"})
	long = append(long, adapter.Msg{Role: "assistant", Content: "first answer"})
	for i := 2; i <= 6; i++ {
		long = append(long, adapter.Msg{Role: "user", Content: "q" + strconv.Itoa(i)})
		long = append(long, adapter.Msg{Role: "assistant", Content: "a" + strconv.Itoa(i)})
	}
	save(s, "key-1", "acc1", "conv1", "dlg1", long)

	edited := append([]adapter.Msg{{Role: "user", Content: "EDITED first question"}}, long[1:]...)
	edited = append(edited, adapter.Msg{Role: "user", Content: "q7"})
	if _, ok := s.Lookup("key-1", edited); ok {
		t.Error("editing the first turn must force a replay, not a chain")
	}
}

// SESS-053: lookup cost is independent of how many sessions are stored. A
// content-scanning implementation (the design this replaces) grows with the
// store; a keyed one computes at most two frontier hashes.
func TestSess053LookupCostIsConstant(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 500; i++ {
		key := "key-" + strconv.Itoa(i)
		save(s, key, "acc"+strconv.Itoa(i), "conv"+strconv.Itoa(i), "dlg", turn(1))
	}
	save(s, "target", "acc-t", "conv-t", "dlg-t", turn(1))
	before := s.HashOps()
	if _, ok := s.Lookup("target", turn(2)); !ok {
		t.Fatal("expected a hit on the target session")
	}
	if ops := s.HashOps() - before; ops > 2 {
		t.Errorf("lookup computed %d frontier hashes with 500 sessions stored, want ≤2 (a scan would scale)", ops)
	}
}

// STICKY-006: TTL expiry — entry expires after idle TTL.
func TestTTLExpiry(t *testing.T) {
	s, fc := newTestStore(t)
	save(s, "key-1", "a", "c", "d", turn(1))
	fc.Advance(31 * time.Minute)
	if _, ok := s.Lookup("key-1", turn(2)); ok {
		t.Error("expired entry must not hit")
	}
	if n := s.Sweep(); n != 1 {
		t.Errorf("Sweep removed %d, want 1", n)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d after sweep, want 0", s.Len())
	}
}

// STICKY-012: idle-based (sliding) TTL — activity slides UpdatedAt forward.
func TestSlidingTTL(t *testing.T) {
	s, fc := newTestStore(t)
	save(s, "key-1", "a", "c", "d", turn(1))
	fc.Advance(29 * time.Minute)
	if _, ok := s.Lookup("key-1", turn(2)); !ok {
		t.Fatal("expected hit at age 29m")
	}
	// The hit slid updated_at forward; advancing another 2m must stay inside TTL.
	fc.Advance(2 * time.Minute)
	if _, ok := s.Lookup("key-1", turn(3)); !ok {
		t.Error("expected hit after activity slid TTL forward (29m + 2m)")
	}
}

// STICKY-007: LRU-500 eviction — oldest UpdatedAt evicted at 501 inserts.
func TestLRUEviction(t *testing.T) {
	fc := testutil.NewFakeClock()
	s := New(Options{TTL: time.Hour, Max: 500, Now: fc.Now})
	for i := 0; i < 501; i++ {
		save(s, "key-"+strconv.Itoa(i), "a", "c"+strconv.Itoa(i), "d", turn(1))
		fc.Advance(time.Second)
	}
	if s.Len() != 500 {
		t.Fatalf("Len = %d, want 500", s.Len())
	}
	if _, ok := s.Lookup("key-0", turn(2)); ok {
		t.Error("evicted entry must not hit")
	}
	if _, ok := s.Lookup("key-500", turn(2)); !ok {
		t.Error("newest entry must survive eviction")
	}
}

// STICKY-013: persistence round-trip.
func TestPersistenceRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "a", "c", "d", turn(1))
	snap := s.Snapshot()

	s2, _ := newTestStore(t)
	s2.Restore(snap)
	if s2.Len() != 1 {
		t.Fatalf("restored Len = %d", s2.Len())
	}
	got, ok := s2.Lookup("key-1", turn(2))
	if !ok || got.ConversationID != "c" || got.PreviousDialogID != "d" {
		t.Errorf("restored lookup = %+v ok=%v", got, ok)
	}
}

// Delete removes the slot so subsequent lookups miss.
func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "a", "c", "d", turn(1))
	s.Delete("key-1")
	if _, ok := s.Lookup("key-1", turn(2)); ok {
		t.Error("deleted entry must not hit")
	}
}

// UpdateDialog advances the chaining anchor and refreshes activity.
func TestUpdateDialog(t *testing.T) {
	s, fc := newTestStore(t)
	save(s, "key-1", "a", "c", "d1", turn(1))
	fc.Advance(29 * time.Minute)
	s.UpdateDialog("key-1", "dlg-2", turn(1))
	fc.Advance(29 * time.Minute) // 58m total, but UpdateDialog slid updated_at
	got, ok := s.Lookup("key-1", turn(2))
	if !ok || got.PreviousDialogID != "dlg-2" {
		t.Errorf("lookup after UpdateDialog = %+v ok=%v", got, ok)
	}
}

// The frontier advances when a turn starts, so a client that disconnects after
// its message was consumed does not re-send it next time (STICKY-010).
func TestLookupAdvancesTheFrontier(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "a", "c", "d", turn(1))
	if _, ok := s.Lookup("key-1", turn(2)); !ok {
		t.Fatal("expected a hit")
	}
	e, _ := s.Get("key-1")
	if e.MsgCount != adapter.CountNonSystem(turn(2)) {
		t.Errorf("stored frontier count = %d, want %d", e.MsgCount, adapter.CountNonSystem(turn(2)))
	}
	if e.FrontierHash != adapter.FrontierHash(turn(2)) {
		t.Error("stored frontier hash did not advance to the consumed history")
	}
}

// SESS-020/021 at the store level: one holder at a time, waiters either get the
// session or give up at the bound (the fork decision is the caller's).
func TestAcquireSerialisesOneSession(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if held, waited := s.Acquire(ctx, "key-1", time.Second); !held || waited {
		t.Fatalf("first acquire = held=%v waited=%v, want a free session", held, waited)
	}
	if held, _ := s.Acquire(ctx, "key-1", 0); held {
		t.Error("second acquire must not hold the session while the first does")
	}
	if s.InFlight("key-1") != 1 {
		t.Errorf("in-flight = %d, want 1", s.InFlight("key-1"))
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.Release("key-1")
	}()
	held, waited := s.Acquire(ctx, "key-1", time.Second)
	if !held || !waited {
		t.Errorf("waiter = held=%v waited=%v, want it to acquire after the release", held, waited)
	}
	s.Release("key-1")

	if held, _ := s.Acquire(ctx, "key-1", 0); !held {
		t.Fatal("expected to hold the now-free session")
	}
	if held, waited := s.Acquire(ctx, "key-1", 20*time.Millisecond); held || !waited {
		t.Errorf("Acquire with a held session and a short bound = held=%v waited=%v, want a fork decision", held, waited)
	}
	s.Release("key-1")
}

// Concurrent access under -race (TDD §1.5).
func TestConcurrentAccess(t *testing.T) {
	s, _ := newTestStore(t)
	save(s, "key-1", "a", "c", "d", turn(1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			s.Lookup("key-1", turn(2))
			s.Len()
			s.Sweep()
			s.Acquire(context.Background(), "key-1", 0)
			s.Release("key-1")
		}
	}()
	<-done
}
