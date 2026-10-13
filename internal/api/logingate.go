package api

// loginGate serializes the login chain per account: one password login at a
// time per account, ever (BUGS-FOUND #2's rule, kept for the renew job): two
// concurrent password logins from one IP is exactly the pattern risk control
// reacts to. With the request paths cut out of credential work, the gate's
// remaining users are the hourly renew job (renewjob.go) and the OTP admin
// submit flow.
//
// Lock ordering, deliberately: g.mu is a leaf — it guards only the flight
// map and is never held across I/O. What IS held across the login is the
// flight itself, and a flight is always bounded by its winner's own context
// (the renew job's per-account timeout). Losers wait on the flight's done
// channel with their own ctx in the select. No code path holds pool.mu while
// acquiring a flight, and the winner's pool mutations (Enable/
// ReportDisabled) are leaf acquisitions released before the flight ends, so
// no lock cycle exists.

import (
	"context"
	"sync"
)

type loginGate struct {
	mu      sync.Mutex
	flights map[string]*loginFlight
}

// loginFlight is one in-flight credential resolution for one account. done
// is closed (and the flight unregistered) by the winner when its attempt
// ends; waiters already holding the pointer still wake on the close.
type loginFlight struct {
	done chan struct{}
}

// acquire reserves the account's login chain. The first caller wins: it must
// run the resolution and call the returned finish exactly once. Concurrent
// callers lose: they wait — bounded by their own ctx, never by the winner —
// and re-read the state the winner left.
func (g *loginGate) acquire(ctx context.Context, accountID string) (won bool, finish func(), err error) {
	g.mu.Lock()
	if g.flights == nil {
		g.flights = make(map[string]*loginFlight)
	}
	fl, ok := g.flights[accountID]
	if !ok {
		fl = &loginFlight{done: make(chan struct{})}
		g.flights[accountID] = fl
		g.mu.Unlock()
		return true, func() { g.release(accountID, fl) }, nil
	}
	g.mu.Unlock()
	select {
	case <-fl.done:
		return false, nil, nil
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

// release ends a flight: the entry is dropped from the map BEFORE done is
// closed, so a caller arriving after the close starts a fresh flight
// instead of finding a finished one. Waiters that already hold the pointer
// wake on the close regardless.
func (g *loginGate) release(accountID string, fl *loginFlight) {
	g.mu.Lock()
	if g.flights[accountID] == fl {
		delete(g.flights, accountID)
	}
	g.mu.Unlock()
	close(fl.done)
}
