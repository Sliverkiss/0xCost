// Command mimo2api is the OpenAI-compatible gateway binary (SDD §3, §6):
// env config → account store (file or Upstash, ADR 0018) → pool janitors +
// credential sweeper → HTTP server with graceful shutdown on SIGTERM/SIGINT.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/api"
	"github.com/yg/mimo2api/internal/config"
	"github.com/yg/mimo2api/internal/egress"
	"github.com/yg/mimo2api/internal/mimo"
	"github.com/yg/mimo2api/internal/pool"
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

	// -import-redis <accounts.json>: seed the Redis store (DS_REDIS_HOST) from
	// a file and exit. The hosted deployment has no durable file system, so
	// this is how a local accounts file becomes the remote pool — the same
	// flag the reference deployment uses (ADR 0018).
	importRedis := flag.String("import-redis", "", "import an accounts.json file into the configured Redis store, then exit")
	// Registered so flag.Parse accepts it; the command and its arguments are read
	// from argv (see accountsFlagValue) because a subcommand's own arguments
	// follow the flag as ordinary words.
	flag.Bool("accounts", false, "manage the account store; use: -accounts <command> (help lists them)")
	flag.Parse()
	if *importRedis != "" {
		if err := runImportRedis(*importRedis); err != nil {
			log.Fatalf("mimo2api: import-redis: %v", err)
		}
		return
	}

	// -accounts <cmd> [args]: manage the configured account store (file or
	// Upstash) without editing a file or opening a Redis console (ADR 0020).
	if args, ok := accountsFlagValue(os.Args[1:]); ok {
		if err := runAccountsCommand(args); err != nil {
			log.Fatalf("mimo2api: -accounts: %v", err)
		}
		return
	}

	if err := run(); err != nil {
		log.Fatalf("mimo2api: %v", err)
	}
}

// runImportRedis copies a local accounts file into the configured Redis store
// and exits. It is idempotent (per-account SET) and never touches the local
// file. Guarded on DS_REDIS_HOST being set, so it cannot silently seed a file
// backend with itself.
func runImportRedis(path string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.RedisHost() == "" {
		return fmt.Errorf("-import-redis requires DS_REDIS_HOST to be set (the local backend is already the file)")
	}
	backend, err := accounts.OpenUpstash(cfg.RedisHost(), cfg.RedisToken(), log.Default())
	if err != nil {
		return err
	}
	defer backend.Close()
	store, err := accounts.Load(path)
	if err != nil {
		return err
	}
	accts := store.Accounts()
	if len(accts) == 0 {
		return fmt.Errorf("%s holds no accounts", path)
	}
	for _, a := range accts {
		if err := backend.SaveAccount(context.Background(), a); err != nil {
			return err
		}
	}
	// Read back through the store contract: the import is only done when the
	// remote side answers with the records we just wrote (external effect, so
	// verify it rather than trusting the writes).
	verifier, err := accounts.LoadBackend(context.Background(), backend)
	if err != nil {
		return err
	}
	if got := len(verifier.Accounts()); got != len(accts) {
		return fmt.Errorf("verify: Redis holds %d accounts, imported %d", got, len(accts))
	}
	log.Printf("import-redis: %d accounts now in %s", len(accts), backend.Addr())
	return nil
}

// openStore chooses the account backend (ADR 0018): Redis when
// DS_REDIS_HOST is configured — a hosted deployment has no durable file
// system — otherwise the accounts file.
//
// A configured-but-unusable Redis is fatal: the operator named a source of
// truth, and starting on an empty file fallback would silently serve nobody
// (the same reasoning the reference deployment uses).
func openStore(cfg *config.Config) (*accounts.Store, error) {
	if cfg.RedisHost() == "" {
		store, err := accounts.Load(cfg.AccountsFile())
		if err != nil {
			return nil, err
		}
		log.Printf("accounts: file backend %s (%d accounts)", cfg.AccountsFile(), len(store.Accounts()))
		return store, nil
	}
	backend, err := accounts.OpenUpstash(cfg.RedisHost(), cfg.RedisToken(), log.Default())
	if err != nil {
		return nil, fmt.Errorf("DS_REDIS_HOST is set but the store is unusable — refusing to start on a stale file fallback: %w", err)
	}
	store, err := accounts.LoadBackend(context.Background(), backend)
	if err != nil {
		backend.Close()
		return nil, err
	}
	log.Printf("accounts: redis backend %s (%d accounts)", backend.Addr(), len(store.Accounts()))
	return store, nil
}

// run wires the stack and serves until a signal arrives.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	// Egress labels: accounts.json names them, the environment carries the
	// endpoints and their credentials. An account whose label has no endpoint is
	// a startup error naming it — a silent fallback would put that account's
	// traffic on the host's own IP, which is what the label exists to prevent
	// (SDD §4.8, REQ-16).
	labels := []string{}
	for _, a := range store.Accounts() {
		labels = append(labels, a.Egress)
	}
	egressProxies, err := egress.Resolve(egress.Labels(labels), os.Getenv)
	if err != nil {
		return err
	}
	if len(egressProxies) > 0 {
		log.Printf("egress: %d labelled egress(es) configured: %s", len(egressProxies), strings.Join(egress.Labels(labels), ", "))
	}

	client := mimo.New(mimo.Options{
		Base:                    cfg.UpstreamBase(),
		Egress:                  egressProxies,
		UserAgent:               cfg.UserAgent(),
		SecChUA:                 cfg.SecChUA(),
		Timezone:                cfg.Timezone(),
		IdleTimeout:             cfg.SSEIdleTimeout(),
		HTTP:                    &http.Client{Timeout: cfg.UpstreamTimeout()},
		DisableOTPAuto:          !cfg.OTPAuto(),
		DisableCrossRegionAgree: !cfg.CrossRegionAgree(),
	})

	// Pool (owner rulings 2026-10): two states only — available / disabled. There
	// is no probe sweeper, no cooldown, and no concurrency cap (2026-10-14,
	// "并发槽砍了"): failures pass through to the client, and the upstream decides
	// what it will serve.
	accts := store.Accounts()
	var poolAccts []accounts.Account
	for _, a := range accts {
		if a.Enabled {
			poolAccts = append(poolAccts, a)
		}
	}
	p := pool.New(poolAccts, pool.Options{
		// Risk-control signals are correlated across accounts on one egress
		// inside this window (SDD §4.5.2).
		RiskWindow: cfg.PoolStatsWindow(),
	})

	srv := api.New(api.Options{
		APIKeys:      cfg.APIKeys(),
		Pool:         p,
		Accounts:     store,
		Client:       client,
		DefaultModel: cfg.DefaultModel(),
		ReplayCap:    cfg.ReplayCap(),
		// Image input guards (IMG-*): 10 images / 20 MiB each by default.
		MaxImages:         cfg.MaxImages(),
		MaxImageBytes:     cfg.MaxImageBytes(),
		MaxImagePixels:    cfg.MaxImagePixels(),
		ImageAllowPrivate: cfg.ImageAllowPrivate(),
		Debug:             cfg.Debug(),
		// ADR 0019: a real client-facing budget and a per-turn upstream deadline.
		// The per-turn silence window (LeaseTTL) is request liveness, not slot
		// capacity — the slots are gone (owner ruling 2026-10-14).
		RequestTimeout: cfg.RequestTimeout(),
		LeaseTTL:       cfg.LeaseTTL(),

		// Audio surface (M18).
		AudioPoll:      cfg.AudioPoll(),
		TTSMaxTimeout:  cfg.TTSMaxTimeout(),
		CatalogTTL:     cfg.CatalogTTL(),
		TTSTimeout:     cfg.TTSTimeout(),
		ASRTimeout:     cfg.ASRTimeout(),
		DefaultVoice:   cfg.TTSVoice(),
	})

	// Warm the superseded-model map before serving. It is the deployment's view of
	// which model ids the upstream will actually run, and the request paths read
	// it from memory only — so nothing on a chat or audio request can be turned
	// into a catalog read (which could in turn demand a login).
	warmCtx, cancelWarm := context.WithTimeout(context.Background(), 20*time.Second)
	srv.RefreshModelRedirects(warmCtx)
	cancelWarm()

	// Background janitors (SDD §3): the model catalog refresher and the hourly
	// session-renew job. The probe sweeper is gone with cooldown (owner ruling
	// 2026-10), the slot reaper is gone with the slots (owner ruling
	// 2026-10-14), and session freshness moved off the request paths onto the
	// renew job ("定时巡检比被调用接口造成问题要稳定") — the job runs one round
	// immediately so the deployment starts on fresh sessions, then once per
	// interval.
	ctx, stopJanitors := context.WithCancel(context.Background())
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		probeTick := time.NewTicker(cfg.ProbeInterval())
		defer probeTick.Stop()
		renewDone := make(chan struct{})
		go func() {
			defer close(renewDone)
			srv.RunRenewJob(ctx, cfg.RenewInterval())
		}()
		for {
			select {
			case <-ctx.Done():
				<-renewDone
				return
			case <-probeTick.C:
				// Keep the superseded-id map current: an upstream that retires a
				// model mid-flight must not start failing requests that used to
				// work (TTL-guarded inside).
				srv.RefreshModelRedirects(ctx)
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
