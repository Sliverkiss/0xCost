// Command chatglm2api runs the anonymous OpenAI-compatible gateway to
// chatglm.cn: it prewarms the identity pool, starts the maintenance scheduler
// and serves the HTTP API.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"chatglm2api/internal/config"
	"chatglm2api/internal/egress"
	"chatglm2api/internal/pool"
	"chatglm2api/internal/scheduler"
	"chatglm2api/internal/server"
	"chatglm2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := config.LoadOptional(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	up := upstream.New(cfg.BaseURL)
	eg := egress.New(cfg.Egress)
	xff := eg.Next

	p := pool.New(pool.Config{
		Acquirer:           up,
		WarmSize:           cfg.Pool.WarmSize,
		MaxSize:            cfg.Pool.MaxSize,
		AcquireQPS:         cfg.Pace.AcquireQPS,
		AcquireInterval:    cfg.Pace.AcquireInterval.D(),
		AcquireJitter:      cfg.Pace.AcquireJitter.D(),
		AcquireConcurrency: cfg.Pool.AcquireConcurrency,
		BreakerThreshold:   cfg.Pool.BreakerThreshold,
		BreakerCooldown:    cfg.Pool.BreakerCooldown.D(),
		AcquireWait:        cfg.Pool.AcquireWait.D(),
		StateFile:          cfg.StateFile,
		PersistInterval:    cfg.Pool.PersistInterval.D(),
		XFF:                xff,
	})
	defer p.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Prewarm the virgin stock before the listener binds so the first client
	// request does not pay acquisition latency. A failed prewarm is not fatal:
	// the pool can still acquire on demand and the scheduler keeps retrying.
	if cfg.Pool.WarmSize > 0 {
		warmCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		start := time.Now()
		if err := p.Prewarm(warmCtx, cfg.Pool.WarmSize); err != nil {
			log.Printf("WARN: [main] prewarm: %v", err)
		} else {
			log.Printf("[main] prewarmed %d identities in %s", cfg.Pool.WarmSize, time.Since(start).Round(time.Millisecond))
		}
		cancel()
	}

	sch := scheduler.New(scheduler.Config{
		Pool:          p,
		TopUpInterval: 30 * time.Second,
		GCInterval:    5 * time.Minute,
	})
	go sch.Run(ctx)

	srv := server.New(server.Config{
		Config:   cfg,
		Pool:     p,
		Upstream: up,
		Egress:   eg,
		APIKey:   cfg.APIKey,
		ModelFetcher: func(ctx context.Context) ([]string, error) {
			return fetchModelIDs(ctx, p, up, xff)
		},
	})

	if cfg.Admin.Enabled {
		srv.EnableAdmin()
		log.Printf("[main] admin endpoints enabled (api_key required)")
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(shutdownCtx)
		cancel()
		// Wait briefly for background conversation cleanup before the process
		// exits; it is best effort, so a slow upstream must not hang shutdown.
		done := make(chan struct{})
		go func() { srv.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			log.Printf("WARN: [main] background cleanup still running at shutdown")
		}
	}()

	log.Printf("chatglm2api listening on %s (api_key=%v, warm_size=%d cards)",
		cfg.Listen, cfg.APIKey != "", cfg.Pool.WarmSize)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// fetchModelIDs reads the guest model catalogue through any idle identity on a
// best-effort basis. It never acquires a new identity: listing models must not
// consume pool capacity.
func fetchModelIDs(ctx context.Context, p *pool.Pool, up *upstream.Client, xff func() string) ([]string, error) {
	lease, ok := p.TrySelect()
	if !ok {
		return nil, pool.ErrNoIdentity
	}
	defer lease.Release()
	id := lease.Identity
	cat, err := up.FetchModels(ctx, id.AccessToken, id.DeviceID, id.UserAgent(), xff())
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cat.Models))
	for _, m := range cat.Models {
		if m.SelectedModel != "" {
			ids = append(ids, m.SelectedModel)
		}
	}
	return ids, nil
}
