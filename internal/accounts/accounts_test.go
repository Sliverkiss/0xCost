package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleFile() File {
	return File{Accounts: []Account{
		{
			ID:      "acc-1",
			Phone:   "13800000001",
			Enabled: true,
			Label:   "one",
			Session: &Session{Cookie: "userId=1; serviceToken=abc", PH: "cGg=", FetchedAt: 1700000000},
			State:   "available",
		},
		{
			ID:            "acc-2",
			Phone:         "13800000002",
			Enabled:       true,
			PassToken:     "rt-2",
			UserID:        "u2",
			State:         "cooldown",
			CooldownUntil: 1700000123,
		},
	}}
}

func writeSample(t *testing.T, path string, f File) {
	t.Helper()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ACC-001: load round-trip including runtime state fields.
func TestLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeSample(t, path, sampleFile())
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Accounts()
	want := sampleFile().Accounts
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].State != want[i].State ||
			got[i].CooldownUntil != want[i].CooldownUntil {
			t.Errorf("acc %d: got %+v want %+v", i, got[i], want[i])
		}
		if want[i].Session != nil {
			if got[i].Session == nil || got[i].Session.Cookie != want[i].Session.Cookie ||
				got[i].Session.PH != want[i].Session.PH || got[i].Session.FetchedAt != want[i].Session.FetchedAt {
				t.Errorf("acc %d session: got %+v", i, got[i].Session)
			}
		}
	}
}

// ACC-002: atomic write — the target file is always complete JSON.
func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	writeSample(t, path, sampleFile())
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		accs := s.Accounts()
		accs[0].State = "available"
		accs[0].CooldownUntil = int64(1700000000 + i)
		if err := s.Update(accs[0]); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f File
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("iteration %d: file not complete JSON: %v", i, err)
		}
		if len(f.Accounts) != 2 {
			t.Fatalf("iteration %d: accounts = %d", i, len(f.Accounts))
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".accounts-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

// ACC-003: file mode 0600 enforced on re-save of a pre-existing 0644 file.
func TestPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	data, _ := json.Marshal(sampleFile())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	accs := s.Accounts()
	accs[0].Label = "updated"
	if err := s.Update(accs[0]); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}
}

// ACC-004: hot reload — file modifications picked up on Load.
func TestHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeSample(t, path, sampleFile())
	s1, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.Accounts()) != 2 {
		t.Fatalf("initial = %d", len(s1.Accounts()))
	}
	f := sampleFile()
	f.Accounts = append(f.Accounts, Account{ID: "acc-3", Enabled: true})
	writeSample(t, path, f)
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Accounts()) != 3 {
		t.Errorf("reloaded = %d, want 3", len(s2.Accounts()))
	}
}

// ACC-005: duplicate enabled ids rejected loudly.
func TestDuplicateIDsRejected(t *testing.T) {
	f := sampleFile()
	f.Accounts = append(f.Accounts, Account{ID: "acc-1", Enabled: true})
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeSample(t, path, f)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected duplicate-id error")
	}
	if !strings.Contains(err.Error(), "acc-1") {
		t.Errorf("error must name the duplicate id: %v", err)
	}
}

// ACC-006: disabled duplicates tolerated, load succeeds.
func TestDisabledDuplicatesTolerated(t *testing.T) {
	f := sampleFile()
	f.Accounts = append(f.Accounts, Account{ID: "acc-1", Enabled: false})
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeSample(t, path, f)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("disabled duplicate must not fail load: %v", err)
	}
	if len(s.Accounts()) != 3 {
		t.Errorf("accounts = %d", len(s.Accounts()))
	}
}

// ACC-007: corrupt file errors naming the file.
func TestCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected corrupt-file error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the file: %v", err)
	}
}

// Missing file loads an empty store (empty pool surfaces downstream as 503).
func TestMissingFileEmptyStore(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 0 {
		t.Errorf("accounts = %d", len(s.Accounts()))
	}
}

// Update of an unknown id is a no-op (no phantom entries).
func TestUpdateUnknownID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	writeSample(t, path, sampleFile())
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(Account{ID: "ghost", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 2 {
		t.Errorf("phantom entry created: %d", len(s.Accounts()))
	}
}
