// Package pool implements the account pool: states (available / disabled),
// score-based selection with round-robin fairness, and per-account in-flight
// semaphores (SDD §4).
//
// There is deliberately no cooldown, backoff or circuit breaking: an upstream
// 429/5xx/transport failure never changes an account's availability (owner
// ruling, 2026-10 — "we don't do cooldown/backoff, failures pass through").
// Our own cooldown was observed to be stricter than the upstream's real rate
// limits (one 429 parked the whole pool for 60s while the upstream had long
// recovered), which is harm, not protection. The only terminal state besides
// available is disabled, set by risk control (451/461), a dead credential, or
// an operator.
package pool

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
)

// State is an account's health state: available or disabled.
type State string

const (
	StateAvailable State = "available"
	StateDisabled  State = "disabled"
)

// ErrNoAccounts: pool empty or all disabled → 503 no_accounts (SDD §7.4).
var ErrNoAccounts = errors.New("no accounts available: pool is empty or all accounts are disabled")

// ErrBusy: all in-flight slots busy beyond the queue wait → 429 pool_busy
// (SDD §7.4). A capacity signal, not backoff.
type ErrBusy struct {
	RetryAfter time.Duration
}

// RetryAfterSeconds clamps to at least 1s for the Retry-After header.
func (e *ErrBusy) RetryAfterSeconds() int { return clampSeconds(e.RetryAfter) }

func (e *ErrBusy) Error() string {
	return "all account slots busy; retry after " + e.RetryAfter.Round(time.Second).String()
}

func clampSeconds(d time.Duration) int {
	s := int(d / time.Second)
	if s < 1 {
		s = 1
	}
	return s
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

// Options configures the pool. Zero fields take SDD defaults.
type Options struct {
	MaxInflight int           // per-account semaphore (default 2, SDD §4.4)
	QueueWait   time.Duration // acquire wait bound (default 15s)
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
	id     string
	egress string // egress label; DefaultEgress when unset (SDD §4.8)
	state  State
	// disabled/permanent/reason are the disable bookkeeping (SDD §4.6): set by
	// risk control, a dead credential, or an operator — never by a 429/5xx.
	disabled  bool
	permanent bool // dead passToken: never auto-recovers (SDD §4.5.2, TDD RISK-003)
	reason    string
	sem       chan struct{} // in-flight semaphore; len() is the inflight count
	// lastErrorStatus/lastErrorAt record the most recent upstream failure of
	// any kind: "which error did this account last hit, and when" is the first
	// question asked when reading a pool. Diagnostics only — they never change
	// availability.
	lastErrorStatus int
	lastErrorAt     time.Time
	ewmaLatency     float64 // seconds, EWMA alpha 0.2 (SDD §4.4)
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
	riskWindow  time.Duration
	riskEvents  []RiskEvent // bounded history, oldest first
	warmPenalty float64
	now         func() time.Time
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
// members; persisted runtime state (state / disabled_reason) is honored so
// bans survive restart (SDD §7.5, TDD POOL-013).
func New(accts []accounts.Account, opts Options) *Pool {
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = 2
	}
	if opts.QueueWait <= 0 {
		opts.QueueWait = 15 * time.Second
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
		warmPenalty: opts.WarmPenalty,
		now:         opts.Now,
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
			id:     a.ID,
			egress: egress,
			sem:    make(chan struct{}, opts.MaxInflight),
		}
		switch State(a.State) {
		case StateDisabled:
			acc.state = StateDisabled
			acc.disabled = true
			acc.reason = a.DisabledReason
		default:
			acc.state = StateAvailable
		}
		p.accounts = append(p.accounts, acc)
		p.byID[a.ID] = acc
	}
	return p
}

// errBusy is the internal "candidates exist but all slots are full" signal —
// waitable, unlike no-accounts which is terminal for the call.
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

	issued time.Time     // when the slot was claimed (for reporting)
	ttl    time.Duration // this slot's silence window (0 = never reaped)

	// lastActivity is the wall clock of the last upstream byte on this lease.
	// The expiry moves with it (ADR 0036): a slot is reaped for **silence**, not
	// for taking long, so a search that keeps fetching pages may hold its slot
	// far past the window while a wedged turn is reclaimed on time.
	lastActivity atomic.Int64

	mu       sync.Mutex
	onExpire func() // cancels the in-flight upstream call (set by the caller)
	released bool
}

// Touch records upstream activity on this lease, moving its expiry. The chat
// pipeline calls it from the SSE reader, i.e. on every byte from the upstream
// (ADR 0036) — not per parsed piece, so a frame this gateway does not turn into
// a delta (a doc or usage event, a page the search is sampling) still counts as
// the upstream being alive. Safe on a nil lease.
func (l *Lease) Touch() {
	if l == nil {
		return
	}
	l.lastActivity.Store(time.Now().UnixNano())
}

// IssuedAt is when the slot was claimed — used for reporting how long a request
// has held capacity, which is a different question from how long the upstream
// has been quiet.
func (l *Lease) IssuedAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	return l.issued
}

// SilentFor reports how long this lease has been without upstream activity, and
// whether it has ever had any (a fresh lease counts from its claim).
func (l *Lease) SilentFor(now time.Time) time.Duration {
	if l == nil {
		return 0
	}
	last := l.lastActivity.Load()
	if last == 0 {
		return now.Sub(l.issued)
	}
	return now.Sub(time.Unix(0, last))
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
		// Silence, not age: the window moves with every upstream byte
		// (Lease.Touch, ADR 0036).
		if l.ttl > 0 && l.SilentFor(now) > l.ttl {
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
			p.logf("pool: reaped slot account=%s after %s of silence (window %s, held %s) — the request that held it was still running",
				l.ID, l.SilentFor(now).Round(time.Second), l.ttl, now.Sub(l.issued).Round(time.Second))
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
// fails closed with ErrBusy. Terminal failure: ErrNoAccounts (empty/all
// disabled), mapped by the API layer (SDD §7.4).
func (p *Pool) Acquire(ctx context.Context, excludeIDs ...string) (*Lease, error) {
	// Queue waiting is wall-clock bounded (real time).
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
			return nil, err // no-accounts: terminal
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
	if a != nil && !a.disabled && len(a.sem) < cap(a.sem) {
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
	excl := make(map[string]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excl[id] = true
	}

	type scored struct {
		a     *account
		score float64
	}
	var candidates []scored
	availableExists := false
	enabled := 0
	for _, a := range p.accounts {
		if excl[a.id] || a.disabled {
			continue
		}
		enabled++
		if a.state != StateAvailable {
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
		return nil, ErrNoAccounts
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
	l.Touch() // a fresh lease counts as active from its claim
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

// ReportSuccess records a successful turn. With the cooldown machinery gone
// there is no failure state to clear; the call is kept as the pipeline's
// "this request ended well" hook so callers need not branch on it.
func (p *Pool) ReportSuccess(accountID string) {}

// Report429 records the upstream's 429 as diagnostics only (owner ruling
// 2026-10): the account stays available, the signal passes through to the
// client, and the last-error fields exist so /v1/admin/pool can still show
// what an account last answered.
func (p *Pool) Report429(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.byID[accountID]; a != nil {
		a.lastErrorStatus = 429
		a.lastErrorAt = p.now()
	}
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
	ID            string `json:"id"`
	Egress        string `json:"egress"`
	State         State  `json:"state"`
	Permanent     bool   `json:"permanent"`
	Reason        string `json:"reason,omitempty"`
	InFlight      int    `json:"in_flight"`
	MaxInFlight   int    `json:"max_in_flight"`
	EWMALatencyMS int64  `json:"ewma_latency_ms"`
	// LastErrorStatus/LastErrorAt record the most recent upstream failure seen
	// on this account, purely as diagnostics — they never affect availability.
	LastErrorStatus int       `json:"last_error_status,omitempty"`
	LastErrorAt     time.Time `json:"last_error_at,omitempty"`
}

// Snapshot returns every account's runtime view, ordered as the pool holds them
// (SDD §4.9). It is a copy: callers cannot reach pool internals through it.
func (p *Pool) Snapshot() []AccountSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountSnapshot, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, AccountSnapshot{
			ID:              a.id,
			Egress:          a.egress,
			State:           a.state,
			Permanent:       a.permanent,
			Reason:          a.reason,
			InFlight:        len(a.sem),
			MaxInFlight:     cap(a.sem),
			EWMALatencyMS:   int64(a.ewmaLatency * 1000),
			LastErrorStatus: a.lastErrorStatus,
			LastErrorAt:     a.lastErrorAt,
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
	case p.notify <- struct{}{}: // waiters must re-evaluate (the pool may now be empty)
	default:
	}
}

// Enable clears disabled state for an account and makes it
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
	a.state = StateAvailable
	select {
	case p.notify <- struct{}{}: // waiters re-evaluate
	default:
	}
}

// RuntimeState returns the persisted-form state for the accounts file
// (SDD §7.5): state and disabled_reason.
func (p *Pool) RuntimeState(accountID string) (State, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.byID[accountID]
	if a == nil {
		return StateDisabled, ""
	}
	return a.state, a.reason
}

// Summary is the /healthz pool summary: state counts only, no credentials
// (SDD §5.1).
type Summary struct {
	Total     int `json:"total"`
	Available int `json:"available"`
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
