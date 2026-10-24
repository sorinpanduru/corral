package pool_test

// Harness for the pool: fake connections, a scripted opener and gate, and the
// fake clock. Interface assumptions beyond the plan's text (stated in the
// test-writer report):
//   - pool.CloseReason is a string type (tokens idle, error, not_primary,
//     client_abandoned, shutdown, incompatible); tests convert from strings.
//   - pool.Admission kinds are pool.Ready, pool.Wait, pool.FailFast.
//   - pool.Noop is a struct type implementing Observer and embeddable.
//   - pool.Backoff has a usable zero value (jitter from math/rand) and the
//     pool takes jitter from Config.Rand.
//   - CheckIdle reports unsolicited bytes by returning an error whose
//     CorralErrorDetail() is "unexpected_response" (ADR-0016 duck typing);
//     any other error means the peer closed the connection.
//   - Checkout failures are *pool.CheckoutError; a canceled or expired caller
//     context returns an error satisfying errors.Is(err, ctx.Err()).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
)

var t0 = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

type unsolicited struct{}

func (unsolicited) Error() string             { return "synthetic unsolicited data" }
func (unsolicited) CorralErrorDetail() string { return "unexpected_response" }

type fconn struct {
	host string
	id   int

	mu      sync.Mutex
	closed  []string
	idleErr error
	pool    *fakeOpener
}

func (c *fconn) Host() string { return c.host }
func (c *fconn) Close(reason pool.CloseReason) {
	c.mu.Lock()
	c.closed = append(c.closed, string(reason))
	c.mu.Unlock()
	if c.pool != nil {
		c.pool.live.Add(-1)
	}
}
func (c *fconn) CheckIdle() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idleErr
}
func (c *fconn) setIdleErr(err error) { c.mu.Lock(); c.idleErr = err; c.mu.Unlock() }
func (c *fconn) closeReasons() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.closed...)
}
func (c *fconn) isClosed() bool { return len(c.closeReasons()) > 0 }

type openCall struct {
	Host string
	At   time.Time
	Err  error
}

// fakeOpener opens fconns. In gated mode every Open blocks until release().
type fakeOpener struct {
	clk *fakeclock.Clock

	mu      sync.Mutex
	nextID  int
	calls   []openCall
	failN   int // fail this many upcoming opens
	failAll bool
	gated   bool
	gate    chan struct{}
	conns   []*fconn

	live    atomic.Int64 // opened and not yet closed, plus opens in flight
	peak    atomic.Int64
	started atomic.Int64
	cur     atomic.Int64 // opens in flight
	curPeak atomic.Int64
}

func newOpener(clk *fakeclock.Clock) *fakeOpener {
	return &fakeOpener{clk: clk, gate: make(chan struct{})}
}

func (o *fakeOpener) setGated(g bool) { o.mu.Lock(); o.gated = g; o.mu.Unlock() }
func (o *fakeOpener) releaseAll() {
	o.mu.Lock()
	o.gated = false
	close(o.gate)
	o.gate = make(chan struct{})
	o.mu.Unlock()
}
func (o *fakeOpener) failNext(n int) { o.mu.Lock(); o.failN = n; o.mu.Unlock() }
func (o *fakeOpener) setFailAll(b bool) {
	o.mu.Lock()
	o.failAll = b
	o.mu.Unlock()
}

func (o *fakeOpener) Open(ctx context.Context, host string) (*fconn, error) {
	n := o.live.Add(1)
	for {
		p := o.peak.Load()
		if n <= p || o.peak.CompareAndSwap(p, n) {
			break
		}
	}
	c := o.cur.Add(1)
	for {
		p := o.curPeak.Load()
		if c <= p || o.curPeak.CompareAndSwap(p, c) {
			break
		}
	}
	o.started.Add(1)
	o.mu.Lock()
	at := o.clk.Now()
	gated, gate := o.gated, o.gate
	o.mu.Unlock()
	if gated {
		select {
		case <-gate:
		case <-ctx.Done():
			o.cur.Add(-1)
			o.live.Add(-1)
			return nil, ctx.Err()
		}
	}
	o.mu.Lock()
	fail := o.failAll
	if o.failN > 0 {
		o.failN--
		fail = true
	}
	var cn *fconn
	if !fail {
		o.nextID++
		cn = &fconn{host: host, id: o.nextID, pool: o}
		o.conns = append(o.conns, cn)
	}
	o.calls = append(o.calls, openCall{Host: host, At: at, Err: nil})
	o.mu.Unlock()
	o.cur.Add(-1)
	if fail {
		o.live.Add(-1)
		return nil, errors.New("synthetic open failure")
	}
	return cn, nil
}

func (o *fakeOpener) openCalls() []openCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]openCall(nil), o.calls...)
}
func (o *fakeOpener) all() []*fconn {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fconn(nil), o.conns...)
}

type fakeGate struct {
	mu sync.Mutex
	a  pool.Admission
}

func (g *fakeGate) Admission() pool.Admission { g.mu.Lock(); defer g.mu.Unlock(); return g.a }
func (g *fakeGate) set(a pool.Admission)      { g.mu.Lock(); g.a = a; g.mu.Unlock() }

func ready(host string) pool.Admission { return pool.Admission{Kind: pool.Ready, Target: host} }
func failFast(detail string) pool.Admission {
	return pool.Admission{Kind: pool.FailFast, Detail: detail}
}
func wait() pool.Admission { return pool.Admission{Kind: pool.Wait} }

type ctxKey struct{}

type h struct {
	t    *testing.T
	clk  *fakeclock.Clock
	op   *fakeOpener
	gate *fakeGate
	p    *pool.Pool[*fconn]
	rec  *recorder
}

func newPool(t *testing.T, cfg pool.Config, adm pool.Admission, noop bool) *h {
	t.Helper()
	clk := fakeclock.New(t0)
	op := newOpener(clk)
	g := &fakeGate{a: adm}
	if cfg.CheckoutTimeout == 0 {
		cfg.CheckoutTimeout = time.Hour
	}
	if cfg.Rand == nil {
		cfg.Rand = func() float64 { return 0.5 }
	}
	x := &h{t: t, clk: clk, op: op, gate: g}
	var obs pool.Observer = pool.Noop{}
	if !noop {
		x.rec = &recorder{}
		obs = x.rec
	}
	x.p = pool.New[*fconn](cfg, op, g, clk, obs)
	t.Cleanup(func() { x.p.Close(context.Background()) })
	return x
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (x *h) quiet() { time.Sleep(30 * time.Millisecond) }

func (x *h) waitStats(what string, f func(pool.Stats) bool) pool.Stats {
	x.t.Helper()
	var s pool.Stats
	eventually(x.t, what, func() bool { s = x.p.Stats(); return f(s) })
	return s
}

func (x *h) checkout(t *testing.T) *pool.Lease[*fconn] {
	t.Helper()
	l, err := x.p.Checkout(context.Background())
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	return l
}

type result struct {
	l   *pool.Lease[*fconn]
	err error
	at  time.Time
}

// async runs Checkout in a goroutine and returns its result channel.
func (x *h) async(ctx context.Context) chan result {
	ch := make(chan result, 1)
	go func() {
		l, err := x.p.Checkout(ctx)
		ch <- result{l, err, x.clk.Now()}
	}()
	return ch
}

func detailOf(err error) string {
	var ce *pool.CheckoutError
	if errors.As(err, &ce) {
		return ce.Detail()
	}
	return ""
}

func corralDetail(err error) string {
	var d interface{ CorralErrorDetail() string }
	if errors.As(err, &d) {
		return d.CorralErrorDetail()
	}
	return ""
}

func recv(t *testing.T, ch chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("checkout did not complete")
		return result{}
	}
}

func noResult(t *testing.T, ch chan result) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("checkout completed unexpectedly: err=%v", r.err)
	case <-time.After(30 * time.Millisecond):
	}
}

// recorder is a pool.Observer that records calls.
type recorder struct {
	pool.Noop

	mu          sync.Mutex
	started     int
	queued      int
	succeeded   []time.Duration
	failed      []string
	failedWait  []time.Duration
	opening     int
	opened      []uint64
	openFailed  int
	closed      []string
	unsolicited []string
	stats       []pool.Stats
	ctxs        []context.Context
}

func (r *recorder) CheckoutStarted(ctx context.Context) { r.mu.Lock(); r.started++; r.mu.Unlock() }
func (r *recorder) CheckoutQueued(ctx context.Context)  { r.mu.Lock(); r.queued++; r.mu.Unlock() }
func (r *recorder) CheckoutSucceeded(ctx context.Context, wait time.Duration) {
	r.mu.Lock()
	r.succeeded = append(r.succeeded, wait)
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
}
func (r *recorder) CheckoutFailed(ctx context.Context, detail string, wait time.Duration) {
	r.mu.Lock()
	r.failed = append(r.failed, detail)
	r.failedWait = append(r.failedWait, wait)
	r.mu.Unlock()
}
func (r *recorder) ConnOpening(ctx context.Context, host string) {
	r.mu.Lock()
	r.opening++
	r.mu.Unlock()
}
func (r *recorder) ConnOpened(ctx context.Context, host string, id uint64) {
	r.mu.Lock()
	r.opened = append(r.opened, id)
	r.mu.Unlock()
}
func (r *recorder) ConnOpenFailed(ctx context.Context, host string, err error) {
	r.mu.Lock()
	r.openFailed++
	r.mu.Unlock()
}
func (r *recorder) ConnClosed(ctx context.Context, host string, reason pool.CloseReason) {
	r.mu.Lock()
	r.closed = append(r.closed, host+":"+string(reason))
	r.mu.Unlock()
}
func (r *recorder) ConnUnsolicited(ctx context.Context, host string) {
	r.mu.Lock()
	r.unsolicited = append(r.unsolicited, host)
	r.mu.Unlock()
}
func (r *recorder) StatsChanged(ctx context.Context, s pool.Stats) {
	r.mu.Lock()
	r.stats = append(r.stats, s)
	r.mu.Unlock()
}

func (r *recorder) with(f func(*recorder)) { r.mu.Lock(); defer r.mu.Unlock(); f(r) }
