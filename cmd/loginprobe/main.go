// loginprobe: one password-login attempt against the live passport, used to
// answer a single question — does the upstream still demand new-device
// verification for this account's device?
//
// Safety: this probe NEVER dispatches an SMS. The client is built with
// DisableOTPAuto, so a verification demand is reported (exit 2) instead of
// auto-starting a challenge. Completing a login is the gateway's job:
// POST /v1/admin/otp/submit with the operator's own code.
//
// Credentials come from the accounts file, never from argv: a password on a
// command line lands in the process table and the shell history.
//
// Exit codes: 0 login OK · 2 verification demanded (no SMS sent) · 1 error.
//
// `go run ./cmd/loginprobe [accounts.json] [account-id]`
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/mimo"
)

type probeAccount struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	Phone     string            `json:"phone"`
	Password  string            `json:"password"`
	UserID    string            `json:"user_id"`
	PassToken string            `json:"pass_token"`
	Enabled   bool              `json:"enabled"`
	Device    map[string]string `json:"device"`
}

type accountFile struct {
	Accounts []probeAccount `json:"accounts"`
}

func main() {
	path := "data/accounts.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	wantID := ""
	if len(os.Args) > 2 {
		wantID = os.Args[2]
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("read accounts:", err)
		os.Exit(1)
	}
	var af accountFile
	if err := json.Unmarshal(raw, &af); err != nil {
		fmt.Println("parse accounts:", err)
		os.Exit(1)
	}
	var pick *probeAccount
	for i := range af.Accounts {
		a := &af.Accounts[i]
		if a.Password == "" || (wantID != "" && a.ID != wantID) {
			continue
		}
		pick = a
		if a.Enabled {
			break
		}
	}
	if pick == nil {
		fmt.Println("no account with a password found (and matching id, if given)")
		os.Exit(1)
	}

	fmt.Printf("account=%s label=%s stored_device=%v (values withheld)\n",
		pick.ID, pick.Label, sortedKeys(pick.Device))

	cfg, _ := config.Load()
	c := mimo.New(mimo.Options{
		Base:           cfg.UpstreamBase(),
		UserAgent:      cfg.UserAgent(),
		SecChUA:        cfg.SecChUA(),
		Timezone:       cfg.Timezone(),
		DisableOTPAuto: true, // hard-coded safety: never burn an SMS from a probe
	}).WithDevice(pick.Device)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	sess, err := c.Login(ctx, pick.Phone, pick.Password)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		var action *mimo.ErrAccountAction
		if errors.As(err, &action) {
			fmt.Printf("VERIFICATION-DEMANDED in %s (no SMS sent): %s\n", took, action.Reason)
			os.Exit(2)
		}
		fmt.Printf("LOGIN-ERROR in %s: %v\n", took, err)
		os.Exit(1)
	}

	fmt.Printf("LOGIN-OK in %s: userID=%s session_cookies=%d ph=%t\n",
		took, sess.UserID, len(strings.Split(sess.Cookie, "; ")), sess.PH != "")
	fmt.Println("  no new-device verification demanded for this device")
	if rotated := rotatedNames(pick.Device, sess.Device); len(rotated) > 0 {
		fmt.Printf("  device identity: %v (rotated by this login)\n", rotated)
	} else {
		fmt.Printf("  device identity: %v (unchanged)\n", sortedKeys(sess.Device))
	}
	if err := c.Validate(ctx, sess.Cookie); err != nil {
		fmt.Println("VALIDATE-ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("VALIDATE-OK")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func rotatedNames(stored, fresh map[string]string) []string {
	var out []string
	for k, v := range fresh {
		if old, ok := stored[k]; ok && old != v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
