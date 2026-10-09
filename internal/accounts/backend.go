// Backend abstraction for the accounts store (ADR 0018): the pool's
// credentials and runtime state have to survive a restart, and a hosted
// deployment (Belmo) has no durable file system of its own — so the file
// becomes one of two implementations rather than the only one.
//
// The live path never reads the backend: the Store caches the set in memory
// (Load happens once at startup) and writes through on mutation, exactly the
// shape the file store always had.
package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Backend persists the account set. Implementations: FileBackend (default,
// accounts.json) and RedisBackend (Upstash, chosen by DS_REDIS_HOST).
type Backend interface {
	// Load returns every persisted account. A missing/empty source is a
	// valid state — an empty pool answers 503 no_accounts downstream.
	Load(ctx context.Context) ([]Account, error)
	// SaveAccount upserts one account's full record.
	SaveAccount(ctx context.Context, a Account) error
	// Close releases resources (a no-op for the file backend).
	Close() error
}

// FileBackend is the accounts.json store: one JSON document, mode 0600,
// atomic write (temp + fsync + rename + directory fsync).
//
// It mirrors the set it has loaded/updated because the on-disk format is a
// single document: a per-account write has to be a read-modify-write of the
// whole file. The only writer is the Store, so the mirror cannot drift.
type FileBackend struct {
	mu   sync.Mutex
	path string
	set  []Account
}

// NewFileBackend returns a file-backed store. The file is not read until
// Load; neither the file nor its directory has to exist.
func NewFileBackend(path string) *FileBackend { return &FileBackend{path: path} }

// Path reports the file the backend owns (log lines and diagnostics).
func (f *FileBackend) Path() string { return f.path }

// Load reads the file. A missing file is an empty set, not an error — that is
// what lets a fresh container start with no accounts and be seeded later.
// Invalid JSON fails loudly naming the file (TDD ACC-007).
func (f *FileBackend) Load(_ context.Context) ([]Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			f.set = nil
			return nil, nil
		}
		return nil, fmt.Errorf("accounts file %s: %w", f.path, err)
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("accounts file %s: invalid JSON: %w", f.path, err)
	}
	f.set = file.Accounts
	return append([]Account(nil), file.Accounts...), nil
}

// SaveAccount replaces (or appends) one entry in the mirrored set and writes
// the whole document atomically.
func (f *FileBackend) SaveAccount(_ context.Context, a Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	replaced := false
	for i := range f.set {
		if f.set[i].ID == a.ID {
			f.set[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		f.set = append(f.set, a)
	}
	return f.saveLocked()
}

// saveLocked writes atomically: temp file in the same directory, fsync,
// rename over the target, fsync the directory. The directory is created on
// demand, so a deployment needs no pre-made data/ volume. File mode is 0600
// (enforced on create and on re-save of a pre-existing 0644 file,
// TDD ACC-003).
func (f *FileBackend) saveLocked() error {
	data, err := json.MarshalIndent(File{Accounts: f.set}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
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
	if err := os.Rename(tmpName, f.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Close is a no-op: the file backend holds no handles between calls.
func (f *FileBackend) Close() error { return nil }
