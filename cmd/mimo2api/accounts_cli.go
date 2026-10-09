// Account management CLI (ADR 0020): `mimo2api -accounts <cmd> [args]`.
//
// Why this exists: when the account store became Upstash (ADR 0018) the
// operational question the file never had showed up — how do you look at,
// enable, disable or remove an account when the store is a remote database and
// there is no file to edit? The answer is this: the same binary, the same
// environment, one subcommand. The operator runs it locally against whatever the
// env configures (DS_REDIS_HOST set → Upstash, otherwise the accounts file), so
// there is no Redis console to reach for and no long-lived admin HTTP surface to
// expose.
//
// Every command opens the store through the same selection the server uses, so
// "which backend am I editing?" is never a guess. Destructive commands name what
// they did; reads print presence and ages, never secrets.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/config"
)

const accountsUsage = `mimo2api -accounts <command> [args]

Account store management for whichever backend the environment selects
(DS_REDIS_HOST set = Upstash, otherwise MIMO2API_ACCOUNTS_FILE).

  list                       one line per account: id, enabled, egress, state of
                             its credential and session
  show <id>                  full record with secrets replaced by presence/length
  enable <id>                mark enabled
  disable <id> [reason]      mark disabled, recording the reason
  set-egress <id> <label>    pin the account to a network path label ("" clears)
  rm <id>                    delete the account
  import <file>              upsert every account in an accounts.json file
  export <file>              write the whole store to a file (mode 0600)
  verify                     connect, count and report the backend
`

// runAccountsCommand dispatches `-accounts`. It returns an error suitable for
// log.Fatal, and prints its own output on success.
func runAccountsCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, accountsUsage)
		return errors.New("-accounts needs a command")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "list":
		return accountsList(store)
	case "show":
		return accountsShow(store, rest)
	case "enable":
		return accountsSetEnabled(store, rest, true)
	case "disable":
		return accountsSetEnabled(store, rest, false)
	case "set-egress":
		return accountsSetEgress(store, rest)
	case "rm", "remove":
		return accountsRemove(store, rest)
	case "import":
		return accountsImport(store, cfg, rest)
	case "export":
		return accountsExport(store, rest)
	case "verify":
		return accountsVerify(store)
	case "help", "-h", "--help":
		fmt.Print(accountsUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, accountsUsage)
		return fmt.Errorf("-accounts: unknown command %q", cmd)
	}
}

func accountsList(store *accounts.Store) error {
	accts := store.Accounts()
	sort.Slice(accts, func(i, j int) bool { return accts[i].ID < accts[j].ID })
	fmt.Printf("%d account(s) in %s\n", len(accts), store.BackendName())
	now := time.Now().Unix()
	for _, a := range accts {
		enabled := "disabled"
		if a.Enabled {
			enabled = "enabled"
		}
		fmt.Printf("  %s  %-8s egress=%-6s %s | %s | %s\n",
			a.ID, enabled, egressLabel(a), credState(a, now), sessionState(a, now), deviceState(a))
	}
	if len(accts) == 0 {
		fmt.Println("  (empty: nothing to serve; import or add an account)")
	}
	return nil
}

func accountsShow(store *accounts.Store, args []string) error {
	id, err := requireID(args, "show")
	if err != nil {
		return err
	}
	a, ok := store.Get(id)
	if !ok {
		return &accounts.ErrNotFound{ID: id}
	}
	now := time.Now().Unix()
	fmt.Printf("id:        %s\n", a.ID)
	fmt.Printf("enabled:   %v\n", a.Enabled)
	if a.DisabledReason != "" {
		fmt.Printf("reason:    %s\n", a.DisabledReason)
	}
	fmt.Printf("phone:     %s\n", masked(a.Phone))
	fmt.Printf("password:  %s\n", presence(a.Password))
	fmt.Printf("user_id:   %s\n", masked(a.UserID))
	fmt.Printf("pass_token:%s\n", presence(a.PassToken))
	fmt.Printf("egress:    %s\n", egressLabel(a))
	fmt.Printf("credential: %s\n", credState(a, now))
	fmt.Printf("session:   %s\n", sessionState(a, now))
	fmt.Printf("device:    %s\n", deviceState(a))
	fmt.Printf("cookie:    %s\n", presence(cookieOf(a)))
	return nil
}

func accountsSetEnabled(store *accounts.Store, args []string, enabled bool) error {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	id, err := requireID(args, verb)
	if err != nil {
		return err
	}
	a, ok := store.Get(id)
	if !ok {
		return &accounts.ErrNotFound{ID: id}
	}
	a.Enabled = enabled
	if enabled {
		a.DisabledReason = ""
	} else if len(args) > 1 {
		a.DisabledReason = strings.Join(args[1:], " ")
	} else {
		a.DisabledReason = "disabled by operator"
	}
	if err := store.Update(a); err != nil {
		return err
	}
	fmt.Printf("%s: %s (%s)\n", verb, id, a.DisabledReason)
	return nil
}

func accountsSetEgress(store *accounts.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("set-egress needs <id> [label] (no label clears it)")
	}
	id := args[0]
	label := ""
	if len(args) > 1 {
		label = args[1]
	}
	a, ok := store.Get(id)
	if !ok {
		return &accounts.ErrNotFound{ID: id}
	}
	a.Egress = label
	if err := store.Update(a); err != nil {
		return err
	}
	if label == "" {
		fmt.Printf("set-egress: %s now uses the direct network path\n", id)
	} else {
		fmt.Printf("set-egress: %s now uses label %q (needs MIMO2API_EGRESS_%s in the server's environment)\n",
			id, label, strings.ToUpper(strings.ReplaceAll(label, "-", "_")))
	}
	return nil
}

func accountsRemove(store *accounts.Store, args []string) error {
	id, err := requireID(args, "rm")
	if err != nil {
		return err
	}
	if err := store.Remove(id); err != nil {
		return err
	}
	fmt.Printf("rm: %s deleted from %s (%d left)\n", id, store.BackendName(), len(store.Accounts()))
	return nil
}

// accountsImport upserts a local accounts.json into the active store. It works
// on either backend, so "load these accounts into Upstash" and "restore a dump
// into the file" are the same command.
func accountsImport(store *accounts.Store, cfg *config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("import needs <accounts.json>")
	}
	src, err := accounts.Load(args[0])
	if err != nil {
		return err
	}
	accts := src.Accounts()
	if len(accts) == 0 {
		return fmt.Errorf("%s holds no accounts", args[0])
	}
	for _, a := range accts {
		if err := store.Put(a); err != nil {
			return fmt.Errorf("import %s: %w", a.ID, err)
		}
	}
	// Read the store back: an import is only done when the destination answers
	// with what we just wrote (external effect, so verify rather than trust).
	missing := 0
	for _, a := range accts {
		if _, ok := store.Get(a.ID); !ok {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("import: %d of %d accounts did not read back", missing, len(accts))
	}
	fmt.Printf("import: %d account(s) written to %s (total %d)\n", len(accts), store.BackendName(), len(store.Accounts()))
	return nil
}

// accountsExport dumps the store to a file with mode 0600. This is the backup
// path a hosted deployment needs: the store may be a database with no file
// system of its own, and a credential set is exactly the thing worth having a
// local copy of.
func accountsExport(store *accounts.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("export needs <file> (it will contain real credentials)")
	}
	accts := store.Accounts()
	data, err := json.MarshalIndent(accounts.File{Accounts: accts}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(args[0], data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(args[0], 0o600); err != nil { // WriteFile only applies mode on create
		return err
	}
	fmt.Printf("export: %d account(s) from %s → %s (mode 0600, holds credentials)\n",
		len(accts), store.BackendName(), args[0])
	return nil
}

func accountsVerify(store *accounts.Store) error {
	accts := store.Accounts()
	enabled, withSession := 0, 0
	for _, a := range accts {
		if a.Enabled {
			enabled++
		}
		if a.Session != nil && a.Session.Cookie != "" {
			withSession++
		}
	}
	fmt.Printf("backend: %s\naccounts: %d (%d enabled, %d with a cached session)\n",
		store.BackendName(), len(accts), enabled, withSession)
	if len(accts) == 0 {
		return errors.New("verify: the store holds no accounts")
	}
	return nil
}

// ---- helpers -------------------------------------------------------------

func requireID(args []string, cmd string) (string, error) {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("%s needs an account id (see -accounts list)", cmd)
	}
	return args[0], nil
}

// egressLabel renders the account's network path label, "direct" when unset.
func egressLabel(a accounts.Account) string {
	if a.Egress == "" {
		return "direct"
	}
	return a.Egress
}

// credState reports the credential without printing it: presence plus the age of
// its own clock is what an operator acts on, and the value is what must never
// leave the store (INV-3, ADR 0020).
func credState(a accounts.Account, now int64) string {
	if a.PassToken == "" {
		return "credential=MISSING"
	}
	return fmt.Sprintf("credential=present(%d chars)", len(a.PassToken))
}

func sessionState(a accounts.Account, now int64) string {
	if a.Session == nil || a.Session.Cookie == "" {
		return "session=none"
	}
	if a.Session.FetchedAt == 0 {
		return "session=present(age unknown)"
	}
	age := time.Duration(now-a.Session.FetchedAt) * time.Second
	return fmt.Sprintf("session=present(age %s)", age.Round(time.Minute))
}

// deviceState reports the passport anchor by presence: an account whose device
// blob is missing will look like new hardware to the passport on the next login,
// which is exactly the state an operator needs to see (ADR 0013).
func deviceState(a accounts.Account) string {
	if len(a.Device) == 0 || a.Device["deviceId"] == "" {
		return "device=MISSING (a login would look like new hardware)"
	}
	return "device=present"
}

func cookieOf(a accounts.Account) string {
	if a.Session == nil {
		return ""
	}
	return a.Session.Cookie
}

// masked keeps the first and last two characters: enough to tell two records
// apart in a terminal, not enough to be a credential dump.
func masked(s string) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

func presence(s string) string {
	if s == "" {
		return "MISSING"
	}
	return fmt.Sprintf("present(%d chars)", len(s))
}

// accountsFlagValue extracts the value of `-accounts <cmd> ...`. command-line
// flags stop at the first non-flag argument, so everything after it belongs to
// the subcommand.
func accountsFlagValue(args []string) ([]string, bool) {
	for i, a := range args {
		if a == "-accounts" {
			if i+1 >= len(args) {
				return nil, true
			}
			return args[i+1:], true
		}
		if strings.HasPrefix(a, "-accounts=") {
			return append([]string{strings.TrimPrefix(a, "-accounts=")}, args[i+1:]...), true
		}
	}
	return nil, false
}
