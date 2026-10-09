package accounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ADR 0020: the management CLI needs an upsert and a delete on the store. The
// request path never uses either (runtime state goes through Update), so these
// cases pin the semantics the CLI depends on: Put adds or replaces, Remove
// reports an unknown id, and both write through to the backend.

func TestStorePutAddsAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	store, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := store.Put(Account{ID: "acc-1", Enabled: true, PassToken: "first"}); err != nil {
		t.Fatalf("put new: %v", err)
	}
	if err := store.Put(Account{ID: "acc-2", Enabled: true}); err != nil {
		t.Fatalf("put second: %v", err)
	}
	if err := store.Put(Account{ID: "acc-1", Enabled: false, PassToken: "second"}); err != nil {
		t.Fatalf("put replace: %v", err)
	}
	accts := store.Accounts()
	if len(accts) != 2 {
		t.Fatalf("store holds %d accounts, want 2 (replace, not append)", len(accts))
	}
	got, _ := store.Get("acc-1")
	if got.Enabled || got.PassToken != "second" {
		t.Errorf("acc-1 = %+v, want the replacement", got)
	}

	// The writes must be on disk, not only in memory: reopening the file must
	// show the same two records.
	reopened, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reopened.Accounts()) != 2 {
		t.Fatalf("file holds %d accounts after Put, want 2", len(reopened.Accounts()))
	}

	// An account without an id cannot be put: nothing to key it on.
	if err := store.Put(Account{Enabled: true}); err == nil {
		t.Error("Put with an empty id must fail")
	}
}

func TestStoreRemoveDeletesAndReportsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	store, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.Put(Account{ID: id, Enabled: true}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	if err := store.Remove("acc-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := store.Get("acc-1"); ok {
		t.Error("acc-1 is still in the store after Remove")
	}
	var nf *ErrNotFound
	if err := store.Remove("acc-1"); !errors.As(err, &nf) {
		t.Errorf("removing an unknown id: err = %v, want ErrNotFound", err)
	}

	reopened, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	accts := reopened.Accounts()
	if len(accts) != 1 || accts[0].ID != "acc-2" {
		t.Fatalf("file holds %+v after Remove, want only acc-2", accts)
	}
}

func TestStoreRemoveSurfacesBackendFailure(t *testing.T) {
	b := &countingBackend{
		loaded:  []Account{{ID: "acc-1", Enabled: true}},
		saveErr: errors.New("backend down"),
	}
	store, err := LoadBackend(context.Background(), b)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := store.Remove("acc-1"); err == nil {
		t.Fatal("Remove must surface a backend failure instead of pretending success")
	}
	// The in-memory set is unchanged on failure: the row still exists.
	if _, ok := store.Get("acc-1"); !ok {
		t.Error("a failed delete must not drop the entry in memory")
	}
}

// The file backend's delete must not leave a stale row behind when two accounts
// share a prefix or when the id is the last one.
func TestFileBackendDeleteHandlesEdges(t *testing.T) {
	dir := t.TempDir()
	fb := NewFileBackend(filepath.Join(dir, "accounts.json"))
	ctx := context.Background()
	for _, id := range []string{"18", "180", "1800"} {
		if err := fb.SaveAccount(ctx, Account{ID: id, Enabled: true}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	if err := fb.DeleteAccount(ctx, "180"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := fb.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d rows (want 2): substring matching would have deleted more than one", len(got))
	}
	if err := fb.DeleteAccount(ctx, "nope"); err != nil {
		t.Fatalf("deleting an unknown id must be a no-op, got %v", err)
	}
	// The file must still be 0600 after a delete-triggered rewrite.
	info, err := os.Stat(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after delete = %o, want 600", perm)
	}
}
