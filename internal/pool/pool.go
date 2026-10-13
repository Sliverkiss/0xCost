// Package pool implements the account pool: states (available / disabled)
// and score-based selection with round-robin fairness (SDD §4).
//
// There is deliberately no cooldown, backoff or circuit breaking: an upstream
// 429/5xx/transport failure never changes an account's availability (owner
// ruling, 2026-10 — "we don't do cooldown/backoff, failures pass through").
// Our own cooldown was observed to be stricter than the upstream's real rate
// limits (one 429 parked the whole pool for 60s while the upstream had long
// recovered), which is harm, not protection. The only terminal state besides
// available is disabled, set by risk control (451/461), a dead credential, or
// an operator.
//
// There is also no concurrency cap (owner ruling, 2026-10-14, "并发槽砍了"):
// an account with capacity free locally may well be the account the upstream
// is throttling, and an account at our synthetic limit may be perfectly
// healthy. The upstream decides what it will serve; a local slot count was
// just another self-imposed limit that invented 429s the upstream never sent.
package pool

import (
	"context"
	"errors"
	"sort"
	"sync"
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
	// RiskWindow is the window inside which risk-control signals are correlated
	// across accounts on one egress (default 5m, SDD §4.5.2).
	RiskWindow time.Duration
	Now        func() time.Time
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
	// lastErrorStatus/lastErrorAt record the most recent upstream failure of
	// any kind: "which error did this account last hit, and when" is the first
	// question asked when reading a pool. Diagnostics only — they never change
	// availability.
	lastErrorStatus int
	lastErrorAt     time.Time
	ewmaLatency     float64 // seconds, EWMA alpha 0.2 (SDD §4.4)
}

// Pool is the account pool. One mutex guards selection and state; there is no
// capacity to hand back on release, so the lock is held only for microseconds
// of scoring (SDD §3).
type Pool struct {
	mu         sync.Mutex
	accounts   []*account // stable order (selection tie-break)
	byID       map[string]*account
	cursor     int // round-robin rotation within the selection band
	riskWindow time.Duration
	riskEvents []RiskEvent // bounded history, oldest first
	now        func() time.Time
}

// New builds a pool from account records. Only enabled entries become pool
// members; persisted runtime state (state / disabled_reason) is honored so
// bans survive restart (SDD §7.5, TDD POOL-013).
func New(accts []accounts.Account, opts Options) *Pool {
	if opts.RiskWindow <= 0 {
		opts.RiskWindow = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Pool{
		riskWindow: opts.RiskWindow,
		now:        opts.Now,
		byID:       make(map[string]*account),
	}
	for _, a := range accts {
		if !a.Enabled {
			continue
		}
		egress := a.Egress
		if egress == "" {
			egress = DefaultEgress
		}
		acc := &account{id: a.ID, egress: egress}
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

// Lease names the account one request is talking to.
//
// A lease is account identity, never capacity or stickiness: it exists so
// every turn of one request keeps the same upstream conversation (ADR 0017).
// It is not shared between requests and carries no conversation state. With
// in-flight slots gone (owner ruling 2026-10-14) Release is a bookkeeping
// no-op, kept so the request paths' deferred cleanup stays symmetric and
// idempotent.
type Lease struct {
	Pool *Pool
	ID   string
	// Egress is the label of the egress this account's traffic must leave
	// through (SDD §4.8). Every upstream call made for this lease uses it.
	Egress string

	mu       sync.Mutex
	released bool
}

// Release marks the lease done. Idempotent; every path may release (early
// return, deferred backstop) without double-counting.
func (l *Lease) Release() {
	if l == nil || l.Pool == nil {
		return
	}
	l.mu.Lock()
	l.released = true
	l.mu.Unlock()
}

// Released reports whether the lease has already been released. Safe to call
// on a nil lease.
func (l *Lease) Released() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

// Acquire selects an available account. Selection is score-based with a
// round-robin rotation inside the best band (SDD §4.4): the score is the
// EWMA first-byte latency, and accounts within best*1.15+1 share the load
// evenly (TDD POOL-001). excludeIDs (retry-on-another-account, SDD §7.2) are
// skipped entirely. There is no waiting and no capacity limit (owner ruling
// 2026-10-14): Acquire returns immediately or fails with ErrNoAccounts (empty
// pool / all disabled / all excluded), mapped by the API layer (SDD §7.4).
func (p *Pool) Acquire(ctx context.Context, excludeIDs ...string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	excl := make(map[string]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		excl[id] = true
	}

	type scored struct {
		a     *account
		score float64
	}
	var candidates []scored
	enabled := 0
	for _, a := range p.accounts {
		if excl[a.id] || a.disabled {
			continue
		}
		enabled++
		if a.state != StateAvailable {
			continue
		}
		candidates = append(candidates, scored{a, a.ewmaLatency})
	}
	if enabled == 0 || len(candidates) == 0 {
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

	return &Lease{Pool: p, ID: pick.id, Egress: pick.egress}, nil
}

// AcquireSticky acquires a specific account (sticky-session path, SDD
// §4.3). When the target is not selectable it falls back to a general
// Acquire; the bool reports whether the sticky target was won.
func (p *Pool) AcquireSticky(ctx context.Context, accountID string) (*Lease, bool, error) {
	p.mu.Lock()
	a := p.byID[accountID]
	if a != nil && !a.disabled && a.state == StateAvailable {
		p.mu.Unlock()
		return &Lease{Pool: p, ID: a.id, Egress: a.egress}, true, nil
	}
	p.mu.Unlock()
	l, err := p.Acquire(ctx)
	return l, false, err
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
