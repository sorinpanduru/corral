// Package pool is the bounded, multiplexing connection pool of the proxy
// (SPEC-002 FR-25 to FR-29, FR-31, FR-32, FR-35, FR-41, FR-56, FR-62;
// ADR-0015, ADR-0019). It is generic over the connection type, and imports
// the standard library and other domain packages only.
//
// All state is guarded by one mutex. Calls into connections (Close,
// CheckIdle is the one exception, being a non-blocking probe) and observers
// run after the mutex is released.
package pool

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
)

// CloseReason is why a connection was closed.
type CloseReason string

// Close reasons.
const (
	CloseIdle            CloseReason = "idle"
	CloseError           CloseReason = "error"
	CloseNotPrimary      CloseReason = "not_primary"
	CloseClientAbandoned CloseReason = "client_abandoned"
	CloseShutdown        CloseReason = "shutdown"
	CloseIncompatible    CloseReason = "incompatible"
)

// Conn is an upstream connection as the pool sees it.
type Conn interface {
	Host() string
	Close(reason CloseReason)
	// CheckIdle is a non-blocking probe of an idle connection (FR-62). A
	// non-nil error means the connection is unusable; an error whose
	// CorralErrorDetail() is "unexpected_response" means unsolicited bytes.
	CheckIdle() error
}

// Opener opens connections to a host.
type Opener[C Conn] interface {
	Open(ctx context.Context, host string) (C, error)
}

// AdmissionKind is the gate's decision.
type AdmissionKind int

const (
	Ready AdmissionKind = iota
	Wait
	FailFast
)

// Admission is what the gate says about new work.
type Admission struct {
	Kind   AdmissionKind
	Target string
	Detail string
}

// Gate supplies the current admission decision.
type Gate interface{ Admission() Admission }

// Order selects which idle connection is handed out.
type Order int

const (
	// OrderMRU hands out the most recently returned connection.
	OrderMRU Order = iota
	// OrderRoundRobin hands out the least recently returned one.
	OrderRoundRobin
)

// Config bounds the pool. CheckoutTimeout 0 means no timeout and is not a
// valid setting: SPEC-002 requires pool.checkout_timeout > 0, enforced by the
// settings rule (T17); the pool only tolerates 0 for tests.
type Config struct {
	MaxSize, MinSize, MaxWaiters int
	CheckoutTimeout, MaxIdleTime time.Duration
	Order                        Order
	Rand                         func() float64 // jitter source; nil uses math/rand
}

// Stats is a snapshot of the pool.
type Stats struct{ Idle, InUse, Connecting, Waiters int }

// Checkout failure details (FR-27, FR-56, FR-47).
const (
	DetailWaitTimeout = "pool_wait_timeout"
	DetailQueueFull   = "pool_queue_full"
	DetailShutdown    = "shutting_down"
)

// CheckoutError is a failed checkout. Its text is constant.
type CheckoutError struct{ detail string }

// Detail is one of the six corral_pool_checkout_failures_total details.
func (e *CheckoutError) Detail() string { return e.detail }

// CorralErrorDetail implements the errclass.DetailedError contract.
func (e *CheckoutError) CorralErrorDetail() string { return e.detail }

func (e *CheckoutError) Error() string { return "pool: checkout failed: " + e.detail }

// Observer receives pool events for metrics and logs. Calls run outside the
// pool's lock.
type Observer interface {
	CheckoutStarted(ctx context.Context)
	CheckoutQueued(ctx context.Context)
	CheckoutSucceeded(ctx context.Context, wait time.Duration)
	CheckoutFailed(ctx context.Context, detail string, wait time.Duration)
	ConnOpening(ctx context.Context, host string)
	ConnOpened(ctx context.Context, host string, id uint64)
	ConnOpenFailed(ctx context.Context, host string, err error)
	ConnClosed(ctx context.Context, host string, reason CloseReason)
	ConnUnsolicited(ctx context.Context, host string)
	StatsChanged(ctx context.Context, s Stats)
}

// Noop is an Observer that does nothing; embed it to override single methods.
type Noop struct{}

func (Noop) CheckoutStarted(context.Context)                       {}
func (Noop) CheckoutQueued(context.Context)                        {}
func (Noop) CheckoutSucceeded(context.Context, time.Duration)      {}
func (Noop) CheckoutFailed(context.Context, string, time.Duration) {}
func (Noop) ConnOpening(context.Context, string)                   {}
func (Noop) ConnOpened(context.Context, string, uint64)            {}
func (Noop) ConnOpenFailed(context.Context, string, error)         {}
func (Noop) ConnClosed(context.Context, string, CloseReason)       {}
func (Noop) ConnUnsolicited(context.Context, string)               {}
func (Noop) StatsChanged(context.Context, Stats)                   {}

// Backoff is the reconnect delay (FR-56): 100 ms doubling to a 5 s cap, with
// up to 20 percent jitter either way, reset by Reset. The zero value is
// usable and draws jitter from math/rand.
type Backoff struct {
	Rand    func() float64
	attempt int
}

// Backoff constants.
const (
	BackoffInitial = 100 * time.Millisecond
	BackoffMax     = 5 * time.Second
	BackoffJitter  = 0.2
)

// Next returns the next delay and advances the sequence.
func (b *Backoff) Next() time.Duration {
	d := BackoffInitial
	for i := 0; i < b.attempt && d < BackoffMax; i++ {
		d *= 2
	}
	if d > BackoffMax {
		d = BackoffMax
	}
	if b.attempt < 32 {
		b.attempt++
	}
	r := rand.Float64
	if b.Rand != nil {
		r = b.Rand
	}
	f := 1 + (2*r()-1)*BackoffJitter
	return time.Duration(float64(d) * f)
}

// Reset restarts the sequence after a success.
func (b *Backoff) Reset() { b.attempt = 0 }

type outcome[C Conn] struct {
	lease *Lease[C]
	err   error
}

type waiter[C Conn] struct {
	ctx   context.Context
	start time.Time
	ch    chan outcome[C]
	done  bool
	timer clock.Timer
}

type idleConn[C Conn] struct {
	c     C
	since time.Time
}

// Lease is one checked-out connection (FR-26). Return or Discard it exactly
// once.
type Lease[C Conn] struct {
	p        *Pool[C]
	conn     C
	host     string
	aborted  chan struct{}
	abort    sync.Once
	done     bool
	closeOn  CloseReason // set by CloseHost or Close: close on return
	hasClose bool
}

// Conn returns the leased connection.
func (l *Lease[C]) Conn() C { return l.conn }

// Host returns the connection's host.
func (l *Lease[C]) Host() string { return l.host }

// Aborted is closed when the lease's host was closed or its barriers aborted.
func (l *Lease[C]) Aborted() <-chan struct{} { return l.aborted }

func (l *Lease[C]) doAbort() { l.abort.Do(func() { close(l.aborted) }) }

// Return gives the connection back for reuse (or closes it if its host was
// closed meanwhile).
func (l *Lease[C]) Return() { l.p.release(l, "", false) }

// Discard closes the connection with reason.
func (l *Lease[C]) Discard(reason CloseReason) { l.p.release(l, reason, true) }

// Pool is the connection pool.
type Pool[C Conn] struct {
	cfg  Config
	op   Opener[C]
	gate Gate
	clk  clock.Clock
	obs  Observer

	mu        sync.Mutex
	closed    bool
	idle      []idleConn[C]
	leases    map[*Lease[C]]struct{}
	waiters   []*waiter[C]
	connect   int
	nextID    uint64
	backoff   Backoff
	nextOpen  time.Time
	openTimer clock.Timer
	idleTimer clock.Timer
	cancels   map[uint64]context.CancelFunc
	last      Stats
}

// New returns a pool. Nothing is opened until a checkout or AdmissionChanged.
func New[C Conn](cfg Config, op Opener[C], gate Gate, clk clock.Clock, obs Observer) *Pool[C] {
	if obs == nil {
		obs = Noop{}
	}
	p := &Pool[C]{cfg: cfg, op: op, gate: gate, clk: clk, obs: obs,
		leases: map[*Lease[C]]struct{}{}, cancels: map[uint64]context.CancelFunc{}}
	p.backoff.Rand = cfg.Rand
	return p
}

type effects struct{ fns []func() }

func (e *effects) add(f func()) { e.fns = append(e.fns, f) }

func (e *effects) run() {
	for _, f := range e.fns {
		f()
	}
}

func (p *Pool[C]) total() int { return len(p.idle) + len(p.leases) + p.connect }

func (p *Pool[C]) stats() Stats {
	return Stats{Idle: len(p.idle), InUse: len(p.leases), Connecting: p.connect, Waiters: len(p.waiters)}
}

// Stats returns a snapshot.
func (p *Pool[C]) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats()
}

// finish queues a StatsChanged call if the stats moved. Call with p.mu held.
func (p *Pool[C]) finish(fx *effects) {
	if s := p.stats(); s != p.last {
		p.last = s
		fx.add(func() { p.obs.StatsChanged(context.Background(), s) })
	}
}

func (p *Pool[C]) closeConn(c C, reason CloseReason, fx *effects) {
	host := c.Host()
	fx.add(func() {
		c.Close(reason)
		p.obs.ConnClosed(context.Background(), host, reason)
	})
}

func (p *Pool[C]) failWaiter(w *waiter[C], detail string, fx *effects) {
	if w.done {
		return
	}
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	wait := p.clk.Now().Sub(w.start)
	ctx := w.ctx
	fx.add(func() { p.obs.CheckoutFailed(ctx, detail, wait) })
	w.ch <- outcome[C]{err: &CheckoutError{detail}}
}

func (p *Pool[C]) newLease(c C) *Lease[C] {
	l := &Lease[C]{p: p, conn: c, host: c.Host(), aborted: make(chan struct{})}
	p.leases[l] = struct{}{}
	return l
}

func (p *Pool[C]) serveWaiter(w *waiter[C], c C, fx *effects) {
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	l := p.newLease(c)
	wait := p.clk.Now().Sub(w.start)
	ctx := w.ctx
	fx.add(func() { p.obs.CheckoutSucceeded(ctx, wait) })
	w.ch <- outcome[C]{lease: l}
}

func (p *Pool[C]) removeWaiter(w *waiter[C]) {
	for i, x := range p.waiters {
		if x == w {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return
		}
	}
}

// takeIdle returns an idle connection to host that passes the FR-62 probe,
// discarding bad ones on the way. Call with p.mu held.
func (p *Pool[C]) takeIdle(host string, fx *effects) (C, bool) {
	var zero C
	for {
		idx := -1
		if p.cfg.Order == OrderRoundRobin {
			for i := range p.idle {
				if p.idle[i].c.Host() == host {
					idx = i
					break
				}
			}
		} else {
			for i := len(p.idle) - 1; i >= 0; i-- {
				if p.idle[i].c.Host() == host {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			return zero, false
		}
		c := p.idle[idx].c
		p.idle = append(p.idle[:idx], p.idle[idx+1:]...)
		if err := c.CheckIdle(); err != nil {
			p.discardBad(c, err, fx)
			continue
		}
		return c, true
	}
}

type detailed interface{ CorralErrorDetail() string }

func (p *Pool[C]) discardBad(c C, err error, fx *effects) {
	host := c.Host()
	if d, ok := err.(detailed); ok && d.CorralErrorDetail() == "unexpected_response" {
		fx.add(func() { p.obs.ConnUnsolicited(context.Background(), host) })
	}
	p.closeConn(c, CloseError, fx)
}

// dispatch hands connections to waiters, starts opens and keeps the minimum.
// Call with p.mu held.
func (p *Pool[C]) dispatch(fx *effects) {
	if p.closed {
		return
	}
	adm := p.gate.Admission()
	switch adm.Kind {
	case FailFast:
		for _, w := range p.waiters {
			p.failWaiter(w, adm.Detail, fx)
		}
		p.waiters = nil
		return
	case Wait:
		return
	}
	for len(p.waiters) > 0 {
		c, ok := p.takeIdle(adm.Target, fx)
		if !ok {
			break
		}
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.serveWaiter(w, c, fx)
	}
	for p.total() < p.cfg.MaxSize && p.canOpenNow(fx) {
		switch {
		case len(p.waiters) > p.connect:
			p.startOpen(adm.Target, p.waiters[p.connect].ctx, fx)
		case p.total() < p.cfg.MinSize:
			p.startOpen(adm.Target, context.Background(), fx)
		default:
			return
		}
	}
}

// canOpenNow applies the reconnect backoff; when an open must wait it arms a
// timer to dispatch again.
func (p *Pool[C]) canOpenNow(fx *effects) bool {
	if !(len(p.waiters) > p.connect || p.total() < p.cfg.MinSize) {
		return false
	}
	now := p.clk.Now()
	if now.Before(p.nextOpen) {
		if p.openTimer == nil {
			p.openTimer = p.clk.AfterFunc(p.nextOpen.Sub(now), p.onOpenTimer)
		}
		return false
	}
	return true
}

func (p *Pool[C]) onOpenTimer() {
	fx := &effects{}
	p.mu.Lock()
	p.openTimer = nil
	p.dispatch(fx)
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}

func (p *Pool[C]) startOpen(host string, base context.Context, fx *effects) {
	p.connect++
	p.nextID++
	id := p.nextID
	ctx, cancel := context.WithCancel(context.WithoutCancel(base))
	p.cancels[id] = cancel
	// ConnOpening is reported before the open starts, so observers see it
	// before ConnOpened or ConnOpenFailed.
	fx.add(func() { p.obs.ConnOpening(ctx, host) })
	fx.add(func() {
		go func() {
			c, err := p.op.Open(ctx, host)
			p.openDone(ctx, id, host, c, err)
		}()
	})
}

func (p *Pool[C]) openDone(ctx context.Context, id uint64, host string, c C, err error) {
	fx := &effects{}
	p.mu.Lock()
	p.connect--
	canceled := ctx.Err() != nil
	if cancel := p.cancels[id]; cancel != nil {
		cancel()
		delete(p.cancels, id)
	}
	switch {
	case err != nil:
		if !p.closed && !canceled {
			p.nextOpen = p.clk.Now().Add(p.backoff.Next())
			fx.add(func() { p.obs.ConnOpenFailed(ctx, host, err) })
		}
	case p.closed:
		p.closeConn(c, CloseShutdown, fx)
	default:
		p.backoff.Reset()
		p.nextOpen = time.Time{}
		fx.add(func() { p.obs.ConnOpened(ctx, host, id) })
		adm := p.gate.Admission()
		switch {
		case adm.Kind == FailFast && adm.Detail == "upstream_incompatible":
			p.closeConn(c, CloseIncompatible, fx)
		case adm.Kind == FailFast && adm.Detail == "upstream_auth_failed":
			p.closeConn(c, CloseError, fx)
		case adm.Kind != Ready || adm.Target != host:
			p.closeConn(c, CloseNotPrimary, fx)
		case len(p.waiters) > 0:
			w := p.waiters[0]
			p.waiters = p.waiters[1:]
			p.serveWaiter(w, c, fx)
		default:
			p.idle = append(p.idle, idleConn[C]{c: c, since: p.clk.Now()})
		}
	}
	p.dispatch(fx)
	p.armIdle()
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}

// Checkout gives the caller one connection to the target (FR-26). It returns
// a *CheckoutError for pool_queue_full, pool_wait_timeout, shutting_down and
// the fail-fast details, and the caller's context error when the caller goes
// away (no failure is recorded, FR-41).
func (p *Pool[C]) Checkout(ctx context.Context) (*Lease[C], error) {
	p.obs.CheckoutStarted(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fx := &effects{}
	p.mu.Lock()
	start := p.clk.Now()
	fail := func(detail string) (*Lease[C], error) {
		p.finish(fx)
		p.mu.Unlock()
		fx.add(func() { p.obs.CheckoutFailed(ctx, detail, p.clk.Now().Sub(start)) })
		fx.run()
		return nil, &CheckoutError{detail}
	}
	if p.closed {
		return fail(DetailShutdown)
	}
	adm := p.gate.Admission()
	if adm.Kind == FailFast {
		return fail(adm.Detail)
	}
	if adm.Kind == Ready && len(p.waiters) == 0 {
		if c, ok := p.takeIdle(adm.Target, fx); ok {
			l := p.newLease(c)
			fx.add(func() { p.obs.CheckoutSucceeded(ctx, 0) })
			p.armIdle()
			p.finish(fx)
			p.mu.Unlock()
			fx.run()
			return l, nil
		}
	}
	w := &waiter[C]{ctx: ctx, start: start, ch: make(chan outcome[C], 1)}
	p.waiters = append(p.waiters, w)
	if t := p.cfg.CheckoutTimeout; t > 0 {
		w.timer = p.clk.AfterFunc(t, func() { p.timeout(w) })
	}
	p.dispatch(fx)
	switch {
	case w.done:
		// Served or failed during dispatch.
	default:
		idx := 0
		for i, x := range p.waiters {
			if x == w {
				idx = i
			}
		}
		beyond := len(p.waiters) - p.connect
		if beyond > p.cfg.MaxWaiters {
			p.removeWaiter(w)
			w.done = true
			if w.timer != nil {
				w.timer.Stop()
			}
			return fail(DetailQueueFull)
		}
		if idx >= p.connect {
			fx.add(func() { p.obs.CheckoutQueued(ctx) })
		}
	}
	p.finish(fx)
	p.mu.Unlock()
	fx.run()

	select {
	case o := <-w.ch:
		return o.lease, o.err
	case <-ctx.Done():
	}
	fx = &effects{}
	p.mu.Lock()
	if !w.done {
		w.done = true
		if w.timer != nil {
			w.timer.Stop()
		}
		p.removeWaiter(w)
		p.dispatch(fx)
		p.finish(fx)
		p.mu.Unlock()
		fx.run()
		return nil, ctx.Err()
	}
	p.mu.Unlock()
	// The outcome arrived at the same moment as the cancellation.
	o := <-w.ch
	if o.lease != nil {
		o.lease.Return()
	}
	return nil, ctx.Err()
}

func (p *Pool[C]) timeout(w *waiter[C]) {
	fx := &effects{}
	p.mu.Lock()
	if !w.done {
		p.removeWaiter(w)
		p.failWaiter(w, DetailWaitTimeout, fx)
		p.finish(fx)
	}
	p.mu.Unlock()
	fx.run()
}

// release returns or discards a lease.
func (p *Pool[C]) release(l *Lease[C], reason CloseReason, discard bool) {
	fx := &effects{}
	p.mu.Lock()
	if l.done {
		p.mu.Unlock()
		return
	}
	l.done = true
	delete(p.leases, l)
	switch {
	case discard:
		p.closeConn(l.conn, reason, fx)
	case p.closed:
		p.closeConn(l.conn, CloseShutdown, fx)
	case l.hasClose:
		p.closeConn(l.conn, l.closeOn, fx)
	default:
		p.idle = append(p.idle, idleConn[C]{c: l.conn, since: p.clk.Now()})
	}
	p.dispatch(fx)
	p.armIdle()
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}

// AdmissionChanged tells the pool the gate's decision changed: waiters are
// served or failed, opens start, the minimum is refilled.
func (p *Pool[C]) AdmissionChanged() {
	fx := &effects{}
	p.mu.Lock()
	p.dispatch(fx)
	p.armIdle()
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}

// CloseHost closes idle connections to host at once, marks leased ones to be
// closed on return, and aborts their barriers (FR-29, FR-35, FR-18).
func (p *Pool[C]) CloseHost(host string, reason CloseReason) {
	fx := &effects{}
	p.mu.Lock()
	kept := p.idle[:0]
	for _, ic := range p.idle {
		if ic.c.Host() == host {
			p.closeConn(ic.c, reason, fx)
		} else {
			kept = append(kept, ic)
		}
	}
	p.idle = kept
	for l := range p.leases {
		if l.host == host {
			l.closeOn, l.hasClose = reason, true
			l.doAbort()
		}
	}
	p.dispatch(fx)
	p.armIdle()
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}

// AbortBarriers aborts the barriers of leases on host without closing
// anything (the target is unknown, FR-35).
func (p *Pool[C]) AbortBarriers(host string) {
	p.mu.Lock()
	for l := range p.leases {
		if l.host == host {
			l.doAbort()
		}
	}
	p.mu.Unlock()
}

// DiscardIdle removes an idle connection at once (FR-62), for the idle
// watcher. It does nothing if the connection is not idle.
func (p *Pool[C]) DiscardIdle(c C, err error) {
	fx := &effects{}
	p.mu.Lock()
	for i, ic := range p.idle {
		if any(ic.c) == any(c) {
			p.idle = append(p.idle[:i], p.idle[i+1:]...)
			p.discardBad(c, err, fx)
			p.dispatch(fx)
			p.armIdle()
			p.finish(fx)
			break
		}
	}
	p.mu.Unlock()
	fx.run()
}

// armIdle schedules the next idle expiry (FR-28). Call with p.mu held.
func (p *Pool[C]) armIdle() {
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
	if p.closed || p.cfg.MaxIdleTime <= 0 || len(p.idle) == 0 || p.total() <= p.cfg.MinSize {
		return
	}
	oldest := p.idle[0].since
	for _, ic := range p.idle {
		if ic.since.Before(oldest) {
			oldest = ic.since
		}
	}
	d := oldest.Add(p.cfg.MaxIdleTime).Sub(p.clk.Now())
	if d < 0 {
		d = 0
	}
	p.idleTimer = p.clk.AfterFunc(d, p.onIdleTimer)
}

func (p *Pool[C]) onIdleTimer() {
	fx := &effects{}
	p.mu.Lock()
	p.idleTimer = nil
	now := p.clk.Now()
	for p.total() > p.cfg.MinSize {
		oi := -1
		for i, ic := range p.idle {
			if now.Sub(ic.since) >= p.cfg.MaxIdleTime && (oi < 0 || ic.since.Before(p.idle[oi].since)) {
				oi = i
			}
		}
		if oi < 0 {
			break
		}
		c := p.idle[oi].c
		p.idle = append(p.idle[:oi], p.idle[oi+1:]...)
		p.closeConn(c, CloseIdle, fx)
	}
	p.armIdle()
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}

// Close shuts the pool down (FR-47 step 5): queued waiters fail with
// shutting_down, idle connections close, leased ones close on return, and new
// checkouts are refused.
func (p *Pool[C]) Close(ctx context.Context) {
	fx := &effects{}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.openTimer != nil {
		p.openTimer.Stop()
		p.openTimer = nil
	}
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
	for _, cancel := range p.cancels {
		cancel()
	}
	for _, w := range p.waiters {
		p.failWaiter(w, DetailShutdown, fx)
	}
	p.waiters = nil
	for _, ic := range p.idle {
		p.closeConn(ic.c, CloseShutdown, fx)
	}
	p.idle = nil
	for l := range p.leases {
		l.closeOn, l.hasClose = CloseShutdown, true
	}
	p.finish(fx)
	p.mu.Unlock()
	fx.run()
}
