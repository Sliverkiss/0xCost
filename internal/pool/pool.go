// Package pool manages the anonymous guest identity pool. Identities are
// disposable cartridges: a virgin identity is acquired, serves exactly one
// request attempt and is discarded. The pool keeps a FIFO stock of virgins,
// leases them with one in-flight stream per identity, discards them on
// completion, and eagerly restocks toward warm_size.
//
// Capacity is modelled as stock plus a bounded FIFO queue of waiters, never as
// a list with a fallback: when the stock is empty a request waits for the paced
// restock up to Config.AcquireWait and then receives ErrNoIdentity. There is no
// row type that could represent a partially used identity, so none can be
// selected. See docs/DESIGN-POOL-V3.md.
package pool

import (
	"container/list"
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chatglm2api/internal/logfmt"
	"chatglm2api/internal/signer"
	"chatglm2api/internal/upstream"
)

// ErrNoIdentity is returned when no virgin identity became available within
// the acquire wait, or immediately when replenishment is known to be failing.
var ErrNoIdentity = errors.New("no identity available")

// GuestBudgetCap is the measured lifetime message budget of one guest identity
// (docs/reverse/ANON-SESSION.md). It is not enforced locally: an identity is
// discarded after its first request. It is used only to report abandoned
// messages and to describe rows written by older pool versions.
const GuestBudgetCap = 10

// Acquirer creates fresh guest identities (upstream.Client satisfies it).
type Acquirer interface {
	AcquireGuest(ctx context.Context, deviceID string, ua upstream.UserAgent, xff string) (*upstream.IdentityTokens, error)
}

// Config is the pool configuration derived from internal/config.
type Config struct {
	Acquirer Acquirer
	// WarmSize is the virgin stock target kept by prewarm and restock.
	WarmSize int
	MaxSize  int
	// AcquireQPS / AcquireInterval / AcquireJitter shape acquisition traffic.
	AcquireQPS         float64
	AcquireInterval    time.Duration
	AcquireJitter      time.Duration
	AcquireConcurrency int
	// AcquireWait bounds how long Select waits for the paced restock before
	// returning ErrNoIdentity. It is the only client-facing pool timeout.
	AcquireWait time.Duration
	// BreakerThreshold failures inside BreakerWindow open the pool breaker.
	BreakerThreshold int
	BreakerCooldown  time.Duration
	BreakerWindow    time.Duration
	StateFile        string
	PersistInterval  time.Duration
	// XFF returns the egress IP for an acquisition. Nil = none.
	XFF func() string
}

func (c Config) withDefaults() Config {
	if c.WarmSize < 0 {
		c.WarmSize = 0
	}
	if c.MaxSize <= 0 {
		c.MaxSize = 512
	}
	if c.MaxSize < c.WarmSize {
		c.MaxSize = c.WarmSize
	}
	if c.AcquireConcurrency <= 0 {
		c.AcquireConcurrency = 4
	}
	if c.AcquireQPS <= 0 {
		c.AcquireQPS = 2
	}
	if c.AcquireInterval <= 0 {
		c.AcquireInterval = time.Second / time.Duration(c.AcquireQPS)
	}
	if c.AcquireWait <= 0 {
		c.AcquireWait = 10 * time.Second
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 20
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = 30 * time.Second
	}
	if c.BreakerWindow <= 0 {
		c.BreakerWindow = time.Minute
	}
	if c.PersistInterval <= 0 {
		c.PersistInterval = 5 * time.Second
	}
	return c
}

// Identity is one anonymous guest identity: a disposable cartridge. It has no
// usage or retirement fields — a cartridge that has served a message is
// removed from the pool rather than marked, so a partially used identity
// cannot be represented and therefore cannot be selected.
type Identity struct {
	UID         string    `json:"uid"`
	AccessToken string    `json:"access_token"`
	DeviceID    string    `json:"device_id"`
	UAIndex     int       `json:"ua_index"`
	AccessExp   time.Time `json:"access_exp"`
	CreatedAt   time.Time `json:"created_at"`

	// InFlight is runtime-only: an identity is leased to at most one request.
	// Leased rows are never persisted (a crash may have spent their message).
	InFlight bool `json:"-"`
}

// UserAgent returns the pinned fingerprint.
func (id *Identity) UserAgent() upstream.UserAgent { return upstream.UserAgentFor(id.UAIndex) }

// usableLocked reports whether id can be stocked: idle and unexpired.
func (id *Identity) usableLocked(now time.Time) bool {
	if id.InFlight {
		return false
	}
	if !id.AccessExp.IsZero() && !now.Before(id.AccessExp) {
		return false
	}
	return true
}

// waiter is one request blocked in Select, in arrival order.
type waiter struct {
	ready chan *Lease
	elem  *list.Element
	done  bool
}

// Pool is the disposable identity pool.
type Pool struct {
	cfg Config

	mu sync.Mutex
	// byUID holds every live row (stock or leased). A row that has served is
	// deleted, never kept.
	byUID map[string]*Identity
	// stock is the FIFO queue of idle virgin UID.
	stock []string
	// waiters is the FIFO of requests blocked for a cartridge.
	waiters *list.List
	// restocking is true while an eager restock goroutine is running.
	restocking     bool
	acquireBackoff time.Duration
	closed         bool
	// closeCh is closed by Close. Background loops and waiters select on it.
	closeCh chan struct{}

	acquireSem chan struct{}
	gate       *pacingGate

	// counters
	acquiredTotal     atomic.Int64
	servedTotal       atomic.Int64
	discardTotal      atomic.Int64
	restockFailures   atomic.Int64
	messagesAbandoned atomic.Int64
	busyRetries       atomic.Int64
	acquireFails      atomic.Int64
	waitTimeouts      atomic.Int64
	waiterTotal       atomic.Int64

	// Circuit breaker: upstream failures across the pool inside BreakerWindow,
	// plus the timestamp of the most recent failure (to evict the window).
	breakerFails int
	breakerLast  time.Time
	breakerUntil time.Time

	dirty  atomic.Bool
	stopCh chan struct{}
	// ctx is cancelled by Close so an in-flight restock acquisition stops
	// promptly instead of holding shutdown for its timeout.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New builds a pool and restores persisted virgin stock when StateFile is set.
func New(cfg Config) *Pool {
	cfg = cfg.withDefaults()
	poolCtx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		cfg:        cfg,
		byUID:      map[string]*Identity{},
		waiters:    list.New(),
		acquireSem: make(chan struct{}, cfg.AcquireConcurrency),
		gate:       newPacingGate(cfg.AcquireInterval, cfg.AcquireJitter),
		stopCh:     make(chan struct{}),
		closeCh:    make(chan struct{}),
		ctx:        poolCtx,
		cancel:     cancel,
	}
	if cfg.StateFile != "" {
		p.load()
		p.wg.Add(1)
		go p.flushLoop()
	}
	return p
}

// Close stops background work (restock, flusher) and writes the final state.
// Waiters are woken with ErrNoIdentity instead of waiting out their deadline.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		// Publishing closeCh under the lock pairs with startRestock's check:
		// either the goroutine sees closed, or Close sees restocking and this
		// close is observed by a loop that already started.
		close(p.closeCh)
		p.mu.Unlock()
		close(p.stopCh)
		p.cancel()
	})
	p.wg.Wait()
	p.Flush()
}

// Flush persists the pool (idle virgins only) atomically.
func (p *Pool) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty.Store(false)
	p.saveLocked()
}

func (p *Pool) flushLoop() {
	t := time.NewTicker(p.cfg.PersistInterval)
	defer p.wg.Done()
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			if p.dirty.Swap(false) {
				p.Flush()
			}
		}
	}
}

// Counts describes pool occupancy.
type Counts struct {
	// Stock is the number of virgin identities that can serve right now.
	Stock    int
	Total    int
	InFlight int
	// Waiters is the number of requests blocked in Select.
	Waiters int
}

// Counts returns current occupancy.
func (p *Pool) Counts() Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.countsLocked(time.Now())
}

func (p *Pool) countsLocked(now time.Time) Counts {
	var c Counts
	c.Total = len(p.byUID)
	for _, id := range p.byUID {
		if id.InFlight {
			c.InFlight++
		}
	}
	c.Stock = p.stockLocked(now)
	c.Waiters = p.waiters.Len()
	return c
}

// Servable reports whether at least one identity can start a request now.
func (p *Pool) Servable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.breakerOpenLocked(time.Now()) {
		return false
	}
	return p.stockLocked(time.Now()) > 0
}

// stockLocked prunes dead queue entries and returns the number of servable
// virgins.
func (p *Pool) stockLocked(now time.Time) int {
	n := 0
	for i := 0; i < len(p.stock); {
		id, ok := p.byUID[p.stock[i]]
		if !ok || !id.usableLocked(now) {
			p.stock = append(p.stock[:i], p.stock[i+1:]...)
			continue
		}
		n++
		i++
	}
	return n
}

// queueLocked appends a UID to the stock queue (acquire order).
func (p *Pool) queueLocked(uid string) {
	for _, q := range p.stock {
		if q == uid {
			return
		}
	}
	p.stock = append(p.stock, uid)
}

// requeueLocked puts an untouched virgin back at the head of the queue.
func (p *Pool) requeueLocked(uid string) {
	for i, q := range p.stock {
		if q == uid {
			p.stock = append(p.stock[:i], p.stock[i+1:]...)
			break
		}
	}
	p.stock = append([]string{uid}, p.stock...)
}

// stockOrHandOffLocked makes an idle virgin available: the oldest waiter is
// served directly (no broadcast), otherwise the identity joins the stock.
// frontOfStock puts a returned lease at the head instead of the tail.
func (p *Pool) stockOrHandOffLocked(id *Identity, frontOfStock bool) {
	for {
		front := p.waiters.Front()
		if front == nil {
			break
		}
		w := front.Value.(*waiter)
		p.waiters.Remove(front)
		if w.done {
			continue
		}
		w.done = true
		w.ready <- p.leaseLocked(id)
		return
	}
	id.InFlight = false
	if frontOfStock {
		p.requeueLocked(id.UID)
	} else {
		p.queueLocked(id.UID)
	}
	p.dirty.Store(true)
}

// IsServed reports whether the attempt holding this lease has marked itself as
// having sent a message upstream. It exists so the server can classify an
// acquire-style failure without guessing.
func (l *Lease) IsServed() bool { return l != nil && l.served.Load() }

// Lease is a held identity. It must end in exactly one of Release or Discard.
type Lease struct {
	Identity *Identity
	pool     *Pool
	ended    atomic.Bool
	served   atomic.Bool
}

// MarkServed records that this attempt is about to send the upstream message.
// After this call Release refuses the cartridge: it can no longer be proven
// virgin. It is called immediately before the chat POST.
func (l *Lease) MarkServed() {
	if l == nil {
		return
	}
	l.served.Store(true)
}

// Release returns a provably untouched identity to the stock. It is legal only
// for a read-only lease that never entered the message path (the /v1/models
// catalogue fetch); a 10061 busy refusal keeps its lease instead of releasing
// it. Release panics on a cartridge that was marked served.
func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	if l.served.Load() {
		panic("pool: Release on a cartridge that served a message; use Discard")
	}
	if l.ended.Swap(true) {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.byUID[l.Identity.UID]
	if !ok {
		return
	}
	// A request may be waiting: the oldest waiter is handed the cartridge
	// directly; otherwise it returns to the head of the stock.
	id.InFlight = false
	p.stock = removeUID(p.stock, id.UID)
	p.dirty.Store(true)
	p.stockOrHandOffLocked(id, true)
}

// Discard ends the cartridge: the row is removed from the pool and from the
// state file, abandoned messages are counted, and a restock is requested.
func (l *Lease) Discard(reason string) {
	if l == nil || l.pool == nil {
		return
	}
	if l.ended.Swap(true) {
		return
	}
	l.pool.discardUID(l.Identity.UID, l.served.Load(), reason)
}

// TrySelect leases a virgin identity if one is immediately available, without
// waiting and without acquiring. It is for read-only uses such as the model
// catalogue fetch, which must not consume stock or block.
func (p *Pool) TrySelect() (*Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.breakerOpenLocked(now) {
		return nil, false
	}
	id := p.popStockLocked(now)
	if id == nil {
		return nil, false
	}
	return p.leaseLocked(id), true
}

// leaseLocked marks id in flight and builds the lease snapshot.
func (p *Pool) leaseLocked(id *Identity) *Lease {
	id.InFlight = true
	p.dirty.Store(true)
	cp := *id
	return &Lease{Identity: &cp, pool: p}
}

// popStockLocked removes and returns the first servable virgin, or nil.
func (p *Pool) popStockLocked(now time.Time) *Identity {
	for i := 0; i < len(p.stock); {
		uid := p.stock[i]
		id, ok := p.byUID[uid]
		if !ok || !id.usableLocked(now) {
			p.stock = append(p.stock[:i], p.stock[i+1:]...)
			continue
		}
		p.stock = append(p.stock[:i], p.stock[i+1:]...)
		return id
	}
	return nil
}

// Select leases a virgin identity. When the stock is empty it registers as a
// FIFO waiter and waits up to Config.AcquireWait for the paced restock. It
// fails immediately when the breaker is open or acquisition is backing off,
// because waiting cannot help then.
func (p *Pool) Select(ctx context.Context) (*Lease, error) {
	if lease, ok := p.TrySelect(); ok {
		return lease, nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrNoIdentity
	}
	now := time.Now()
	// A tripped breaker refuses service even from stock: the pool just proved
	// the upstream unhealthy and a stocked cartridge only carries that failure.
	if p.breakerOpenLocked(now) {
		p.mu.Unlock()
		return nil, ErrNoIdentity
	}
	// Re-check the stock under the lock: a cartridge may have been acquired
	// between the optimistic TrySelect above and this point.
	if id := p.popStockLocked(now); id != nil {
		lease := p.leaseLocked(id)
		p.mu.Unlock()
		return lease, nil
	}
	if p.acquireBackoff > 0 {
		p.mu.Unlock()
		return nil, ErrNoIdentity
	}
	w := &waiter{ready: make(chan *Lease, 1)}
	w.elem = p.waiters.PushBack(w)
	p.waiterTotal.Add(1)
	p.mu.Unlock()

	// The restock self-checks the need (warm target or this waiter).
	p.startRestock()

	timer := time.NewTimer(p.cfg.AcquireWait)
	defer timer.Stop()
	select {
	case lease := <-w.ready:
		return lease, nil
	case <-ctx.Done():
		p.abandonWaiter(w)
		return nil, ctx.Err()
	case <-p.closeCh:
		p.abandonWaiter(w)
		return nil, ErrNoIdentity
	case <-timer.C:
		p.abandonWaiter(w)
		p.waitTimeouts.Add(1)
		return nil, ErrNoIdentity
	}
}

// abandonWaiter removes w from the queue. If a cartridge was handed to it in
// the race, the cartridge is returned to the stock instead of leaking.
func (p *Pool) abandonWaiter(w *waiter) {
	p.mu.Lock()
	if !w.done {
		w.done = true
		p.waiters.Remove(w.elem)
	}
	p.mu.Unlock()
	select {
	case lease := <-w.ready:
		// Handoff won the race: the caller no longer wants it. Release is
		// legal because nothing was sent upstream.
		lease.Release()
	default:
	}
}

// NoteDiscard discards an identity by UID (operator paths that hold only the id).
func (p *Pool) NoteDiscard(uid, reason string) { p.discardUID(uid, false, reason) }

func (p *Pool) discardUID(uid string, served bool, reason string) {
	p.mu.Lock()
	if _, ok := p.byUID[uid]; !ok {
		p.mu.Unlock()
		return
	}
	abandoned := GuestBudgetCap
	if served {
		abandoned--
	}
	delete(p.byUID, uid)
	p.stock = removeUID(p.stock, uid)
	p.discardTotal.Add(1)
	p.messagesAbandoned.Add(int64(abandoned))
	if reason != "" {
		log.Printf("[pool] discarded %s (%s); %d message(s) abandoned", logfmt.Label(uid, ""), reason, abandoned)
	}
	need := p.restockNeedLocked()
	warm := p.cfg.WarmSize
	p.mu.Unlock()

	// A discarded identity must never come back after a crash: flush before the
	// request returns and before any restock can add rows.
	p.Flush()
	if need && warm > 0 {
		p.startRestock()
	}
}

// RecordServed counts one confirmed answer. The cartridge serves one request,
// so this is bookkeeping for /status only; the caller still discards it.
func (p *Pool) RecordServed() { p.servedTotal.Add(1) }

// NoteBusy counts a 10061 "busy" refusal. The lease is kept by the caller and
// retried in place, so the pool has no cooldown state to maintain.
func (p *Pool) NoteBusy() { p.busyRetries.Add(1) }

// NoteFailure feeds the pool-wide circuit breaker. An identity is one-shot, so
// there is no per-identity cooldown to maintain.
func (p *Pool) NoteFailure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// Half-open: the cooldown elapsed and a probe is now failing. Reopen at
	// once instead of collecting another full threshold.
	if p.isHalfOpenLocked(now) {
		p.tripBreakerLocked(now)
		return
	}
	if !p.breakerUntil.IsZero() {
		// Still fully open: failures during the open window have no extra
		// effect; the breaker is already protecting the pool.
		return
	}
	// Sliding window: forget a failure streak that has gone stale.
	if !p.breakerLast.IsZero() && now.Sub(p.breakerLast) > p.cfg.BreakerWindow {
		p.breakerFails = 0
	}
	p.breakerLast = now
	p.breakerFails++
	if p.breakerFails >= p.cfg.BreakerThreshold {
		p.tripBreakerLocked(now)
	}
}

// tripBreakerLocked opens the breaker for BreakerCooldown and clears the
// failure count.
func (p *Pool) tripBreakerLocked(now time.Time) {
	p.breakerUntil = now.Add(p.cfg.BreakerCooldown)
	p.breakerFails = 0
	p.breakerLast = time.Time{}
	log.Printf("WARN: [pool] circuit breaker open for %s after %d failures", p.cfg.BreakerCooldown, p.cfg.BreakerThreshold)
}

// isHalfOpenLocked reports whether the cooldown elapsed but the breaker has
// not yet been closed by a successful probe.
func (p *Pool) isHalfOpenLocked(now time.Time) bool {
	return !p.breakerUntil.IsZero() && !now.Before(p.breakerUntil)
}

// breakerOpenLocked reports whether the pool-wide breaker is blocking
// selection. It is a pure read: /status and Servable call it, and observing
// the breaker must not advance its state.
func (p *Pool) breakerOpenLocked(now time.Time) bool {
	return !p.breakerUntil.IsZero() && now.Before(p.breakerUntil)
}

// BreakerOpen reports the pool-wide breaker state for /status.
func (p *Pool) BreakerOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.breakerOpenLocked(time.Now())
}

// NoteSuccess closes the pool breaker for a successful half-open probe. A
// success while the breaker is closed leaves the failure window untouched.
func (p *Pool) NoteSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isHalfOpenLocked(time.Now()) {
		p.breakerUntil = time.Time{}
		p.breakerFails = 0
		p.breakerLast = time.Time{}
	}
}

// GC discards expired idle identities, then restocks toward warm_size. It runs
// from the scheduler.
func (p *Pool) GC() {
	p.mu.Lock()
	now := time.Now()
	var dead []string
	for uid, id := range p.byUID {
		if id.InFlight {
			continue
		}
		if !id.AccessExp.IsZero() && !now.Before(id.AccessExp) {
			dead = append(dead, uid)
		}
	}
	p.mu.Unlock()
	for _, uid := range dead {
		p.discardUID(uid, false, "expired")
	}
	if len(dead) == 0 {
		p.startRestock()
	}
}

// AcquireOne acquires a single identity (used by the scheduler and CLI).
func (p *Pool) AcquireOne(ctx context.Context) (*Identity, error) {
	if err := p.acquireSlot(ctx); err != nil {
		return nil, err
	}
	defer p.releaseSlot()
	if err := p.acquireOnce(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var newest *Identity
	for _, id := range p.byUID {
		if newest == nil || id.CreatedAt.After(newest.CreatedAt) {
			newest = id
		}
	}
	if newest == nil {
		return nil, ErrNoIdentity
	}
	cp := *newest
	return &cp, nil
}

func (p *Pool) acquireSlot(ctx context.Context) error {
	select {
	case p.acquireSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) releaseSlot() {
	select {
	case <-p.acquireSem:
	default:
	}
}

// acquireOnce performs one paced acquisition and stocks the identity.
func (p *Pool) acquireOnce(ctx context.Context) error {
	if p.cfg.Acquirer == nil {
		return ErrNoIdentity
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrNoIdentity
	}
	backoff := p.acquireBackoff
	live := len(p.byUID)
	p.mu.Unlock()

	if backoff > 0 {
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if live >= p.cfg.MaxSize {
		// The hard cap is enforced again at insertion, where the total is
		// exact; checking here only avoids a pointless upstream call.
		return ErrNoIdentity
	}
	// Pace acquisitions with a floor gap, shared by every producer.
	if !p.gate.Wait(ctx) {
		return ctx.Err()
	}

	deviceID := signer.RandomHex32()
	ua := upstream.PickUserAgent()
	xff := ""
	if p.cfg.XFF != nil {
		xff = p.cfg.XFF()
	}
	tokens, err := p.cfg.Acquirer.AcquireGuest(ctx, deviceID, ua, xff)
	if err != nil {
		p.acquireFails.Add(1)
		p.mu.Lock()
		if p.acquireBackoff == 0 {
			p.acquireBackoff = time.Second
		} else {
			p.acquireBackoff *= 2
			if p.acquireBackoff > 60*time.Second {
				p.acquireBackoff = 60 * time.Second
			}
		}
		p.mu.Unlock()
		return err
	}

	now := time.Now()
	id := &Identity{
		UID:         tokens.UID,
		AccessToken: tokens.AccessToken,
		DeviceID:    deviceID,
		UAIndex:     uaIndex(ua),
		CreatedAt:   now,
	}
	if c, err := signer.ParseClaims(tokens.AccessToken); err == nil && c.ExpiresAt > 0 {
		id.AccessExp = c.Expiry()
	} else {
		id.AccessExp = now.Add(24 * time.Hour)
	}

	p.mu.Lock()
	if _, exists := p.byUID[id.UID]; exists {
		p.mu.Unlock()
		return nil // duplicate uid: nothing to add
	}
	// Enforce the hard cap at insertion, the only point where the total is
	// exact; concurrent acquisitions can both pass the pre-flight check.
	if len(p.byUID) >= p.cfg.MaxSize {
		p.mu.Unlock()
		p.acquiredTotal.Add(1) // the identity exists upstream; count the work
		return nil
	}
	p.byUID[id.UID] = id
	p.acquireBackoff = 0
	p.stockOrHandOffLocked(id, false)
	p.mu.Unlock()
	p.acquiredTotal.Add(1)
	return nil
}

func uaIndex(ua upstream.UserAgent) int {
	all := upstream.UserAgents()
	for i := range all {
		if all[i].UA == ua.UA {
			return i
		}
	}
	return 0
}

// restockNeedLocked reports whether more acquisitions are owed: either the
// servable stock is below warm_size, or a request is waiting on an empty stock.
// Dead queue entries are pruned first so an expired row does not masquerade as
// capacity.
func (p *Pool) restockNeedLocked() bool {
	stock := p.stockLocked(time.Now())
	if stock < p.cfg.WarmSize {
		return true
	}
	return stock == 0 && p.waiters.Len() > 0
}

// startRestock launches one eager restock goroutine if needed. The goroutine
// acquires sequentially (the pacing gate shapes it) until the stock is at
// warm_size, an acquisition fails, the pool closes, or the hard cap is hit.
func (p *Pool) startRestock() {
	p.mu.Lock()
	if p.closed || p.restocking || !p.restockNeedLocked() {
		p.mu.Unlock()
		return
	}
	p.restocking = true
	p.wg.Add(1)
	p.mu.Unlock()
	go p.restockLoop()
}

func (p *Pool) restockLoop() {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		p.restocking = false
		need := p.restockNeedLocked() && !p.closed
		p.mu.Unlock()
		// A discard may have landed while the last acquisition was in flight;
		// do not leave the stock short because the waiter saw a stale count.
		if need {
			p.startRestock()
		}
	}()
	for {
		select {
		case <-p.closeCh:
			return
		default:
		}
		p.mu.Lock()
		need := p.restockNeedLocked()
		closed := p.closed
		atCap := len(p.byUID) >= p.cfg.MaxSize
		p.mu.Unlock()
		if closed || !need || atCap {
			// At the hard cap there is no room; the next discard frees a row and
			// triggers a fresh restock.
			return
		}
		ctx, cancel := context.WithTimeout(p.ctx, p.acquireTimeout())
		err := p.acquireOnce(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, ErrNoIdentity) || errors.Is(err, context.Canceled) {
				// A cap race or shutdown is not an acquisition failure.
				return
			}
			p.restockFailures.Add(1)
			log.Printf("WARN: [pool] restock: %v", err)
			return
		}
	}
}

// acquireTimeout bounds one background acquisition.
func (p *Pool) acquireTimeout() time.Duration {
	d := 5 * p.cfg.AcquireWait
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// TopUp acquires until n more identities exist, bounded by MaxSize. Acquisition
// runs with limited concurrency and is paced by the shared gate.
func (p *Pool) TopUp(ctx context.Context, n int) error {
	if n <= 0 || p.cfg.Acquirer == nil {
		return nil
	}
	p.mu.Lock()
	need := n
	room := p.cfg.MaxSize - len(p.byUID)
	if room < need {
		need = room
	}
	p.mu.Unlock()
	if need <= 0 {
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, need)
	var acquired atomic.Int32
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < need; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.acquireSlot(ctx); err != nil {
				errCh <- err
				return
			}
			defer p.releaseSlot()
			if err := p.acquireOnce(ctx); err != nil {
				errCh <- err
				return
			}
			acquired.Add(1)
		}()
	}
	wg.Wait()
	close(errCh)
	if acquired.Load() == 0 {
		for err := range errCh {
			if err != nil {
				return err
			}
		}
		return ErrNoIdentity
	}
	// Surface failure only when nothing was acquired; partial success is fine.
	return nil
}

// Prewarm acquires until the stock holds at least min identities.
func (p *Pool) Prewarm(ctx context.Context, min int) error {
	p.mu.Lock()
	have := p.stockLocked(time.Now())
	p.mu.Unlock()
	if have >= min {
		return nil
	}
	return p.TopUp(ctx, min-have)
}

// Refill tops the stock up to WarmSize when below it. It is intended for the
// scheduler loop and returns the number of acquisitions owed.
//
// It goes through the single eager restock goroutine instead of a parallel
// TopUp: two producers racing toward the same target would overshoot it and
// waste identities. The restock re-checks the target after every acquisition.
func (p *Pool) Refill(ctx context.Context) int {
	p.mu.Lock()
	now := time.Now()
	stock := p.stockLocked(now)
	target := p.cfg.WarmSize
	p.mu.Unlock()
	if stock >= target {
		return 0
	}
	p.startRestock()
	return target - stock
}

// Inspect returns a snapshot of one identity, or nil.
func (p *Pool) Inspect(uid string) *Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.byUID[uid]; ok {
		cp := *id
		return &cp
	}
	return nil
}

// All returns a snapshot of every live identity, oldest first.
func (p *Pool) All() []*Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Identity, 0, len(p.byUID))
	for _, id := range p.byUID {
		cp := *id
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Remove forgets an identity (operator action) and restocks.
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	if _, ok := p.byUID[uid]; !ok {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()
	p.discardUID(uid, false, "removed")
	return true
}

// Stats counters for /status.
type Stats struct {
	AcquiredTotal     int64 `json:"acquired_total"`
	ServedTotal       int64 `json:"served_total"`
	DiscardTotal      int64 `json:"discard_total"`
	RestockFailures   int64 `json:"restock_failures"`
	MessagesAbandoned int64 `json:"messages_abandoned"`
	BusyRetries       int64 `json:"busy_retries"`
	AcquireFailures   int64 `json:"acquire_failures"`
	WaitTimeouts      int64 `json:"wait_timeouts"`
	WaitersTotal      int64 `json:"waiters_total"`
}

// Stats returns lifetime counters.
func (p *Pool) Stats() Stats {
	return Stats{
		AcquiredTotal:     p.acquiredTotal.Load(),
		ServedTotal:       p.servedTotal.Load(),
		DiscardTotal:      p.discardTotal.Load(),
		RestockFailures:   p.restockFailures.Load(),
		MessagesAbandoned: p.messagesAbandoned.Load(),
		BusyRetries:       p.busyRetries.Load(),
		AcquireFailures:   p.acquireFails.Load(),
		WaitTimeouts:      p.waitTimeouts.Load(),
		WaitersTotal:      p.waiterTotal.Load(),
	}
}

// Status is the /status payload.
type Status struct {
	Stock          int            `json:"stock"`
	Total          int            `json:"total"`
	InFlight       int            `json:"in_flight"`
	Waiters        int            `json:"waiters"`
	WarmSize       int            `json:"warm_size"`
	MaxSize        int            `json:"max_size"`
	AcquireFailing bool           `json:"acquire_failing"`
	AcquireBackoff string         `json:"acquire_backoff,omitempty"`
	Pace           PaceStatus     `json:"pace"`
	Stats          Stats          `json:"stats"`
	Identities     []IdentityView `json:"identities"`
}

// PaceStatus exposes the pool-owned pacing. pace.global_qps is enforced by the
// server request pacer and is reported in the /status "pace" sibling field.
type PaceStatus struct {
	AcquireQPS float64 `json:"acquire_qps"`
}

// IdentityView is a token-free identity row.
type IdentityView struct {
	UID           string    `json:"uid"`
	State         string    `json:"state"`
	InFlight      bool      `json:"in_flight"`
	CreatedAt     time.Time `json:"created_at"`
	AccessExpires string    `json:"access_expires,omitempty"`
}

// Status builds the observability payload.
func (p *Pool) Status() *Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	c := p.countsLocked(now)
	st := &Status{
		Stock: c.Stock, Total: c.Total, InFlight: c.InFlight, Waiters: c.Waiters,
		WarmSize: p.cfg.WarmSize, MaxSize: p.cfg.MaxSize,
		AcquireFailing: p.acquireBackoff > 0,
		Stats:          p.Stats(),
		Pace:           PaceStatus{AcquireQPS: p.cfg.AcquireQPS},
	}
	if p.acquireBackoff > 0 {
		st.AcquireBackoff = p.acquireBackoff.String()
	}
	ids := make([]*Identity, 0, len(p.byUID))
	for _, id := range p.byUID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].CreatedAt.Before(ids[j].CreatedAt) })
	for _, id := range ids {
		view := IdentityView{
			UID: id.UID, InFlight: id.InFlight, CreatedAt: id.CreatedAt,
		}
		if id.InFlight {
			view.State = "leased"
		} else {
			view.State = "stock"
		}
		if !id.AccessExp.IsZero() {
			view.AccessExpires = id.AccessExp.Format(time.RFC3339)
		}
		st.Identities = append(st.Identities, view)
	}
	return st
}

// Search returns identities matching a uid prefix (acct CLI).
func (p *Pool) Search(prefix string) []*Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Identity
	for uid, id := range p.byUID {
		if prefix == "" || strings.HasPrefix(uid, prefix) {
			cp := *id
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func removeUID(list []string, uid string) []string {
	for i, q := range list {
		if q == uid {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}
