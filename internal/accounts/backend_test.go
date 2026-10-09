package accounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// FileBackend
// ---------------------------------------------------------------------------

func TestFileBackendMissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "accounts.json")
	b := NewFileBackend(path)
	if b.Path() != path {
		t.Errorf("Path = %q", b.Path())
	}
	got, err := b.Load(context.Background())
	if err != nil {
		t.Fatalf("a missing file must be an empty set, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file returned %d accounts", len(got))
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestFileBackendInvalidJSONNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileBackend(path).Load(context.Background()); err == nil ||
		!strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want a message naming %s", err, path)
	}
}

// A save creates the directory on demand (a hosted build has no pre-made
// data/ volume), writes mode 0600, and round-trips through a fresh Store.
func TestFileBackendSaveCreatesAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "made", "on", "demand", "accounts.json")
	store, err := LoadBackend(context.Background(), NewFileBackend(path))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to update yet: an unknown id is a no-op, not an error.
	if err := store.Update(Account{ID: "ghost", Enabled: true}); err != nil {
		t.Fatalf("update of an unknown id = %v, want a no-op", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a no-op update must not create the file")
	}

	acct := Account{ID: "acc-1", Phone: "13800000001", Enabled: true, PassToken: "pt-1"}
	if err := store.ReplaceAll(context.Background(), []Account{acct}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("acc-1")
	if !ok || got.PassToken != "pt-1" {
		t.Fatalf("reloaded = %+v (found=%v)", got, ok)
	}

	// An update replaces in place and appends nothing.
	got.PassToken = "pt-2"
	if err := reloaded.Update(got); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Accounts()); n != 1 {
		t.Fatalf("update changed the account count: %d", n)
	}
	if a, _ := again.Get("acc-1"); a.PassToken != "pt-2" {
		t.Errorf("update did not persist: %+v", a)
	}
}

// ReplaceAll validates before writing, so a duplicate-id import cannot land.
func TestReplaceAllRejectsDuplicates(t *testing.T) {
	store, err := LoadBackend(context.Background(), NewFileBackend(filepath.Join(t.TempDir(), "a.json")))
	if err != nil {
		t.Fatal(err)
	}
	err = store.ReplaceAll(context.Background(), []Account{{ID: "dup", Enabled: true}, {ID: "dup", Enabled: true}})
	var dup *ErrDuplicateID
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want ErrDuplicateID", err)
	}
}

// ---------------------------------------------------------------------------
// Store over a backend (write-through semantics, ADR 0018)
// ---------------------------------------------------------------------------

type countingBackend struct {
	loaded   []Account
	saves    []Account
	deletes  []string
	saveErr  error
	loadErr  error
	closed   bool
	loadCall int
}

func (c *countingBackend) Load(context.Context) ([]Account, error) {
	c.loadCall++
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	return c.loaded, nil
}

func (c *countingBackend) SaveAccount(_ context.Context, a Account) error {
	if c.saveErr != nil {
		return c.saveErr
	}
	c.saves = append(c.saves, a)
	return nil
}

func (c *countingBackend) DeleteAccount(_ context.Context, id string) error {
	if c.saveErr != nil {
		return c.saveErr
	}
	c.deletes = append(c.deletes, id)
	kept := c.loaded[:0:0]
	for _, a := range c.loaded {
		if a.ID != id {
			kept = append(kept, a)
		}
	}
	c.loaded = kept
	return nil
}

func (c *countingBackend) Close() error { c.closed = true; return nil }

// The store reads the backend once at startup and never on a mutation path.
func TestStoreLoadsOnceAndWritesThrough(t *testing.T) {
	b := &countingBackend{loaded: []Account{{ID: "acc-1", Enabled: true, PassToken: "old"}}}
	store, err := LoadBackend(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		store.Accounts()
		store.Get("acc-1")
	}
	if b.loadCall != 1 {
		t.Errorf("backend loads = %d, want exactly 1 (startup)", b.loadCall)
	}

	if err := store.Update(Account{ID: "acc-1", Enabled: true, PassToken: "new"}); err != nil {
		t.Fatal(err)
	}
	if len(b.saves) != 1 || b.saves[0].PassToken != "new" {
		t.Fatalf("saves = %+v, want the updated record", b.saves)
	}
	if a, _ := store.Get("acc-1"); a.PassToken != "new" {
		t.Errorf("in-memory value = %+v", a)
	}
	if got := store.BackendName(); got != "custom" {
		t.Errorf("BackendName = %q, want custom for a foreign backend", got)
	}
	if err := store.Close(); err != nil || !b.closed {
		t.Errorf("Close did not reach the backend (%v, closed=%v)", err, b.closed)
	}
}

// A failed write is reported but must not corrupt the live cache: persistence
// is fail-soft, the next mutation re-writes the whole record.
func TestStoreUpdateReportsBackendFailureAndKeepsCache(t *testing.T) {
	b := &countingBackend{
		loaded:  []Account{{ID: "acc-1", Enabled: true, PassToken: "old"}},
		saveErr: errors.New("store down"),
	}
	store, err := LoadBackend(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(Account{ID: "acc-1", Enabled: true, PassToken: "new"}); err == nil {
		t.Fatal("expected the backend failure to be reported")
	}
	if a, _ := store.Get("acc-1"); a.PassToken != "new" {
		t.Errorf("cache = %+v, want the in-memory value kept", a)
	}
}

func TestLoadBackendPropagatesLoadError(t *testing.T) {
	b := &countingBackend{loadErr: errors.New("unreachable")}
	if _, err := LoadBackend(context.Background(), b); err == nil {
		t.Fatal("expected the load error to propagate")
	}
}

func TestBackendNameForFileAndRedis(t *testing.T) {
	fs, err := LoadBackend(context.Background(), NewFileBackend("/tmp/x/accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fs.BackendName(); !strings.HasPrefix(got, "file ") || !strings.Contains(got, "accounts.json") {
		t.Errorf("file BackendName = %q", got)
	}

	f := newFakeRedis(t)
	b, err := OpenUpstash("redis://"+f.addr(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	rs, err := LoadBackend(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if got := rs.BackendName(); !strings.HasPrefix(got, "redis (") || !strings.Contains(got, f.addr()) {
		t.Errorf("redis BackendName = %q", got)
	}
}
