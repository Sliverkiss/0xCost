// Package testutil provides shared test doubles: a fake clock, mock upstream
// and passport servers, SSE fixtures, and assertion helpers (TDD §2.2).
package testutil

import (
	"sync"
	"time"
)

// Clock is the injectable time source used by the session store and pool.
// Production wires RealClock; tests wire FakeClock and call Advance.
type Clock interface {
	Now() time.Time
}

// RealClock is the wall-clock production implementation.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a deterministic clock. Advance moves time forward.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a fake clock starting at a fixed base instant
// (2026-01-01T00:00:00Z — synthetic, never a real deadline).
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake clock forward by d and returns the new time.
func (c *FakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set rewinds/sets the clock to an absolute instant.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
