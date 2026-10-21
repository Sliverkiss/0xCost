// Package scheduler runs the pool maintenance loops for the disposable pool:
// periodic virgin-stock top-up and garbage collection of expired cartridges.
// There is no guest refresh loop (a cartridge serves one request) and no budget
// reconciliation (nothing meters identities any more).
package scheduler

import (
	"context"
	"log"
	"time"

	"chatglm2api/internal/pool"
)

// Config wires the scheduler to the pool.
type Config struct {
	Pool *pool.Pool
	// TopUpInterval is how often the virgin stock is compared to WarmSize.
	TopUpInterval time.Duration
	// GCInterval is how often expired idle identities are discarded.
	GCInterval time.Duration
	// DisableTopUp/GC turn individual loops off.
	DisableTopUp bool
	DisableGC    bool
}

// Scheduler runs maintenance loops until its context is cancelled.
type Scheduler struct {
	cfg Config
}

// New builds a scheduler with defaults for unset intervals.
func New(cfg Config) *Scheduler {
	if cfg.TopUpInterval <= 0 {
		cfg.TopUpInterval = 30 * time.Second
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Scheduler{cfg: cfg}
}

// Run blocks until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	var loops int
	if !s.cfg.DisableTopUp {
		go s.loop(ctx, s.cfg.TopUpInterval, "topup", s.TopUpOnce)
		loops++
	}
	if !s.cfg.DisableGC {
		go s.loop(ctx, s.cfg.GCInterval, "gc", s.GCOnce)
		loops++
	}
	log.Printf("[scheduler] running %d maintenance loop(s)", loops)
	<-ctx.Done()
}

func (s *Scheduler) loop(ctx context.Context, every time.Duration, name string, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// TopUpOnce refills the virgin stock to its target.
func (s *Scheduler) TopUpOnce(ctx context.Context) {
	if s.cfg.Pool == nil {
		return
	}
	s.cfg.Pool.Refill(ctx)
}

// GCOnce discards expired idle identities and re-queues cooled ones.
func (s *Scheduler) GCOnce(ctx context.Context) {
	if s.cfg.Pool == nil {
		return
	}
	s.cfg.Pool.GC()
}
