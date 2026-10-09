// Package pool implements the account pool: states (available / cooldown /
// disabled), score-based selection with round-robin fairness, the cooldown
// ladder, per-account in-flight semaphores, and the recovery probe gate
// (SDD §4).
package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
)

// State is an account's health state (SDD §4.1).
type State string

const (
	StateAvailable State = "available"
	StateCooldown  State = "cooldown"
	StateDisabled  State = "disabled"
)

// ErrNoAccounts: pool empty or all disabled → 503 no_accounts (SDD §7.4).
var ErrNoAccounts = errors.New("no accounts available: pool is empty or all accounts are disabled")

// ErrParked: all candidate accounts cooling down → 429 pool_parked with a
// Retry-After derived from the nearest cooldown expiry (SDD §7.4).
type ErrParked struct {
	RetryAfter time.Duration
}

// RetryAfterSeconds clamps to at least 1s for the Retry-After header.
func (e *ErrParked) RetryAfterSeconds() int { return clampSeconds(e.RetryAfter) }

func (e *ErrParked) Error() string {
	return fmt.Sprintf("all accounts are cooling down; nearest recovery in %s", e.RetryAfter.Round(time.Second))
}

// ErrBusy: all in-flight slots busy beyond the queue wait → 429 pool_busy
// (SDD §7.4).
type ErrBusy struct {
	RetryAfter time.Duration
}

// RetryAfterSeconds clamps to at least 1s for the Retry-After header.
func (e *ErrBusy) RetryAfterSeconds() int { return clampSeconds(e.RetryAfter) }

func (e *ErrBusy) Error() string {
	return fmt.Sprintf("all account slots busy; retry after %s", e.RetryAfter.Round(time.Second))
}

func clampSeconds(d time.Duration) int {
	s := int(d / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// ProbeFunc is the injected recovery probe (SDD §4.5). Wave 5 wires the real
// mimo session check; returning nil means the account session is healthy.
// An auth-expired error (AuthError) triggers exactly one renewal first.
type ProbeFunc func(ctx context.Context, accountID string) error

// RenewFunc performs one STS renewal attempt (wave 5 wires the real client).
type RenewFunc func(ctx context.Context, accountID string) error

// authError marks probe errors that warrant an STS renewal attempt.
type authError struct{ err error }

func (authError) Error() string       { return "auth expired" }
func (a authError) Unwrap() error     { return a.err }
func (authError) IsAuthExpired() bool { return true }

// AuthError wraps an error as an auth-expired signal (used by the mimo
// client and probes).
func AuthError(err error) error { return authError{err} }

func isAuthErr(err error) bool {
	var ae interface{ IsAuthExpired() bool }
	return errors.As(err, &ae) && ae.IsAuthExpired()
}

// Options configures the pool. Zero fields take SDD defaults.
type Options struct {
	MaxInflight int           // per-account semaphore (default 2, SDD §4.4)
	QueueWait   time.Duration // acquire wait bound (default 15s)
	Cooldown429 time.Duration // first cooldown on 429 (default 60s)
	Cooldown5xx time.Duration // first cooldown on 5xx/transport (default 30s)
	CooldownCap time.Duration // ladder cap (default 10m)
	WarmPenalty float64       // selection penalty for accounts with inflight work
	Now         func() time.Time
}

type account struct {
	id            string
	state         State
	cooldownUntil time.Time
	disabled      bool
	permanent     bool // 461: never auto-recovers (SDD §4.6, TDD POOL-012)
	reason        string
	failStreak    int           // consecutive cooldown-triggering failures
	sem           chan struct{} // in-flight semaphore; len() is the inflight count
	ewmaLatency   float64       // seconds, EWMA alpha 0.2 (SDD §4.4)
}

// Pool is the account pool. One mutex guards selection and state; in-flight
// slots are buffered channels, so Release needs no lock (SDD §3: pool ops
// are microseconds).
type Pool struct {
	mu          sync.Mutex
	accounts    []*account // stable order (selection tie-break)
	byID        map[string]*account
	cursor      int // round-robin rotation within the selection band
	maxInflight int
	queueWait   time.Duration
	cd429       time.Duration
	cd5xx       time.Duration
	cdCap       time.Duration
	warmPenalty float64
	now         func() time.Time
	probe       ProbeFunc
	renew       RenewFunc
	notify      chan struct{} // release signal for Acquire waiters (buffered 1)
}

// New builds a pool from account records. Only enabled entries become pool
// members; persisted runtime state (state / cooldown_until /
// disabled_reason) is honored so cooldowns and bans survive restart
// (SDD §7.5, TDD POOL-013).
func New(accts []accounts.Account, opts Options, probe ProbeFunc, renew RenewFunc) *Pool {
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = 2
	}
	if opts.QueueWait <= 0 {
		opts.QueueWait = 15 * time.Second
	}
	if opts.Cooldown429 <= 0 {
		opts.Cooldown429 = 60 * time.Second
	}
	if opts.Cooldown5xx <= 0 {
		opts.Cooldown5xx = 30 * time.Second
	}
	if opts.CooldownCap <= 0 {
		opts.CooldownCap = 10 * time.Minute
	}
	if opts.WarmPenalty <= 0 {
		opts.WarmPenalty = 0.5
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Pool{
		maxInflight: opts.MaxInflight,
		queueWait:   opts.QueueWait,
		cd429:       opts.Cooldown429,
		cd5xx:       opts.Cooldown5xx,
		cdCap:       opts.CooldownCap,
		warmPenalty: opts.WarmPenalty,
		now:         opts.Now,
		probe:       probe,
		renew:       renew,
		notify:      make(chan struct{}, 1),
		byID:        make(map[string]*account),
	}
	for _, a := range accts {
		if !a.Enabled {
			continue
		}
		acc := &account{
			id:  a.ID,
			sem: make(chan struct{}, opts.MaxInflight),
		}
		switch State(a.State) {
		case StateDisabled:
			acc.state = StateDisabled
			acc.disabled = true
			acc.reason = a.DisabledReason
		case StateCooldown:
			acc.state = StateCooldown
			acc.cooldownUntil = time.Unix(a.CooldownUntil, 0)
		default:
			acc.state = StateAvailable
		}
		p.accounts = append(p.accounts, acc)
		p.byID[a.ID] = acc
	}
	return p
}

// errBusy is the internal "candidates exist but all slots are full" signal —
// waitable, unlike parked/no-accounts which are terminal for the call.
var errBusy = errors.New("pool: all in-flight slots busy")

// Lease is a held in-flight slot on one account.
type Lease struct {
	Pool *Pool
	ID   string
}

// Release frees the in-flight slot and wakes one Acquire waiter.
func (l *Lease) Release() {
	if l == nil || l.Pool == nil {
		return
	}
	p := l.Pool
	p.mu.Lock()
	a := p.byID[l.ID]
	p.mu.Unlock()
	if a == nil {
		return
	}
	select {
	case <-a.sem: // drain this lease's token (exactly one per lease)
	default: // defensive: never block on double-Release
	}
	select {
	case p.notify <- struct{}{}: // wake one waiter
	default:
	}
}

// Acquire selects an account and claims one in-flight slot. Selection is
// score-based with a round-robin rotation inside the best band (SDD §4.4):
// score = (inflight/capacity)*100 + ewma_latency + warm_penalty; accounts
// within best*1.15+1 share the load evenly (TDD POOL-001/015).
// excludeIDs (retry-on-another-account, SDD §7.2) are skipped entirely.
// Waits up to queueWait (or the context deadline) for a free slot, then
// fails closed with ErrBusy. Terminal failures: ErrParked (all cooling),
// ErrNoAccounts (empty/all-disabled) — both mapped by the API layer
// (SDD §7.4).
func (p *Pool) Acquire(ctx context.Context, excludeIDs ...string) (*Lease, error) {
	// Queue waiting is wall-clock bounded (real time), independent of the
	// injectable clock used for cooldown math.
	deadline := time.Now().Add(p.queueWait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		p.mu.Lock()
		lease, err := p.tryAcquireLocked(excludeIDs)
		p.mu.Unlock()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, errBusy) {
			return nil, err // parked / no-accounts: terminal
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, &ErrBusy{RetryAfter: p.queueWait}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err() // no slot held: nothing leaks (TDD POOL-016)
		case <-p.notify:
			timer.Stop()
		case <-timer.C:
			// Loop once more; the next pass sees the expired deadline.
		}
	}
}

// AcquireSticky acquires a specific account (sticky-session path, SDD
// §4.3). When the target is not selectable or has no free slot, falls back
// to a general Acquire; the bool reports whether the sticky target was won.
func (p *Pool) AcquireSticky(ctx context.Context, accountID string) (*Lease, bool, error) {
	p.mu.Lock()
	a := p.byID[accountID]
	var lease *Lease
	if a != nil && !a.disabled && a.state == StateAvailable && len(a.sem) < cap(a.sem) {
		select {
		case a.sem <- struct{}{}:
			lease = &Lease{Pool: p, ID: a.id}
		default:
		}
	}
	p.mu.Unlock()
	if lease != nil {
		return lease, true, nil
	}
	l, err := p.Acquire(ctx)
	return l, false, err
}

// tryAcquireLocked attempts an immediate selection. Callers hold p.mu.
func (p *Pool) tryAcquireLocked(excludeIDs []string) (*Lease, error) {
	now := p.now()
	excl := make(map[string]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excl[id] = true
	}

	type scored struct {
		a     *account
		score float64
	}
	var candidates []scored
	var nearest time.Time
	availableExists := false
	enabled := 0
	for _, a := range p.accounts {
		if excl[a.id] || a.disabled {
			continue
		}
		enabled++
		if a.state != StateAvailable {
			// Cooldown expiry alone is NOT selectable: the probe gate
			// applies first (SDD §4.5, TDD POOL-006).
			if nearest.IsZero() || a.cooldownUntil.Before(nearest) {
				nearest = a.cooldownUntil
			}
			continue
		}
		availableExists = true
		if len(a.sem) >= cap(a.sem) {
			continue // full: contributes to busy, not to candidates
		}
		score := float64(len(a.sem))/float64(p.maxInflight)*100 + a.ewmaLatency
		if len(a.sem) > 0 {
			score += p.warmPenalty
		}
		candidates = append(candidates, scored{a, score})
	}
	if enabled == 0 {
		return nil, ErrNoAccounts // empty pool / all disabled (TDD POOL-011)
	}
	if len(candidates) == 0 {
		if availableExists {
			return nil, errBusy // slots may free up: waitable
		}
		if nearest.IsZero() {
			nearest = now.Add(p.cd429)
		}
		return nil, &ErrParked{RetryAfter: nearest.Sub(now)}
	}

	// Band selection: best*1.15+1, deterministic order (stable account
	// order), round-robin rotation inside the band for fairness.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score < candidates[j].score })
	threshold := candidates[0].score*1.15 + 1
	end := 0
	for end < len(candidates) && candidates[end].score <= threshold {
		end++
	}
	band := candidates[:end]
	p.cursor++
	pick := band[p.cursor%len(band)].a

	// Claim under the pool lock so selection and slot-taking are atomic
	// with respect to other Acquires (Release takes no lock, so capacity
	// can only grow between the check and the claim).
	select {
	case pick.sem <- struct{}{}:
	default:
		return nil, errBusy
	}
	return &Lease{Pool: p, ID: pick.id}, nil
}

// RecordFirstByte feeds first-byte latency into the EWMA selection term
// (SDD §4.4).
func (p *Pool) RecordFirstByte(accountID string, elapsed time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		a.ewmaLatency = 0.8*a.ewmaLatency + 0.2*elapsed.Seconds()
	}
}

// ReportSuccess resets the cooldown ladder (SDD §4.5, TDD POOL-004).
func (p *Pool) ReportSuccess(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		a.failStreak = 0
	}
}

// Report429 applies the rate-limit cooldown ladder: base 60s doubling to the
// 10m cap (SDD §4.5, TDD POOL-002/003).
func (p *Pool) Report429(accountID string) {
	p.cooldown(accountID, p.cd429)
}

// Report5xx applies the upstream-sick ladder: base 30s doubling to the cap
// (SDD §4.5, TDD POOL-005). Transport errors are treated the same.
func (p *Pool) Report5xx(accountID string) {
	p.cooldown(accountID, p.cd5xx)
}

func (p *Pool) cooldown(accountID string, base time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil || a.disabled {
		return
	}
	a.failStreak++
	d := base
	for i := 1; i < a.failStreak; i++ {
		d *= 2
		if d >= p.cdCap || d <= 0 {
			d = p.cdCap
			break
		}
	}
	a.state = StateCooldown
	a.cooldownUntil = p.now().Add(d)
}

// ReportDisabled disables an account with a reason. Disabled accounts are
// never auto-probed; permanent (461) is recorded as such (SDD §4.6, TDD
// POOL-012). Recovery is an operator action (re-enable in the accounts
// file).
func (p *Pool) ReportDisabled(accountID, reason string, permanent bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil {
		return
	}
	a.state = StateDisabled
	a.disabled = true
	a.permanent = permanent
	a.reason = reason
	select {
	case p.notify <- struct{}{}: // waiters must re-evaluate (may now park)
	default:
	}
}

// Enable clears disabled/cooldown state for an account and makes it
// selectable again (SDD §2.2 amendment: the operator completed the
// new-device OTP verification via /v1/admin/otp/submit). Idempotent.
func (p *Pool) Enable(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil {
		return
	}
	a.disabled = false
	a.permanent = false
	a.reason = ""
	a.failStreak = 0
	a.cooldownUntil = time.Time{}
	a.state = StateAvailable
	select {
	case p.notify <- struct{}{}: // waiters re-evaluate (may unpark)
	default:
	}
}

// ProbeSweep runs recovery probes over cooldown-expired accounts (SDD
// §4.5, TDD POOL-006/007): probe OK → available; probe auth-expired →
// exactly one STS renewal, success → available, failure → disabled; probe
// sick (5xx/transport) → stay cooling, re-armed at the 5xx base. Disabled
// accounts are never probed (TDD POOL-012). Returns the recovered count.
func (p *Pool) ProbeSweep(ctx context.Context) int {
	p.mu.Lock()
	now := p.now()
	var due []*account
	for _, a := range p.accounts {
		if a.disabled {
			continue
		}
		if a.state == StateCooldown && now.After(a.cooldownUntil) {
			due = append(due, a)
		}
	}
	p.mu.Unlock()

	recovered := 0
	for _, a := range due {
		var err error
		if p.probe != nil {
			err = p.probe(ctx, a.id)
		}
		p.mu.Lock()
		if a.disabled || a.state != StateCooldown {
			p.mu.Unlock()
			continue // state changed under us; leave it
		}
		switch {
		case err == nil:
			a.state = StateAvailable
			a.failStreak = 0
			recovered++
		case isAuthErr(err) && p.renew != nil:
			p.mu.Unlock()
			rerr := p.renew(ctx, a.id) // exactly one renewal attempt
			p.mu.Lock()
			if a.disabled || a.state != StateCooldown {
				p.mu.Unlock()
				continue
			}
			if rerr == nil {
				a.state = StateAvailable
				a.failStreak = 0
				recovered++
			} else {
				a.state = StateDisabled
				a.disabled = true
				a.reason = "session renewal failed: " + rerr.Error()
			}
		default:
			// Probe sick: keep cooling (ladder continues on next failure).
			a.cooldownUntil = p.now().Add(p.cd5xx)
		}
		p.mu.Unlock()
	}
	if recovered > 0 {
		select {
		case p.notify <- struct{}{}:
		default:
		}
	}
	return recovered
}

// RuntimeState returns the persisted-form state for the accounts file
// (SDD §7.5): state, cooldown_until (unix seconds), disabled_reason.
func (p *Pool) RuntimeState(accountID string) (State, int64, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil {
		return StateDisabled, 0, ""
	}
	return a.state, a.cooldownUntil.Unix(), a.reason
}

// Summary is the /healthz pool summary: state counts only, no credentials
// (SDD §5.1).
type Summary struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Cooldown  int `json:"cooldown"`
	Disabled  int `json:"disabled"`
}

// Summarize returns state counts.
func (p *Pool) Summarize() Summary {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Summary{Total: len(p.accounts)}
	for _, a := range p.accounts {
		switch {
		case a.disabled:
			s.Disabled++
		case a.state == StateCooldown:
			s.Cooldown++
		default:
			s.Available++
		}
	}
	return s
}

// IDs returns all pool member ids (introspection/tests).
func (p *Pool) IDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.accounts))
	for i, a := range p.accounts {
		out[i] = a.id
	}
	return out
}

// StateOf returns the account's current state.
func (p *Pool) StateOf(accountID string) State {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		return a.state
	}
	return StateDisabled
}

// CooldownUntilOf returns the current cooldown deadline (tests).
func (p *Pool) CooldownUntilOf(accountID string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		return a.cooldownUntil
	}
	return time.Time{}
}
