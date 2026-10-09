package session

import (
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

// STICKY-002: continuation match returns the stored slot (SDD §4.2).
func TestLookupContinuationMatch(t *testing.T) {
	s, _ := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{
		AccountID:        "acc1",
		ConversationID:   "conv1",
		PreviousDialogID: "dlg1",
		Fingerprint:      adapter.Fingerprint(t1),
		MsgCount:         2,
	})
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	got, ok := s.Lookup(t2)
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
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{
		AccountID:      "acc1",
		ConversationID: "conv1",
		Fingerprint:    adapter.Fingerprint(t1),
		MsgCount:       2,
	})
	if _, ok := s.Lookup(t1); ok {
		t.Error("identical history must be a miss (regeneration, not continuation)")
	}
}

// STICKY-004: multiple matches → newest UpdatedAt wins.
func TestLookupNewestWins(t *testing.T) {
	s, fc := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "old", ConversationID: "c-old", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	fc.Advance(10 * time.Minute)
	s.Save(Entry{AccountID: "new", ConversationID: "c-new", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	got, ok := s.Lookup(t2)
	if !ok || got.AccountID != "new" {
		t.Errorf("expected newest entry to win, got %+v ok=%v", got, ok)
	}
}

// STICKY-006: TTL expiry — entry expires after idle TTL.
func TestTTLExpiry(t *testing.T) {
	s, fc := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "a", ConversationID: "c", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	fc.Advance(31 * time.Minute)
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	if _, ok := s.Lookup(t2); ok {
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
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "a", ConversationID: "c", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	fc.Advance(29 * time.Minute)
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	if _, ok := s.Lookup(t2); !ok {
		t.Fatal("expected hit at age 29m")
	}
	// The hit slid updated_at forward; advancing another 2m must stay inside TTL.
	fc.Advance(2 * time.Minute)
	t3 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"}, [2]string{"assistant", "a2"}, [2]string{"user", "q3"})
	if _, ok := s.Lookup(t3); !ok {
		t.Error("expected hit after activity slid TTL forward (29m + 2m)")
	}
}

// STICKY-007: LRU-500 eviction — oldest UpdatedAt evicted at 501 inserts.
func TestLRUEviction(t *testing.T) {
	fc := testutil.NewFakeClock()
	s := New(Options{TTL: time.Hour, Max: 500, Now: fc.Now})
	var firstFp []adapter.Msg
	for i := 0; i < 501; i++ {
		m := msgs([2]string{"user", "q" + strconv.Itoa(i)}, [2]string{"assistant", "a" + strconv.Itoa(i)})
		if i == 0 {
			firstFp = m
		}
		s.Save(Entry{
			AccountID:      "a",
			ConversationID: "c" + strconv.Itoa(i),
			Fingerprint:    adapter.Fingerprint(m),
			MsgCount:       2,
		})
		fc.Advance(time.Second)
	}
	if s.Len() != 500 {
		t.Fatalf("Len = %d, want 500", s.Len())
	}
	// The oldest entry (c0) must be gone; a continuation of its history misses.
	ext := append(append([]adapter.Msg{}, firstFp...), adapter.Msg{Role: "user", Content: "next"})
	if _, ok := s.Lookup(ext); ok {
		t.Error("evicted entry must not hit")
	}
	// The newest entry (c500) must still be present.
	last := msgs([2]string{"user", "q500"}, [2]string{"assistant", "a500"}, [2]string{"user", "x"})
	if _, ok := s.Lookup(last); !ok {
		t.Error("newest entry must survive eviction")
	}
}

// STICKY-013: persistence round-trip.
func TestPersistenceRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "a", ConversationID: "c", PreviousDialogID: "d", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	snap := s.Snapshot()

	s2, _ := newTestStore(t)
	s2.Restore(snap)
	if s2.Len() != 1 {
		t.Fatalf("restored Len = %d", s2.Len())
	}
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	got, ok := s2.Lookup(t2)
	if !ok || got.ConversationID != "c" || got.PreviousDialogID != "d" {
		t.Errorf("restored lookup = %+v ok=%v", got, ok)
	}
}

// Delete removes the slot so subsequent lookups miss.
func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "a", ConversationID: "c", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	s.Delete("a", "c")
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	if _, ok := s.Lookup(t2); ok {
		t.Error("deleted entry must not hit")
	}
}

// UpdateDialog advances the chaining anchor and refreshes activity.
func TestUpdateDialog(t *testing.T) {
	s, fc := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "a", ConversationID: "c", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	fc.Advance(29 * time.Minute)
	s.UpdateDialog("a", "c", "dlg-2")
	fc.Advance(29 * time.Minute) // 58m total, but UpdateDialog slid updated_at
	t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
	got, ok := s.Lookup(t2)
	if !ok || got.PreviousDialogID != "dlg-2" {
		t.Errorf("lookup after UpdateDialog = %+v ok=%v", got, ok)
	}
}

// Concurrent access under -race (TDD §1.5).
func TestConcurrentAccess(t *testing.T) {
	s, _ := newTestStore(t)
	t1 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"})
	s.Save(Entry{AccountID: "a", ConversationID: "c", Fingerprint: adapter.Fingerprint(t1), MsgCount: 2})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			t2 := msgs([2]string{"user", "q1"}, [2]string{"assistant", "a1"}, [2]string{"user", "q2"})
			s.Lookup(t2)
			s.Len()
			s.Sweep()
		}
	}()
	<-done
}
