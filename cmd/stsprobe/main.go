// stsprobe: live-renews a session from a stored passToken and prints which
// part of the landing chain produced the session cookies. It exists to
// regression-test the landing-chain walk (docs/login-chain-reverse.md §1)
// without burning an SMS: `go run ./cmd/stsprobe <accounts.json>`.
//
// Evidence contract: how many cookies the chain planted, the identity fields
// it yielded, and whether the composed session validates against the upstream.
// With PERSIST=1 it writes the renewed session back into the accounts file
// (the operator escape hatch when a login attempt burned an OTP and the
// gateway has no session left).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/mimo"
)

type accountFile struct {
	Accounts []struct {
		ID        string            `json:"id"`
		UserID    string            `json:"user_id"`
		PassToken string            `json:"pass_token"`
		Enabled   bool              `json:"enabled"`
		Device    map[string]string `json:"device"`
	} `json:"accounts"`
}

func main() {
	path := "data/accounts.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
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
	var userID, passToken, acctID string
	var device map[string]string
	for _, a := range af.Accounts {
		if a.PassToken != "" && (userID == "" || a.Enabled) {
			userID, passToken, acctID, device = a.UserID, a.PassToken, a.ID, a.Device
			if a.Enabled {
				break
			}
		}
	}
	if passToken == "" {
		fmt.Println("no enabled account with a pass_token")
		os.Exit(1)
	}

	cfg, _ := config.Load()
	c := mimo.New(mimo.Options{
		Base:      cfg.UpstreamBase(),
		UserAgent: cfg.UserAgent(),
		SecChUA:   cfg.SecChUA(),
		Timezone:  cfg.Timezone(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	devNames := make([]string, 0, len(device))
	for k := range device {
		devNames = append(devNames, k)
	}
	fmt.Printf("stored device identity: %v (presented on every hop)\n", devNames)

	start := time.Now()
	sess, err := c.WithDevice(device).Renew(ctx, userID, passToken)
	if err != nil {
		fmt.Println("RENEW-ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("RENEW-OK in %s: userID=%s cookies=%d ph=%t\n",
		time.Since(start).Round(time.Millisecond), sess.UserID, len(strings.Split(sess.Cookie, "; ")), sess.PH != "")
	harvested := make([]string, 0, len(sess.Device))
	for k, v := range sess.Device {
		if old, ok := device[k]; ok && old != v {
			harvested = append(harvested, k+"(rotated)")
			continue
		}
		harvested = append(harvested, k)
	}
	fmt.Printf("device identity after renewal: %v\n", harvested)

	if err := c.Validate(ctx, sess.Cookie); err != nil {
		fmt.Println("VALIDATE-ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("VALIDATE-OK")

	if os.Getenv("PERSIST") != "1" {
		return
	}
	// Persist surgically: touch only this account's session/identity fields so
	// unrelated keys (label, enabled, cooldown) survive.
	var rawDoc map[string]any
	if err := json.Unmarshal(raw, &rawDoc); err != nil {
		fmt.Println("persist: parse:", err)
		os.Exit(1)
	}
	list, _ := rawDoc["accounts"].([]any)
	wrote := false
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok || m["id"] != acctID {
			continue
		}
		m["session"] = map[string]any{"cookie": sess.Cookie, "ph": sess.PH, "fetched_at": time.Now().Unix()}
		m["user_id"] = sess.UserID
		m["pass_token"] = sess.PassToken
		if len(sess.Device) > 0 {
			m["device"] = sess.Device
		}
		m["state"] = "available"
		wrote = true
	}
	if !wrote {
		fmt.Println("persist: account not found in file")
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(rawDoc, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		fmt.Println("persist:", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Println("persist:", err)
		os.Exit(1)
	}
	fmt.Println("PERSISTED session to", path)
}
