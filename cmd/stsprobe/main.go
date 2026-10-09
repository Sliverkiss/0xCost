// stsprobe: live-renews a session from a stored passToken and prints which
// part of the landing chain produced the session cookies. It exists to
// regression-test the landing-chain walk (docs/login-chain-reverse.md §1)
// without burning an SMS: `go run ./cmd/stsprobe <accounts.json>`.
//
// Evidence contract: it prints the hop chain (status + host only, no query
// values), how many cookies the chain planted, and whether the composed
// session validates against the upstream.
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
		ID        string `json:"id"`
		UserID    string `json:"user_id"`
		PassToken string `json:"pass_token"`
		Enabled   bool   `json:"enabled"`
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
	var userID, passToken string
	for _, a := range af.Accounts {
		if a.Enabled && a.PassToken != "" {
			userID, passToken = a.UserID, a.PassToken
			break
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

	start := time.Now()
	sess, err := c.Renew(ctx, userID, passToken)
	if err != nil {
		fmt.Println("RENEW-ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("RENEW-OK in %s: userID=%s cookies=%d ph=%t\n",
		time.Since(start).Round(time.Millisecond), sess.UserID, len(strings.Split(sess.Cookie, "; ")), sess.PH != "")

	if err := c.Validate(ctx, sess.Cookie); err != nil {
		fmt.Println("VALIDATE-ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("VALIDATE-OK")
}
