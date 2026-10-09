// Package accounts holds the account pool: the credentials, the device
// identity and the runtime state, kept in memory and written through to a
// persistence backend (ADR 0018, SDD §4.7).
//
// Two backends: accounts.json (default, mode 0600, atomic write) and Redis,
// for a hosted deployment without a durable file system
// (`DS_REDIS_HOST` + `DS_REDIS_TOKEN`, Upstash-shaped). The package is
// deliberately pure Go/stdlib — the Redis client is a small RESP2
// implementation in redis.go, so the binary keeps a dependency-free build.
package accounts

import (
	"context"
	"fmt"
	"sync"

	"github.com/yg/mimo2api/internal/egress"
)

// Session is the derived, refreshable upstream session (absent = login on
// demand, SDD §4.7).
type Session struct {
	Cookie    string `json:"cookie"`
	PH        string `json:"ph"`
	FetchedAt int64  `json:"fetched_at"` // unix seconds
}

// Account is one pool entry (SDD §4.7). Runtime state (user_id, pass_token,
// session, state, cooldown_until, disabled_reason) is persisted back so
// restarts don't re-login the whole pool.
type Account struct {
	ID       string `json:"id"`
	Phone    string `json:"phone,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
	Label    string `json:"label,omitempty"`
	// Egress is an egress LABEL only ("hk1"); the endpoint and its credentials
	// come from the environment (MIMO2API_EGRESS_HK1). Empty means the default
	// "direct" egress. Validated at startup (SDD §4.8).
	Egress    string `json:"egress,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	PassToken string `json:"pass_token,omitempty"`
	// Device is the passport device identity (deviceId/pass_ua/uLocale/…)
	// harvested from a login and replayed on every later login/renewal, so the
	// upstream recognises the device instead of seeing new hardware each time
	// (docs/login-chain-reverse.md §5 item 1; internal/mimo/device.go).
	Device         map[string]string `json:"device,omitempty"`
	Session        *Session          `json:"session,omitempty"`
	State          string            `json:"state,omitempty"` // available | cooldown | disabled
	CooldownUntil  int64             `json:"cooldown_until,omitempty"`
	DisabledReason string            `json:"disabled_reason,omitempty"`
}

// File is the on-disk wrapper.
type File struct {
	Accounts []Account `json:"accounts"`
}

// ErrDuplicateID is returned when two enabled entries share an id — rejected
// loudly at load, not silently joined (SDD §4.7, TDD ACC-005/POOL-014).
type ErrDuplicateID struct {
	ID    string
	Index int
}

func (e *ErrDuplicateID) Error() string {
	return fmt.Sprintf("duplicate enabled account id %q at entry %d", e.ID, e.Index)
}

// Store is the in-memory account set. Reads never touch the backend; every
// mutation writes the changed account through (write-through, ADR 0018).
type Store struct {
	mu       sync.Mutex
	backend  Backend
	accounts []Account
}

// Load reads an accounts.json file into a new Store (the default backend).
// A missing file returns an empty store (not an error — an empty pool
// surfaces as 503 no_accounts downstream). Invalid JSON fails loudly naming
// the file (TDD ACC-007).
func Load(path string) (*Store, error) {
	return LoadBackend(context.Background(), NewFileBackend(path))
}

// LoadBackend loads from any backend, validating what it returns.
//
// An empty backend is a valid state: a fresh hosted deployment boots with an
// empty pool and is seeded through the admin surface or `-import-redis`.
func LoadBackend(ctx context.Context, backend Backend) (*Store, error) {
	accounts, err := backend.Load(ctx)
	if err != nil {
		return nil, err
	}
	if err := validate(accounts); err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	return &Store{backend: backend, accounts: accounts}, nil
}

// Close releases the backend (a no-op for the file backend).
func (s *Store) Close() error { return s.backend.Close() }

// BackendName names the active backend for startup logs.
func (s *Store) BackendName() string {
	switch b := s.backend.(type) {
	case *RedisBackend:
		return "redis (" + b.Addr() + ")"
	case *FileBackend:
		return "file " + b.Path()
	default:
		return "custom"
	}
}

// validate rejects duplicate enabled ids; disabled duplicates are tolerated
// and the later entry supersedes (TDD ACC-006).
func validate(accounts []Account) error {
	seen := map[string]bool{}
	for i, a := range accounts {
		// The egress LABEL is validated for every entry, enabled or not: a typo
		// in a disabled account should not wait for the day it is enabled to
		// surface, and the label charset is what makes its env name derivable
		// (SDD §4.8).
		if err := egress.Validate(a.Egress); err != nil && a.Egress != "" {
			return fmt.Errorf("accounts[%d] (%s): %w", i, a.ID, err)
		}
		if !a.Enabled {
			continue
		}
		if seen[a.ID] {
			return &ErrDuplicateID{ID: a.ID, Index: i}
		}
		seen[a.ID] = true
	}
	return nil
}

// Accounts returns a copy of the entries.
func (s *Store) Accounts() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Account(nil), s.accounts...)
}

// Get returns a copy of the entry with the given id, or ok=false.
func (s *Store) Get(id string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// Update replaces the stored entry with the same id (runtime-state persist,
// e.g. after login/refresh) and writes it through to the backend. Unknown id
// is a no-op.
//
// A backend failure is returned but the in-memory value is kept: the live
// pool state is correct and the next mutation re-writes the whole record, so
// a transient store failure costs at most one re-login after a restart. The
// caller logs it (SDD §4.7).
func (s *Store) Update(a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.accounts {
		if s.accounts[i].ID == a.ID {
			s.accounts[i] = a
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return s.backend.SaveAccount(context.Background(), a)
}

// ReplaceAll swaps the whole set (import/seeding) and writes every account
// through. Used by `-import-redis`, never on the request path.
func (s *Store) ReplaceAll(ctx context.Context, accounts []Account) error {
	if err := validate(accounts); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	for _, a := range accounts {
		if err := s.backend.SaveAccount(ctx, a); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.accounts = append([]Account(nil), accounts...)
	s.mu.Unlock()
	return nil
}
