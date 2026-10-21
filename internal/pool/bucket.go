package pool

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// pacingGate is a floor-gap rate limiter: consecutive acquisition starts are
// separated by at least one interval (+ jitter). It is deliberately not a
// token bucket — a bucket lets a short burst through as long as the long-run
// average holds, and a burst is exactly the shape the upstream edge flags
// (docs/RESEARCH-pool-designs.md §4.3).
type pacingGate struct {
	mu       sync.Mutex
	interval time.Duration
	jitter   time.Duration
	next     time.Time // earliest instant for the next start
}

func newPacingGate(interval, jitter time.Duration) *pacingGate {
	if interval < 0 {
		interval = 0
	}
	if jitter < 0 {
		jitter = 0
	}
	return &pacingGate{interval: interval, jitter: jitter}
}

// Wait blocks until this caller may start an acquisition, or ctx ends. It
// returns false when the context ended first.
func (g *pacingGate) Wait(ctx context.Context) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	now := time.Now()
	slot := g.next
	if slot.Before(now) {
		slot = now
	}
	gap := g.interval
	if g.jitter > 0 {
		gap += randDuration(g.jitter)
	}
	g.next = slot.Add(gap)
	g.mu.Unlock()

	wait := time.Until(slot)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// randDuration returns a value in [-d, +d].
func randDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)*2+1)) - d
}
