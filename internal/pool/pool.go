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

// FailureClass names the kind of upstream failure that cooled an account down
// (SDD §4.5.1, spec §4). Each class owns its counter and its ladder: a transport
// failure must never advance the 5xx ladder, and after the fact an operator must
// be able to tell which one cooled the account.
type FailureClass string

const (
	// ClassTransport: connection error, timeout, SSE idle-stall, EOF mid-body —
	// failures with no HTTP response, which may be network- or egress-shaped
	// rather than the account's fault.
	ClassTransport FailureClass = "transport"
	// ClassUpstream5xx: the upstream answered 5xx (or a non-SSE 200 body).
	ClassUpstream5xx FailureClass = "upstream-5xx"
	// ClassRiskControl: a 451/461. Deliberately NOT a ladder class — it disables
	// the account instead of cooling it (SDD §4.5.2) — and it exists so the
	// snapshot can say what the account last hit. ReportFailure with this class
	// would fall back to the 5xx ladder, so nothing may report it as a failure.
	ClassRiskControl FailureClass = "risk-control"
	// ClassThrottle429: the upstream answered 429.
	ClassThrottle429 FailureClass = "throttle-429"
)

// FailureClasses is the full set, in a stable order (the /v1/admin/pool view
// renders every class, present or not).
func FailureClasses() []FailureClass {
	return []FailureClass{ClassTransport, ClassUpstream5xx, ClassThrottle429}
}

// DefaultEgress is the label of the shared, unlabelled egress. Accounts without
// an egress label are behind it (SDD §4.8).
const DefaultEgress = "direct"

// RiskEvent is one risk-control signal (451/461) recorded for classification
// (SDD §4.5.2, spec §5). The response body itself is never stored — only a digest
// — and no credential appears here (INV-4).
type RiskEvent struct {
	AccountID  string    `json:"account_id"`
	Egress     string    `json:"egress"`
	Model      string    `json:"model,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	Status     int       `json:"status"`
	BodyDigest string    `json:"body_digest,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at"`
}

// riskEventCap bounds the recorded history. It is evidence, so it is never
// erased by time (RISK-006) — only by this cap, which keeps a long-running
// gateway's memory flat.
const riskEventCap = 200

// classLadder is one class's base and cap (SDD §4.5.1).
type classLadder struct {
	base time.Duration
	cap  time.Duration
}

// Options configures the pool. Zero fields take SDD defaults.
type Options struct {
	MaxInflight       int           // per-account semaphore (default 2, SDD §4.4)
	QueueWait         time.Duration // acquire wait bound (default 15s)
	Cooldown429       time.Duration // first cooldown on 429 (default 60s)
	Cooldown5xx       time.Duration // first cooldown on 5xx (default 30s)
	CooldownTransport time.Duration // first cooldown on transport failures (default 15s)
	CooldownCap       time.Duration // ladder cap for 429/5xx (default 10m)
	// CooldownTransportCap caps the transport ladder (default 5m). A transport
	// failure is less likely to be the account's fault, so it escalates less far.
	CooldownTransportCap time.Duration
	// RiskWindow is the window inside which risk-control signals are correlated
	// across accounts on one egress (default 5m, SDD §4.5.2).
	RiskWindow  time.Duration
	WarmPenalty float64 // selection penalty for accounts with inflight work
	Now         func() time.Time
	// SlotTTL bounds how long one request may hold an in-flight slot before the
	// reaper force-releases it (ADR 0019). 0 disables the reaper.
	SlotTTL time.Duration
	// Logf receives reaper messages; nil is silent.
	Logf func(format string, args ...any)
}

type account struct {
	id            string
	egress        string // egress label; DefaultEgress when unset (SDD §4.8)
	state         State
	cooldownUntil time.Time
	disabled      bool
	permanent     bool // dead passToken: never auto-recovers (SDD §4.5.2, TDD RISK-003)
	reason        string
	// failures counts consecutive failures per class; the counters never
	// escalate each other (REQ-2/REQ-6).
	failures map[FailureClass]int
	// cooldownClass/cooldownStatus/cooldownSince record why the account is
	// cooling, not just until when (REQ-5).
	cooldownClass  FailureClass
	cooldownStatus int
	cooldownSince  time.Time
	// lastError* record the most recent failure of any kind, whether or not it
	// entered a cooldown: "which error did this account last hit, and when" is
	// the first question asked when reading a pool (REQ-12).
	lastErrorClass  FailureClass
	lastErrorStatus int
	lastErrorAt     time.Time
	sem             chan struct{} // in-flight semaphore; len() is the inflight count
	ewmaLatency     float64       // seconds, EWMA alpha 0.2 (SDD §4.4)
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
	ladders     map[FailureClass]classLadder
	riskWindow  time.Duration
	riskEvents  []RiskEvent // bounded history, oldest first
	warmPenalty float64
	now         func() time.Time
	probe       ProbeFunc
	renew       RenewFunc
	notify      chan struct{} // release signal for Acquire waiters (buffered 1)
	// slotTTL/ live / expiredSlots implement the slot reaper (ADR 0019): a slot
	// is capacity held for one request, and a request that outlives its TTL must
	// not pin the account it leased.
	slotTTL      time.Duration
	logf         func(format string, args ...any)
	live         map[*Lease]struct{}
	expiredSlots int
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
	if opts.CooldownTransport <= 0 {
		opts.CooldownTransport = 15 * time.Second
	}
	if opts.CooldownCap <= 0 {
		opts.CooldownCap = 10 * time.Minute
	}
	if opts.CooldownTransportCap <= 0 {
		opts.CooldownTransportCap = 5 * time.Minute
	}
	if opts.RiskWindow <= 0 {
		opts.RiskWindow = 5 * time.Minute
	}
	if opts.WarmPenalty <= 0 {
		opts.WarmPenalty = 0.5
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Pool{
		maxInflight: opts.MaxInflight,
		slotTTL:     opts.SlotTTL,
		logf:        opts.Logf,
		live:        make(map[*Lease]struct{}),
		queueWait:   opts.QueueWait,
		riskWindow:  opts.RiskWindow,
		ladders: map[FailureClass]classLadder{
			ClassTransport:   {base: opts.CooldownTransport, cap: opts.CooldownTransportCap},
			ClassUpstream5xx: {base: opts.Cooldown5xx, cap: opts.CooldownCap},
			ClassThrottle429: {base: opts.Cooldown429, cap: opts.CooldownCap},
		},
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
		egress := a.Egress
		if egress == "" {
			egress = DefaultEgress
		}
		acc := &account{
			id:       a.ID,
			egress:   egress,
			sem:      make(chan struct{}, opts.MaxInflight),
			failures: map[FailureClass]int{},
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
//
// A lease is capacity for ONE request, never stickiness: it names the account
// the request is talking to (so every turn of that request keeps the same
// upstream conversation) and it occupies one of that account's in-flight slots
// (ADR 0019). It is not shared between requests and carries no conversation
// state.
type Lease struct {
	Pool *Pool
	ID   string
	// Egress is the label of the egress this account's traffic must leave
	// through (SDD §4.8). Every upstream call made for this lease uses it.
	Egress string

	issued time.Time     // when the slot was claimed (wall clock, for the reaper)
	ttl    time.Duration // this slot's TTL (0 = never reaped)

	mu       sync.Mutex
	onExpire func() // cancels the in-flight upstream call (set by the caller)
	released bool
}

// ArmExpiry registers the cancel func the reaper calls when this lease
// outlives its TTL (ADR 0019). Call it right before starting the upstream call
// it should abort; passing nil clears it.
func (l *Lease) ArmExpiry(cancel func()) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.onExpire = cancel
	l.mu.Unlock()
}

// Released reports whether the slot has already been given back. Safe to call
// on a nil lease.
func (l *Lease) Released() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

// Release frees the in-flight slot and wakes one Acquire waiter. It is
// idempotent: every path may release (early return, first-byte policy, the
// reaper, a deferred backstop) without double-counting capacity.
func (l *Lease) Release() {
	if l == nil || l.Pool == nil {
		return
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	l.onExpire = nil
	l.mu.Unlock()

	p := l.Pool
	p.mu.Lock()
	delete(p.live, l)
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

// ReapExpiredSlots force-releases every slot whose holder outlived its TTL and
// asks it to abort the upstream call it was holding (ADR 0019). It returns how
// many slots were reclaimed.
//
// This is the backstop, not the mechanism: the request path itself bounds each
// upstream turn with a context deadline. The reaper exists because the failure
// it prevents is silent — a wedged connection used to hold an account slot for
// as long as the HTTP client allowed (30 minutes), and with the default two
// slots per account that is the whole account.
func (p *Pool) ReapExpiredSlots() int {
	if p == nil || p.slotTTL <= 0 {
		return 0
	}
	now := time.Now()
	p.mu.Lock()
	var expired []*Lease
	for l := range p.live {
		if l.ttl > 0 && !l.issued.IsZero() && now.Sub(l.issued) > l.ttl {
			expired = append(expired, l)
		}
	}
	p.mu.Unlock()
	if len(expired) == 0 {
		return 0
	}
	for _, l := range expired {
		l.mu.Lock()
		cancel := l.onExpire
		l.mu.Unlock()
		if cancel != nil {
			cancel() // abort the upstream call this slot was holding
		}
		l.Release()
		if p.logf != nil {
			p.logf("pool: reaped slot account=%s after %s (TTL %s) — the request that held it was still running", l.ID, now.Sub(l.issued).Round(time.Second), l.ttl)
		}
	}
	p.mu.Lock()
	p.expiredSlots += len(expired)
	p.mu.Unlock()
	return len(expired)
}

// InFlight reports how many in-flight slots are currently held across all
// accounts (test and observability aid for the slot policies, ADR 0019).
func (p *Pool) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accounts {
		n += len(a.sem)
	}
	return n
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
			lease = p.registerLease(a)
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
			nearest = now.Add(p.ladders[ClassThrottle429].base)
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
	return p.registerLease(pick), nil
}

// registerLease creates a lease on an account and tracks it for the slot
// reaper. Callers hold p.mu.
func (p *Pool) registerLease(a *account) *Lease {
	l := &Lease{Pool: p, ID: a.id, Egress: a.egress, issued: time.Now(), ttl: p.slotTTL}
	if p.slotTTL > 0 {
		p.live[l] = struct{}{}
	}
	return l
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

// ReportFailure applies the ladder of that failure's own class and records the
// cause (SDD §4.5.1, POOLATTR-*). status is the upstream's status when there was
// one (0 for a transport failure). No class may escalate another: each keeps its
// own counter. The upstream's own Retry-After is never read — ladders are the
// single source of cooldown timing (owner decision, spec §10.2).
func (p *Pool) ReportFailure(accountID string, class FailureClass, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil || a.disabled {
		return
	}
	ladder, known := p.ladders[class]
	if !known {
		// Unknown classes cool like a 5xx: fail closed rather than inventing a
		// shorter penalty for a signal nobody classified.
		ladder = p.ladders[ClassUpstream5xx]
	}
	if a.failures == nil {
		a.failures = map[FailureClass]int{}
	}
	a.failures[class]++
	d := ladder.base
	for i := 1; i < a.failures[class]; i++ {
		d *= 2
		if d >= ladder.cap || d <= 0 {
			d = ladder.cap
			break
		}
	}
	a.state = StateCooldown
	a.cooldownUntil = p.now().Add(d)
	a.cooldownClass = class
	a.cooldownStatus = status
	a.cooldownSince = p.now()
	a.lastErrorClass = class
	a.lastErrorStatus = status
	a.lastErrorAt = a.cooldownSince
}

// ReportTransport records a transport-class failure (connection error, timeout,
// SSE idle-stall, EOF mid-body) on its own ladder (POOLATTR-001/003/004).
func (p *Pool) ReportTransport(accountID string) {
	p.ReportFailure(accountID, ClassTransport, 0)
}

// FailuresOf reports the per-class failure counters of one account. Every known
// class is present, zero included, so callers (and the /v1/admin/pool view) can
// render a stable shape.
func (p *Pool) FailuresOf(accountID string) map[FailureClass]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[FailureClass]int, len(FailureClasses()))
	for _, c := range FailureClasses() {
		out[c] = 0
	}
	if a := p.byID[accountID]; a != nil {
		for c, n := range a.failures {
			out[c] = n
		}
	}
	return out
}

// CooldownInfoOf reports why an account is cooling: the class, the upstream status
// that caused it (0 when there was none) and when it started. Empty when the
// account has never cooled.
func (p *Pool) CooldownInfoOf(accountID string) (FailureClass, int, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil {
		return "", 0, time.Time{}
	}
	return a.cooldownClass, a.cooldownStatus, a.cooldownSince
}

// ResetFailures clears an account's failure counters without touching its cooldown
// — used when a credential renewal succeeds (POOLATTR-006). Cooldowns are
// time-owned: releasing one here would hand an unhealthy account straight back,
// which is exactly the loop the reference implementation documents avoiding.
func (p *Pool) ResetFailures(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		a.failures = map[FailureClass]int{}
	}
}

// ReportSuccess resets the cooldown ladder (SDD §4.5, TDD POOL-004).
func (p *Pool) ReportSuccess(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		a.failures = map[FailureClass]int{}
	}
}

// Report429 applies the rate-limit cooldown ladder: base 60s doubling to the
// 10m cap (SDD §4.5, TDD POOL-002/003).
func (p *Pool) Report429(accountID string) {
	p.ReportFailure(accountID, ClassThrottle429, 429)
}

// Report5xx applies the upstream-sick ladder: base 30s doubling to the cap
// (SDD §4.5, TDD POOL-005). Transport failures have their own ladder now —
// call ReportTransport for those (SDD §4.5.1).
func (p *Pool) Report5xx(accountID string) {
	p.ReportFailure(accountID, ClassUpstream5xx, 0)
}

// ReportRiskControl records a risk-control signal (451/461) and disables the
// account — fail closed, because we cannot yet tell an account-scoped block from
// an egress- or model-scoped one (SDD §4.5.2, spec §5).
//
// It deliberately does NOT set the permanent flag: permanence is a claim about the
// upstream's intent, and the form of 451/461 is unverified here. The account is
// therefore recoverable through Enable (RISK-002/006).
//
// It returns how many distinct accounts on the same egress (this one included)
// have recorded a risk-control signal inside the correlation window — the number
// that eventually tells the three shapes apart: a signal that travels with the
// account, one that follows the egress, or a transient challenge. Callers must not
// deliberately induce a signal to produce this number (REQ-9).
func (p *Pool) ReportRiskControl(accountID, reason string, ev RiskEvent) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil {
		// An unknown account is still worth recording: the evidence is about the
		// upstream's behaviour, not about our bookkeeping.
		p.recordRiskLocked(RiskEvent{
			AccountID: accountID, Egress: DefaultEgress,
			Reason: reason, Status: ev.Status, Model: ev.Model,
			Channel: ev.Channel, BodyDigest: ev.BodyDigest, At: p.now(),
		})
		return p.sameEgressCountLocked(DefaultEgress, p.now())
	}
	a.disabled = true
	a.permanent = false
	a.reason = reason
	a.state = StateDisabled
	a.cooldownUntil = time.Time{}
	a.lastErrorClass = ClassRiskControl
	a.lastErrorStatus = ev.Status
	a.lastErrorAt = p.now()
	p.recordRiskLocked(RiskEvent{
		AccountID:  a.id,
		Egress:     a.egress,
		Model:      ev.Model,
		Channel:    ev.Channel,
		Status:     ev.Status,
		BodyDigest: ev.BodyDigest,
		Reason:     reason,
		At:         p.now(),
	})
	return p.sameEgressCountLocked(a.egress, p.now())
}

func (p *Pool) recordRiskLocked(ev RiskEvent) {
	p.riskEvents = append(p.riskEvents, ev)
	if len(p.riskEvents) > riskEventCap {
		p.riskEvents = p.riskEvents[len(p.riskEvents)-riskEventCap:]
	}
}

// sameEgressCountLocked counts distinct accounts behind one egress with a signal
// inside the window ending at now.
func (p *Pool) sameEgressCountLocked(egress string, now time.Time) int {
	seen := map[string]bool{}
	for _, ev := range p.riskEvents {
		if ev.Egress != egress {
			continue
		}
		if now.Sub(ev.At) > p.riskWindow {
			continue
		}
		seen[ev.AccountID] = true
	}
	return len(seen)
}

// RiskEvents returns a copy of the recorded risk-control history, oldest first.
func (p *Pool) RiskEvents() []RiskEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]RiskEvent, len(p.riskEvents))
	copy(out, p.riskEvents)
	return out
}

// AccountSnapshot is one account's runtime view for the operator endpoint
// (SDD §4.9). It carries no credential material of any kind: an account id, its
// egress LABEL, and counters/state. Masking and credential age are the API
// layer's business (it owns the store).
type AccountSnapshot struct {
	ID               string         `json:"id"`
	Egress           string         `json:"egress"`
	State            State          `json:"state"`
	Permanent        bool           `json:"permanent"`
	Reason           string         `json:"reason,omitempty"`
	InFlight         int            `json:"in_flight"`
	MaxInFlight      int            `json:"max_in_flight"`
	EWMALatencyMS    int64          `json:"ewma_latency_ms"`
	CooldownUntil    time.Time      `json:"cooldown_until,omitempty"`
	CooldownClass    FailureClass   `json:"cooldown_class,omitempty"`
	CooldownSince    time.Time      `json:"cooldown_since,omitempty"`
	CooldownCausedBy int            `json:"cooldown_status,omitempty"`
	Failures         map[string]int `json:"failures"`
	LastErrorClass   FailureClass   `json:"last_error_class,omitempty"`
	LastErrorStatus  int            `json:"last_error_status,omitempty"`
	LastErrorAt      time.Time      `json:"last_error_at,omitempty"`
}

// Snapshot returns every account's runtime view, ordered as the pool holds them
// (SDD §4.9). It is a copy: callers cannot reach pool internals through it.
func (p *Pool) Snapshot() []AccountSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountSnapshot, 0, len(p.accounts))
	for _, a := range p.accounts {
		failures := make(map[string]int, len(a.failures))
		for class, n := range a.failures {
			if n > 0 {
				failures[string(class)] = n
			}
		}
		out = append(out, AccountSnapshot{
			ID:               a.id,
			Egress:           a.egress,
			State:            a.state,
			Permanent:        a.permanent,
			Reason:           a.reason,
			InFlight:         len(a.sem),
			MaxInFlight:      cap(a.sem),
			EWMALatencyMS:    int64(a.ewmaLatency * 1000),
			CooldownUntil:    a.cooldownUntil,
			CooldownClass:    a.cooldownClass,
			CooldownSince:    a.cooldownSince,
			CooldownCausedBy: a.cooldownStatus,
			Failures:         failures,
			LastErrorClass:   a.lastErrorClass,
			LastErrorStatus:  a.lastErrorStatus,
			LastErrorAt:      a.lastErrorAt,
		})
	}
	return out
}

// PermanentOf reports whether an account's disable is permanent (a dead
// passToken). Risk-control disables are never permanent (RISK-002).
func (p *Pool) PermanentOf(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		return a.permanent
	}
	return false
}

// EgressOf reports an account's egress label (DefaultEgress when unset).
func (p *Pool) EgressOf(accountID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		return a.egress
	}
	return ""
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
	a.failures = map[FailureClass]int{}
	a.cooldownClass, a.cooldownStatus, a.cooldownSince = "", 0, time.Time{}
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
			a.failures = map[FailureClass]int{}
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
				a.failures = map[FailureClass]int{}
				recovered++
			} else {
				a.state = StateDisabled
				a.disabled = true
				a.reason = "session renewal failed: " + rerr.Error()
			}
		default:
			// Probe sick: keep cooling at the 5xx base (ladder continues on the
			// next failure of that class).
			a.cooldownUntil = p.now().Add(p.ladders[ClassUpstream5xx].base)
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
