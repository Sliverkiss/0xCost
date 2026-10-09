package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/accounts"
)

// ADR 0020: `mimo2api -accounts <cmd>` is the answer to "how do I manage
// accounts when the store is Upstash and there is no file to edit?". These
// cases drive the command against a file backend (the same code path the real
// Upstash run exercises, only the backend differs) and assert on effects: the
// store contents, the file mode, and that secrets never reach stdout.

// cliEnv points the process at a temp accounts file and makes sure no Redis
// backend can be selected by accident.
func cliEnv(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "accounts.json")
	t.Setenv("MIMO2API_ACCOUNTS_FILE", path)
	t.Setenv("DS_REDIS_HOST", "")
	t.Setenv("DS_REDIS_TOKEN", "")
	t.Setenv("MIMO2API_PORT", "0")
	return path
}

// capture runs fn with stdout captured and returns what it printed.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	runErr := fn()
	w.Close()
	os.Stdout = old
	out := <-done
	r.Close()
	return out, runErr
}

func seedFile(t *testing.T, path string, accts ...accounts.Account) {
	t.Helper()
	data, err := json.MarshalIndent(accounts.File{Accounts: accts}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) accounts.File {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f accounts.File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("accounts file does not parse: %v", err)
	}
	return f
}

func TestAccountsCLIListsAndMasksSecrets(t *testing.T) {
	dir := t.TempDir()
	path := cliEnv(t, dir)
	seedFile(t, path,
		accounts.Account{ID: "13800000001", Phone: "13800000001", Password: "sup3r-secret",
			PassToken: "token-value-1234", Enabled: true,
			Device:  map[string]string{"deviceId": "dev-1"},
			Session: &accounts.Session{Cookie: "cUserId=x; serviceToken=y", FetchedAt: 1000}},
		accounts.Account{ID: "13800000002", Enabled: false, DisabledReason: "manual gate"},
	)

	out, err := capture(t, func() error { return runAccountsCommand([]string{"list"}) })
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"2 account(s)", "13800000001", "enabled", "13800000002", "disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output is missing %q:\n%s", want, out)
		}
	}

	out, err = capture(t, func() error { return runAccountsCommand([]string{"show", "13800000001"}) })
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	for _, secret := range []string{"sup3r-secret", "token-value-1234", "cUserId=x", "serviceToken=y"} {
		if strings.Contains(out, secret) {
			t.Errorf("show printed a secret (%q):\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "13800000") || !strings.Contains(out, "present(") {
		t.Errorf("show should print masked identifiers and presence markers:\n%s", out)
	}
}

func TestAccountsCLIEnableDisableAndEgress(t *testing.T) {
	dir := t.TempDir()
	path := cliEnv(t, dir)
	seedFile(t, path, accounts.Account{ID: "acc-1", Enabled: true})

	if _, err := capture(t, func() error {
		return runAccountsCommand([]string{"disable", "acc-1", "passport", "locked"})
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	f := readFile(t, path)
	if len(f.Accounts) != 1 || f.Accounts[0].Enabled {
		t.Fatalf("after disable: %+v", f.Accounts)
	}
	if f.Accounts[0].DisabledReason != "passport locked" {
		t.Errorf("reason = %q, want %q", f.Accounts[0].DisabledReason, "passport locked")
	}

	if _, err := capture(t, func() error { return runAccountsCommand([]string{"enable", "acc-1"}) }); err != nil {
		t.Fatalf("enable: %v", err)
	}
	f = readFile(t, path)
	if !f.Accounts[0].Enabled || f.Accounts[0].DisabledReason != "" {
		t.Errorf("after enable: %+v (reason must be cleared)", f.Accounts[0])
	}

	if _, err := capture(t, func() error { return runAccountsCommand([]string{"set-egress", "acc-1", "sg"}) }); err != nil {
		t.Fatalf("set-egress: %v", err)
	}
	if got := readFile(t, path).Accounts[0].Egress; got != "sg" {
		t.Errorf("egress = %q, want sg", got)
	}
	if _, err := capture(t, func() error { return runAccountsCommand([]string{"set-egress", "acc-1"}) }); err != nil {
		t.Fatalf("clear egress: %v", err)
	}
	if got := readFile(t, path).Accounts[0].Egress; got != "" {
		t.Errorf("egress = %q, want it cleared", got)
	}

	// An unknown id must fail loudly rather than silently doing nothing.
	if _, err := capture(t, func() error { return runAccountsCommand([]string{"enable", "nope"}) }); err == nil {
		t.Error("enable with an unknown id must return an error")
	}
}

func TestAccountsCLIRemoveImportExport(t *testing.T) {
	dir := t.TempDir()
	path := cliEnv(t, dir)
	seedFile(t, path,
		accounts.Account{ID: "keep", Enabled: true},
		accounts.Account{ID: "drop", Enabled: true},
	)

	if _, err := capture(t, func() error { return runAccountsCommand([]string{"rm", "drop"}) }); err != nil {
		t.Fatalf("rm: %v", err)
	}
	f := readFile(t, path)
	if len(f.Accounts) != 1 || f.Accounts[0].ID != "keep" {
		t.Fatalf("after rm: %+v", f.Accounts)
	}

	src := filepath.Join(dir, "import.json")
	seedFile(t, src,
		accounts.Account{ID: "keep", Enabled: false, Phone: "19900000000"},
		accounts.Account{ID: "fresh", Enabled: true, PassToken: "t"},
	)
	out, err := capture(t, func() error { return runAccountsCommand([]string{"import", src}) })
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2 account(s) written") {
		t.Errorf("import output: %s", out)
	}
	f = readFile(t, path)
	if len(f.Accounts) != 2 {
		t.Fatalf("after import: %+v (want keep + fresh upserted)", f.Accounts)
	}
	for _, a := range f.Accounts {
		if a.ID == "keep" && a.Enabled {
			t.Error("import must replace an existing row, not skip it")
		}
	}

	dump := filepath.Join(dir, "dump.json")
	if _, err := capture(t, func() error { return runAccountsCommand([]string{"export", dump}) }); err != nil {
		t.Fatalf("export: %v", err)
	}
	info, err := os.Stat(dump)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("export mode = %o, want 600 (it holds credentials)", perm)
	}
	if got := readFile(t, dump); len(got.Accounts) != 2 {
		t.Errorf("export wrote %d accounts, want 2", len(got.Accounts))
	}
	// Exporting over an existing file must not widen its mode.
	if err := os.Chmod(dump, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return runAccountsCommand([]string{"export", dump}) }); err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Logf("note: mode after re-export = %o", perm)
	}
	info2, _ := os.Stat(dump)
	if perm := info2.Mode().Perm(); perm != 0o600 {
		t.Errorf("re-export left mode %o, want 600", perm)
	}
}

func TestAccountsCLIVerifyUsageAndUnknownCommand(t *testing.T) {
	dir := t.TempDir()
	path := cliEnv(t, dir)
	seedFile(t, path, accounts.Account{ID: "acc-1", Enabled: true,
		Session: &accounts.Session{Cookie: "serviceToken=z", FetchedAt: 1}})

	out, err := capture(t, func() error { return runAccountsCommand([]string{"verify"}) })
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	for _, want := range []string{"1 (1 enabled, 1 with a cached session)"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output missing %q:\n%s", want, out)
		}
	}

	if _, err := capture(t, func() error { return runAccountsCommand([]string{"bogus"}) }); err == nil {
		t.Error("an unknown subcommand must fail")
	}
	if _, err := capture(t, func() error { return runAccountsCommand([]string{"rm"}) }); err == nil {
		t.Error("rm without an id must fail")
	}
	if _, err := capture(t, func() error { return runAccountsCommand(nil) }); err == nil {
		t.Error("no subcommand must fail")
	}
}

// A store with no accounts must say so rather than reporting success: an empty
// pool is a deployment that cannot serve a single request.
func TestAccountsCLIVerifyOnEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := cliEnv(t, dir)
	seedFile(t, path)
	quiet, err := capture(t, func() error { return runAccountsCommand([]string{"verify"}) })
	if err == nil {
		t.Fatalf("verify on an empty store must fail; output:\n%s", quiet)
	}
}

// The flag extractor is what lets one binary be both server and admin tool.
func TestAccountsFlagValue(t *testing.T) {
	got, ok := accountsFlagValue([]string{"-log-level", "info", "-accounts", "list"})
	if !ok || len(got) != 1 || got[0] != "list" {
		t.Errorf("got %v ok=%v", got, ok)
	}
	got, ok = accountsFlagValue([]string{"-accounts=show", "acc-1"})
	if !ok || len(got) != 2 || got[0] != "show" || got[1] != "acc-1" {
		t.Errorf("got %v ok=%v", got, ok)
	}
	if _, ok := accountsFlagValue([]string{"-log-level", "debug"}); ok {
		t.Error("no -accounts in argv must not trigger the CLI")
	}
	if got, ok := accountsFlagValue([]string{"-accounts"}); !ok || len(got) != 0 {
		t.Errorf("-accounts without a command must be detected with no args, got %v ok=%v", got, ok)
	}
}
