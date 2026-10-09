// Package session implements the sticky-conversation store: conversation
// fingerprint → {accountID, conversationID, previousDialogID} with idle TTL
// and LRU cap (SDD §4.3).
package session

import (
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/adapter"
)

// Entry is a stored sticky-session slot (SDD §4.3).
type Entry struct {
	AccountID        string `json:"account_id"`
	ConversationID   string `json:"conversation_id"`
	PreviousDialogID string `json:"previous_dialog_id"`
	Fingerprint      string `json:"fingerprint"`
	MsgCount         int    `json:"msg_count"`
	CreatedAt        int64  `json:"created_at"` // unix seconds
	UpdatedAt        int64  `json:"updated_at"` // unix seconds (idle TTL anchor)
}

// Store is the in-process sticky-session store. Own mutex; TTL + LRU enforced
// lazily on access plus via an explicit Sweep for the background janitor.
type Store struct {
	mu      sync.Mutex
	entries map[string]*Entry // key: accountID + ":" + conversationID
	ttl     time.Duration
	max     int
	now     func() time.Time
	lookups int // total Lookup calls (test instrument, TDD NOSTICK-003)
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
		entries: make(map[string]*Entry),
		ttl:     opts.TTL,
		max:     opts.Max,
		now:     opts.Now,
	}
}

func key(accountID, conversationID string) string {
	return accountID + ":" + conversationID
}

// Lookup finds the sticky slot for a message list. A hit requires:
//   - the current list is a continuation of the stored fingerprint
//     (adapter.Continuation, SDD §4.2), and
//   - the non-system message count strictly increased (replay guard,
//     TDD STICKY-003), and
//   - the entry has not expired (idle TTL, TDD STICKY-006).
//
// Multiple matches → newest UpdatedAt wins (TDD STICKY-004). A hit slides
// the idle TTL forward (STICKY-012).
func (s *Store) Lookup(msgs []adapter.Msg) (Entry, bool) {
	s.mu.Lock()
	s.lookups++
	s.mu.Unlock()
	fp := adapter.Fingerprint(msgs)
	non := adapter.CountNonSystem(msgs)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	var best *Entry
	for _, e := range s.entries {
		if expired(e, now, s.ttl) {
			continue
		}
		if e.Fingerprint == "" || !adapter.Continuation(msgs, e.Fingerprint) {
			continue
		}
		if non <= e.MsgCount {
			continue // replay guard: identical history is regeneration
		}
		if best == nil || e.UpdatedAt > best.UpdatedAt {
			best = e
		}
	}
	if best == nil {
		return Entry{}, false
	}
	// Slide idle TTL and update the frontier to the current history. The
	// returned copy carries the PRE-advance frontier (what was stored) so
	// callers can compute the delta tail; the store advances so the next
	// request sees the new frontier.
	cp := *best
	best.UpdatedAt = now.Unix()
	best.Fingerprint = fp
	best.MsgCount = non
	return cp, true
}

func expired(e *Entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(time.Unix(e.UpdatedAt, 0)) > ttl
}

// Save stores (or overwrites) a sticky slot.
func (s *Store) Save(e Entry) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.CreatedAt == 0 {
		e.CreatedAt = now.Unix()
	}
	e.UpdatedAt = now.Unix()
	s.entries[key(e.AccountID, e.ConversationID)] = &e
	s.evictLocked()
}

// UpdateDialog advances the chaining anchor after a completed turn.
// No-op when the slot is missing (e.g. mid-stream failure deleted it).
func (s *Store) UpdateDialog(accountID, conversationID, dialogID string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key(accountID, conversationID)]; ok {
		e.PreviousDialogID = dialogID
		e.UpdatedAt = now.Unix()
	}
}

// Delete removes a sticky slot (mid-stream failure / commit-boundary paths,
// SDD §4.3).
func (s *Store) Delete(accountID, conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key(accountID, conversationID))
}

// Len returns the live entry count (expired entries purged first).
func (s *Store) Len() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	return len(s.entries)
}

// Lookups returns the total number of Lookup calls served (test
// instrumentation for TDD NOSTICK-003: non-streaming paths never consult
// the store).
func (s *Store) Lookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups
}

// Sweep purges expired entries (background janitor, SDD §3) and returns the
// number removed.
func (s *Store) Sweep() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeExpiredLocked(now)
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
// oldest UpdatedAt (SDD §4.3, scaled convstore.go:63-79).
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

// Snapshot returns a copy of all live entries (persistence round-trip,
// TDD STICKY-013).
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

// Restore loads entries saved by Snapshot into the store (restart survival,
// SDD §4.3 optional persistence).
func (s *Store) Restore(entries []Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		cp := e
		s.entries[key(e.AccountID, e.ConversationID)] = &cp
	}
	s.evictLocked()
}
