// Package accounts persists the account pool credentials store:
// data/accounts.json, mode 0600, atomic write (temp + rename + fsync),
// hot-reloadable (SDD §4.7).
package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	Egress         string   `json:"egress,omitempty"`
	UserID         string   `json:"user_id,omitempty"`
	PassToken      string   `json:"pass_token,omitempty"`
	Session        *Session `json:"session,omitempty"`
	State          string   `json:"state,omitempty"` // available | cooldown | disabled
	CooldownUntil  int64    `json:"cooldown_until,omitempty"`
	DisabledReason string   `json:"disabled_reason,omitempty"`
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

// Store wraps the file with a mutex and atomic persistence.
type Store struct {
	mu       sync.Mutex
	path     string
	accounts []Account
}

// Load reads the accounts file. A missing file returns an empty store (not
// an error — an empty pool surfaces as 503 no_accounts downstream). Invalid
// JSON fails loudly naming the file (TDD ACC-007).
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("accounts file %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("accounts file %s: invalid JSON: %w", path, err)
	}
	if err := validate(f.Accounts); err != nil {
		return nil, fmt.Errorf("accounts file %s: %w", path, err)
	}
	s.accounts = f.Accounts
	return s, nil
}

// validate rejects duplicate enabled ids; disabled duplicates are tolerated
// and the later entry supersedes (TDD ACC-006).
func validate(accounts []Account) error {
	seen := map[string]bool{}
	for i, a := range accounts {
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
// e.g. after login/refresh), then persists atomically. Unknown id is a no-op.
func (s *Store) Update(a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == a.ID {
			s.accounts[i] = a
			return s.saveLocked()
		}
	}
	return nil
}

// saveLocked writes atomically: temp file in the same directory, fsync,
// rename over the target, fsync the directory. File mode is 0600 (enforced
// on create and on re-save of a pre-existing 0644 file, TDD ACC-003).
func (s *Store) saveLocked() error {
	f := File{Accounts: s.accounts}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
