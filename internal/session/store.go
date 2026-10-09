// Package session implements session identity and the sticky store: a resolved
// session key → {accountID, conversationID, previousDialogID} with frontier
// verification, idle TTL, LRU cap and per-session in-flight accounting
// (SDD §4.3/§4.3.1, spec docs/specs/session-identity.md, ADR 0009).
//
// Two properties are deliberate and load-bearing:
//
//   - Reuse is gated on verification of the whole stored frontier, not on a key
//     match, so a colliding key costs a rebuild and can never hand a caller
//     another conversation's chain (INV-1).
//   - No message content is stored: only ids, counts, an opaque fingerprint and
//     timestamps (INV-3).
package session

import (
	"context"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// Entry is a stored session slot (SDD §4.3.1).
type Entry struct {
	SessionKey       string `json:"session_key"`
	AccountID        string `json:"account_id"`
	ConversationID   string `json:"conversation_id"`
	PreviousDialogID string `json:"previous_dialog_id"`
	FrontierHash     string `json:"frontier_hash"` // canonical hash of the consumed frontier
	MsgCount         int    `json:"msg_count"`     // non-system messages consumed (replay guard)
	CreatedAt        int64  `json:"created_at"`    // unix seconds
	UpdatedAt        int64  `json:"updated_at"`    // unix seconds (idle TTL anchor)
}

// Store is the in-process session store. Own mutex; TTL + LRU enforced lazily
// on access plus via an explicit Sweep for the background janitor.
type Store struct {
	mu       sync.Mutex
	entries  map[string]*Entry // key: session key
	inFlight map[string]int    // key: session key → in-flight streaming requests
	ttl      time.Duration
	max      int
	now      func() time.Time
	lookups  int // total Lookup calls (test instrument, TDD NOSTICK-003/SESS-030)
	hashOps  int // frontier hashes computed by Lookup (SESS-053: must not grow with store size)
}

// Options configures a Store.
type Options struct {
	TTL time.Duration // idle TTL (default 30m, SDD §4.3)
	Max int           // LRU cap (default 500, SDD §4.3)
	Now func() time.Time
}

// New creates a store. Zero-value options take SDD defaults.
func New(opts Options) *Store {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Minute
	}
	if opts.Max <= 0 {
		opts.Max = 500
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{
		entries:  make(map[string]*Entry),
		inFlight: make(map[string]int),
		ttl:      opts.TTL,
		max:      opts.Max,
		now:      opts.Now,
	}
}

// Lookup finds the verified session slot for a request.
//
// A hit requires all three (SDD §4.3.1, INV-1):
//   - a stored entry under this session key that has not expired,
//   - the entry's frontier to be a prefix of the current history — verified by
//     hashing the stored number of non-system messages and comparing, so an
//     edited early turn is a miss rather than a lookalike match,
//   - the non-system message count to have strictly increased (replay guard,
//     TDD STICKY-003).
//
// On a hit the idle TTL slides and the entry's frontier advances to the current
// history: a turn that started upstream has consumed its messages even if the
// client later disconnects (TDD STICKY-010). The returned copy carries the
// PRE-advance frontier so the caller can plan the delta tail.
func (s *Store) Lookup(sessionKey string, msgs []adapter.Msg) (Entry, bool) {
	s.mu.Lock()
	s.lookups++
	now := s.now()
	e, ok := s.entries[sessionKey]
	if !ok || expired(e, now, s.ttl) {
		s.mu.Unlock()
		return Entry{}, false
	}
	non := adapter.CountNonSystem(msgs)
	if non <= e.MsgCount || e.FrontierHash == "" {
		s.mu.Unlock()
		return Entry{}, false
	}
	s.hashOps++
	if adapter.FrontierHash(headNonSystem(msgs, e.MsgCount)) != e.FrontierHash {
		s.mu.Unlock()
		return Entry{}, false
	}
	cp := *e
	s.hashOps++
	e.FrontierHash = adapter.FrontierHash(msgs)
	e.MsgCount = non
	e.UpdatedAt = now.Unix()
	s.mu.Unlock()
	return cp, true
}

// headNonSystem returns the first n non-system messages (the frontier the entry
// claims to have consumed).
func headNonSystem(msgs []adapter.Msg, n int) []adapter.Msg {
	out := make([]adapter.Msg, 0, n)
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		out = append(out, m)
		if len(out) == n {
			break
		}
	}
	return out
}

func expired(e *Entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(time.Unix(e.UpdatedAt, 0)) > ttl
}

// Save stores (or overwrites) a session slot.
func (s *Store) Save(e Entry) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.CreatedAt == 0 {
		e.CreatedAt = now.Unix()
	}
	e.UpdatedAt = now.Unix()
	cp := e
	s.entries[e.SessionKey] = &cp
	s.evictLocked()
}

// UpdateDialog advances the chaining anchor and the frontier after a completed
// turn. No-op when the slot is missing (e.g. mid-stream failure deleted it).
func (s *Store) UpdateDialog(sessionKey, dialogID string, msgs []adapter.Msg) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[sessionKey]
	if !ok {
		return
	}
	e.PreviousDialogID = dialogID
	e.FrontierHash = adapter.FrontierHash(msgs)
	e.MsgCount = adapter.CountNonSystem(msgs)
	e.UpdatedAt = now.Unix()
}

// Delete removes a session slot (mid-stream failure / commit-boundary paths,
// SDD §4.3).
func (s *Store) Delete(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, sessionKey)
}

// Acquire claims the session for one streaming request (fork policy, REQ-8).
// It returns whether the caller now holds the session and whether it had to
// wait for it. timeout <= 0 means "do not wait".
func (s *Store) Acquire(ctx context.Context, sessionKey string, timeout time.Duration) (held, waited bool) {
	s.mu.Lock()
	if s.inFlight[sessionKey] == 0 {
		s.inFlight[sessionKey] = 1
		s.mu.Unlock()
		return true, false
	}
	s.mu.Unlock()
	if timeout <= 0 {
		return false, false
	}

	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, true
		case <-tick.C:
			s.mu.Lock()
			if s.inFlight[sessionKey] == 0 {
				s.inFlight[sessionKey] = 1
				s.mu.Unlock()
				return true, true
			}
			s.mu.Unlock()
			if time.Now().After(deadline) {
				return false, true
			}
		}
	}
}

// Release frees a session claimed by Acquire.
func (s *Store) Release(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[sessionKey] > 0 {
		s.inFlight[sessionKey]--
	}
}

// InFlight reports how many streaming requests currently hold the session.
func (s *Store) InFlight(sessionKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight[sessionKey]
}

// Len returns the live entry count (expired entries purged first).
func (s *Store) Len() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	return len(s.entries)
}

// Lookups returns the total number of Lookup calls served (test instrumentation
// for TDD NOSTICK-003/SESS-030: one-shot paths never consult the store).
func (s *Store) Lookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups
}

// HashOps returns how many frontier hashes Lookup has computed in total. One
// lookup costs at most two regardless of how many sessions are stored, which is
// what SESS-053 asserts — a scan-by-content implementation would grow with the
// store instead.
func (s *Store) HashOps() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hashOps
}

// Sweep purges expired entries (background janitor, SDD §3) and returns the
// number removed.
func (s *Store) Sweep() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeExpiredLocked(now)
}

// Get returns a copy of one entry (tests and debug surfaces).
func (s *Store) Get(sessionKey string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[sessionKey]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

func (s *Store) purgeExpiredLocked(now time.Time) int {
	var removed int
	for k, e := range s.entries {
		if expired(e, now, s.ttl) {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// evictLocked enforces the LRU cap: while over cap, evict the entry with the
// oldest UpdatedAt (SDD §4.3).
func (s *Store) evictLocked() {
	for len(s.entries) > s.max {
		var oldestKey string
		var oldest int64 = 1 << 62
		for k, e := range s.entries {
			if e.UpdatedAt < oldest {
				oldest = e.UpdatedAt
				oldestKey = k
			}
		}
		delete(s.entries, oldestKey)
	}
}

// Snapshot returns a copy of all live entries (persistence round-trip, TDD
// STICKY-013).
func (s *Store) Snapshot() []Entry {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, *e)
	}
	return out
}

// Restore loads entries saved by Snapshot into the store.
func (s *Store) Restore(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		cp := e
		s.entries[e.SessionKey] = &cp
	}
	s.evictLocked()
}
