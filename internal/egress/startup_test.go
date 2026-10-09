package egress_test

// The startup contract, end to end but without a process: accounts.json names the
// labels, the environment carries the endpoints, and a broken pair is a startup
// error rather than a silent direct connection (SDD §4.8, REQ-16).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/egress"
)

func writeAccounts(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func resolveFromStore(t *testing.T, store *accounts.Store, env map[string]string) error {
	t.Helper()
	var labels []string
	for _, a := range store.Accounts() {
		labels = append(labels, a.Egress)
	}
	_, err := egress.Resolve(egress.Labels(labels), func(name string) string { return env[name] })
	return err
}

// EGRESS-009: a label with no endpoint fails startup and names the variable.
func TestStoreLabelWithoutEndpointFailsStartup(t *testing.T) {
	path := writeAccounts(t, `{"accounts":[{"id":"acc1","enabled":true,"egress":"hk1"}]}`)
	store, err := accounts.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = resolveFromStore(t, store, map[string]string{})
	if err == nil {
		t.Fatal("startup accepted a label with no endpoint: traffic would leave from the host's own IP")
	}
	if !strings.Contains(err.Error(), "MIMO2API_EGRESS_HK1") {
		t.Errorf("the error does not name the variable to set: %v", err)
	}
	// With the variable set, the same store resolves.
	if err := resolveFromStore(t, store, map[string]string{"MIMO2API_EGRESS_HK1": "http://proxy.example:8080"}); err != nil {
		t.Errorf("resolving a configured label failed: %v", err)
	}
}

// EGRESS-010: a malformed label never reaches the pool — the accounts file is
// rejected at load with the entry's position and id.
func TestStoreRejectsMalformedLabel(t *testing.T) {
	path := writeAccounts(t, `{"accounts":[{"id":"acc1","enabled":true,"egress":"HK1"}]}`)
	_, err := accounts.Load(path)
	if err == nil {
		t.Fatal("Load accepted an uppercase label")
	}
	if !strings.Contains(err.Error(), "acc1") {
		t.Errorf("the error does not identify the entry: %v", err)
	}
}

// EGRESS-011: relabelling is reversible — the labels are read from the file each
// startup, so an account can move between egresses without code or state changes.
func TestRelabellingIsReversible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	env := map[string]string{
		"MIMO2API_EGRESS_HK1": "http://hk1.example:8080",
		"MIMO2API_EGRESS_SG2": "http://sg2.example:8080",
	}
	steps := []struct {
		body     string
		wantLbls []string
	}{
		{`{"accounts":[{"id":"acc1","enabled":true,"egress":"hk1"}]}`, []string{"hk1"}},
		{`{"accounts":[{"id":"acc1","enabled":true,"egress":"sg2"}]}`, []string{"sg2"}},
		{`{"accounts":[{"id":"acc1","enabled":true}]}`, nil},
	}
	for _, step := range steps {
		if err := os.WriteFile(path, []byte(step.body), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := accounts.Load(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		var labels []string
		for _, a := range store.Accounts() {
			labels = append(labels, a.Egress)
		}
		got := egress.Labels(labels)
		if len(got) != len(step.wantLbls) {
			t.Fatalf("labels = %v, want %v", got, step.wantLbls)
		}
		for i := range got {
			if got[i] != step.wantLbls[i] {
				t.Errorf("labels = %v, want %v", got, step.wantLbls)
			}
		}
		if err := resolveFromStore(t, store, env); err != nil {
			t.Errorf("resolving %v failed: %v", got, err)
		}
	}
}
