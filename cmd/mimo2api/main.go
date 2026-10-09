// Command mimo2api is the OpenAI-compatible gateway binary (SDD §3, §6):
// env config → accounts file → pool janitors + session sweeper → HTTP server
// with graceful shutdown on SIGTERM/SIGINT so in-flight streams drain.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/api"
	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
	"github.com/yg/mimo2api/internal/session"
)

// shutdownGrace bounds the drain window after SIGTERM/SIGINT: streams get
// this long to finish before the process exits (SDD §3).
const shutdownGrace = 30 * time.Second

func main() {
	// healthcheck subcommand: GET /healthz against the configured addr,
	// exit 0/1 (SDD §6 — distroless has no curl/wget).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		log.Fatalf("mimo2api: %v", err)
	}
}

// run wires the stack and serves until a signal arrives.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	store, err := accounts.Load(cfg.AccountsFile())
	if err != nil {
		return err
	}

	client := mimo.New(mimo.Options{
		Base:                    cfg.UpstreamBase(),
		UserAgent:               cfg.UserAgent(),
		SecChUA:                 cfg.SecChUA(),
		Timezone:                cfg.Timezone(),
		IdleTimeout:             cfg.SSEIdleTimeout(),
		DisableOTPAuto:          !cfg.OTPAuto(),
		DisableCrossRegionAgree: !cfg.CrossRegionAgree(),
	})

	// Pool with the real probe (session check → available, SDD §4.5) and
	// renewer (STS refresh on auth-expired probes, SDD §2.3).
	probe := func(ctx context.Context, accountID string) error {
		acct, ok := store.Get(accountID)
		if !ok {
			return fmt.Errorf("account %s not found", accountID)
		}
		if acct.Session == nil || acct.Session.Cookie == "" {
			return pool.AuthError(fmt.Errorf("account %s has no cached session", accountID))
		}
		return client.Validate(ctx, acct.Session.Cookie)
	}
	renew := func(ctx context.Context, accountID string) error {
		acct, ok := store.Get(accountID)
		if !ok {
			return fmt.Errorf("account %s not found", accountID)
		}
		if acct.UserID == "" || acct.PassToken == "" {
			return fmt.Errorf("account %s has no passToken to renew", accountID)
		}
		sess, rerr := client.Renew(ctx, acct.UserID, acct.PassToken)
		if rerr != nil {
			return rerr
		}
		acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
		if uerr := store.Update(acct); uerr != nil {
			log.Printf("accounts: persist renewal for %s: %v", accountID, uerr)
		}
		return nil
	}

	accts := store.Accounts()
	var poolAccts []accounts.Account
	for _, a := range accts {
		if a.Enabled {
			poolAccts = append(poolAccts, a)
		}
	}
	p := pool.New(poolAccts, pool.Options{
		MaxInflight: cfg.MaxInflight(),
		QueueWait:   cfg.QueueWait(),
		Cooldown429: cfg.Cooldown429(),
		Cooldown5xx: cfg.Cooldown5xx(),
		CooldownCap: cfg.CooldownCap(),
	}, probe, renew)

	sessions := session.New(session.Options{
		TTL: cfg.StickyTTL(),
		Max: cfg.StickyMax(),
	})

	srv := api.New(api.Options{
		APIKeys:      cfg.APIKeys(),
		Pool:         p,
		Accounts:     store,
		Sessions:     sessions,
		Client:       client,
		DefaultModel: cfg.DefaultModel(),
		ReplayCap:    cfg.ReplayCap(),
		Keepalive:    cfg.Keepalive(),
		Debug:        cfg.Debug(),
	})

	// Background janitors (SDD §3): recovery probes every probeInterval,
	// session sweep on the same loop, plus a 6h STS keepalive sweep (§2.3).
	ctx, stopJanitors := context.WithCancel(context.Background())
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		probeTick := time.NewTicker(cfg.ProbeInterval())
		defer probeTick.Stop()
		refreshTick := time.NewTicker(cfg.SessionRefresh())
		defer refreshTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-probeTick.C:
				if n := p.ProbeSweep(ctx); n > 0 {
					log.Printf("pool: recovered %d account(s)", n)
				}
				if n := sessions.Sweep(); n > 0 {
					log.Printf("session: swept %d expired entr%s", n, plural(n))
				}
			case <-refreshTick.C:
				stsSweep(ctx, store, client)
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind first so a port clash is reported before we announce a listener.
	ln, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		stopJanitors()
		<-janitorDone
		return err
	}
	log.Printf("mimo2api: listening on %s (accounts: %d enabled, api keys: %d)", ln.Addr(), len(poolAccts), len(cfg.APIKeys()))

	// Serve until SIGTERM/SIGINT, then drain in-flight streams (SDD §3,
	// TDD API-025) — see serveUntil for the drain contract.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("mimo2api: %s received, draining in-flight streams (max %s)", sig, shutdownGrace)
		cancel()
	}()

	serveErr := serveUntil(ctx, httpSrv, ln, shutdownGrace, log.Printf)
	stopJanitors()
	<-janitorDone
	return serveErr
}

// stsSweep renews cached sessions so pool accounts never ride a stale
// serviceToken into a request (SDD §2.3 timer sweep, session.js:203-230
// analogue).
func stsSweep(ctx context.Context, store *accounts.Store, client *mimo.Client) {
	for _, acct := range store.Accounts() {
		if !acct.Enabled || acct.UserID == "" || acct.PassToken == "" {
			continue
		}
		if acct.Session == nil {
			continue
		}
		sess, err := client.Renew(ctx, acct.UserID, acct.PassToken)
		if err != nil {
			log.Printf("accounts: STS keepalive for %s: %v", acct.ID, err)
			continue
		}
		acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
		if err := store.Update(acct); err != nil {
			log.Printf("accounts: persist STS keepalive for %s: %v", acct.ID, err)
		}
	}
}

// runHealthcheck does GET /healthz against the configured addr and returns
// the process exit code: 0 healthy, 1 not (SDD §6, TDD DOCKER-002).
func runHealthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mimo2api healthcheck: config: %v\n", err)
		return 1
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + healthAddr(cfg) + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mimo2api healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "mimo2api healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// healthAddr maps the configured host to a dial target: a 0.0.0.0 bind is
// probed via 127.0.0.1 (same machine, SDD §6 note).
func healthAddr(cfg *config.Config) string {
	host := cfg.Host()
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, cfg.Port())
}

// plural returns "y"/"ies" for the swept-entries log line.
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
