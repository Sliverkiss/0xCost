package accounts

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ============================================================================
// Additional coverage for internal/accounts (TDD §6.2 floor ≥80%).
// ============================================================================

// Get must return the stored entry (all fields, including session) and miss
// cleanly for an unknown id; the returned value is a copy, so callers cannot
// mutate the store by accident.
func TestGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeSample(t, path, sampleFile())
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("hit returns the stored account", func(t *testing.T) {
		got, ok := s.Get("acc-1")
		if !ok {
			t.Fatal("acc-1 not found")
		}
		if got.ID != "acc-1" || got.Phone != "13800000001" || got.Label != "one" || !got.Enabled {
			t.Errorf("account = %+v", got)
		}
		if got.Session == nil || got.Session.Cookie != "userId=1; serviceToken=abc" ||
			got.Session.PH != "cGg=" || got.Session.FetchedAt != 1700000000 {
			t.Errorf("session = %+v", got.Session)
		}
	})

	t.Run("second entry keeps runtime state", func(t *testing.T) {
		got, ok := s.Get("acc-2")
		if !ok {
			t.Fatal("acc-2 not found")
		}
		if got.State != "cooldown" || got.CooldownUntil != 1700000123 || got.PassToken != "rt-2" {
			t.Errorf("account = %+v", got)
		}
	})

	t.Run("miss returns zero value and false", func(t *testing.T) {
		got, ok := s.Get("does-not-exist")
		if ok {
			t.Fatalf("unknown id reported found: %+v", got)
		}
		// reflect.DeepEqual, not ==: Account carries the device-identity map
		// (deviceId/pass_ua/uLocale) and is therefore not comparable.
		if !reflect.DeepEqual(got, Account{}) {
			t.Errorf("miss value = %+v, want zero Account", got)
		}
	})

	t.Run("returned value is a copy", func(t *testing.T) {
		got, _ := s.Get("acc-1")
		got.Label = "mutated"
		again, _ := s.Get("acc-1")
		if again.Label != "one" {
			t.Errorf("Get leaked a mutable reference: label = %q", again.Label)
		}
	})
}

// A write must surface a filesystem error instead of silently claiming
// success, and must not leave a partial file behind.
func TestSavePropagatesFilesystemError(t *testing.T) {
	dir := t.TempDir()
	// A regular file standing in for the parent directory makes MkdirAll fail
	// with ENOTDIR.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "accounts.json")
	s, err := LoadBackend(context.Background(), NewFileBackend(target))
	if err != nil {
		t.Fatal(err)
	}
	s.accounts = []Account{{ID: "a", Enabled: true}}
	// Point the backend at the unusable path *after* a successful load, so the
	// case exercises the write path and not the load path.
	s.backend.(*FileBackend).path = filepath.Join(blocker, "accounts.json")
	target = filepath.Join(blocker, "accounts.json")

	if err := s.Update(Account{ID: "a", Enabled: true}); err == nil {
		t.Fatal("expected an error when the parent path is not a directory")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Error("target file materialised despite a failed atomic write")
	}
}
