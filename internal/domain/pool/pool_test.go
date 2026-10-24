package pool_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
)

const H1 = "h1.test:27017"
const H2 = "h2.test:27017"

func cfg(max int) pool.Config {
	return pool.Config{MaxSize: max, MaxWaiters: 1000, CheckoutTimeout: time.Hour}
}

// FR-26: one lease per checkout; Return makes the connection reusable (MRU by
// default).
func TestCheckoutReturnReuse_FR26(t *testing.T) {
	x := newPool(t, cfg(4), ready(H1), false)
	l := x.checkout(t)
	if l.Host() != H1 || l.Conn().Host() != H1 {
		t.Fatalf("host %q", l.Host())
	}
	c1 := l.Conn()
	if s := x.p.Stats(); s.InUse != 1 || s.Idle != 0 {
		t.Fatalf("stats: %+v", s)
	}
	l.Return()
	if s := x.p.Stats(); s.InUse != 0 || s.Idle != 1 {
		t.Fatalf("stats after return: %+v", s)
	}
	l2 := x.checkout(t)
	if l2.Conn() != c1 {
		t.Fatal("the idle connection was not reused")
	}
	if n := len(x.op.openCalls()); n != 1 {
		t.Fatalf("opens = %d, want 1", n)
	}
	l2.Return()
}

// FR-26: Discard closes the connection with the reason and frees the slot.
func TestDiscard_FR26(t *testing.T) {
	x := newPool(t, cfg(1), ready(H1), false)
	l := x.checkout(t)
	c := l.Conn()
	l.Discard("error")
	if r := c.closeReasons(); len(r) != 1 || r[0] != "error" {
		t.Fatalf("close reasons: %v", r)
	}
	if s := x.p.Stats(); s.InUse != 0 || s.Idle != 0 {
		t.Fatalf("stats: %+v", s)
	}
	l2 := x.checkout(t) // the slot is free again
	if l2.Conn() == c {
		t.Fatal("a discarded connection was reused")
	}
	l2.Return()
	x.rec.with(func(r *recorder) {
		if len(r.closed) != 1 || r.closed[0] != H1+":error" {
			t.Errorf("observer closes: %v", r.closed)
		}
	})
}

// AC-12 (unit): MaxSize counts connections being opened. With opens held in
// flight, at most MaxSize opens start; at no instant do more than MaxSize
// connections exist.
func TestBoundsCountConnecting_AC12(t *testing.T) {
	x := newPool(t, cfg(4), ready(H1), false)
	x.op.setGated(true)
	var chans []chan result
	for i := 0; i < 10; i++ {
		chans = append(chans, x.async(context.Background()))
	}
	x.waitStats("4 connecting", func(s pool.Stats) bool { return s.Connecting == 4 })
	x.quiet()
	s := x.p.Stats()
	if s.Connecting != 4 || s.Waiters != 10 && s.Waiters != 6 {
		// Waiters may count all 10 until connections are handed out, or the 6
		// without a connection in progress.
		t.Logf("stats: %+v", s)
	}
	if x.op.curPeak.Load() > 4 {
		t.Fatalf("%d opens in flight, max 4", x.op.curPeak.Load())
	}
	x.op.releaseAll()
	var leases []*pool.Lease[*fconn]
	eventually(t, "4 leases", func() bool {
		for i, ch := range chans {
			if ch == nil {
				continue
			}
			select {
			case r := <-ch:
				if r.err != nil {
					t.Fatal(r.err)
				}
				chans[i] = nil
				leases = append(leases, r.l)
			default:
			}
		}
		return len(leases) >= 4
	})
	x.quiet()
	for i, ch := range chans {
		if ch == nil {
			continue
		}
		select {
		case r := <-ch:
			chans[i] = nil
			leases = append(leases, r.l)
		default:
		}
	}
	if len(leases) != 4 {
		t.Fatalf("%d leases, want 4 (the rest must wait)", len(leases))
	}
	for _, l := range leases {
		l.Return()
	}
	// The remaining six are served by the same 4 connections.
	got := 4
	for got < 10 {
		eventually(t, "waiter served", func() bool {
			for i, ch := range chans {
				if ch == nil {
					continue
				}
				select {
				case r := <-ch:
					if r.err != nil {
						t.Fatal(r.err)
					}
					chans[i] = nil
					got++
					r.l.Return()
					return true
				default:
				}
			}
			return false
		})
	}
	if x.op.peak.Load() > 4 {
		t.Fatalf("peak connections = %d, max 4", x.op.peak.Load())
	}
}

// AC-12 (unit): 200 concurrent checkouts on MaxSize=4 never create more than
// four connections, and all complete.
func TestTwoHundredCheckoutsBounded_AC12(t *testing.T) {
	x := newPool(t, cfg(4), ready(H1), false)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	hold := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := x.p.Checkout(context.Background())
			if err != nil {
				errs <- err
				return
			}
			<-hold
			l.Return()
		}()
	}
	x.waitStats("4 in use", func(s pool.Stats) bool { return s.InUse == 4 })
	x.quiet()
	if s := x.p.Stats(); s.InUse+s.Idle+s.Connecting > 4 {
		t.Fatalf("stats exceed the bound: %+v", s)
	}
	close(hold)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("checkouts did not complete")
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if x.op.peak.Load() > 4 {
		t.Fatalf("peak connections = %d, max 4", x.op.peak.Load())
	}
}

// FR-25: MinSize is kept while the gate is Ready and not while it waits.
func TestMinSize_FR25(t *testing.T) {
	c := cfg(5)
	c.MinSize = 2
	x := newPool(t, c, ready(H1), false)
	x.p.AdmissionChanged()
	x.waitStats("2 idle", func(s pool.Stats) bool { return s.Idle == 2 })
	for _, cn := range x.op.all() {
		if cn.host != H1 {
			t.Fatalf("minimum connection to %q", cn.host)
		}
	}
	// Gate in Wait: closing the idle connections does not refill them.
	x.gate.set(wait())
	x.p.AdmissionChanged()
	n := len(x.op.openCalls())
	x.p.CloseHost(H1, "not_primary")
	x.waitStats("empty", func(s pool.Stats) bool { return s.Idle == 0 })
	x.quiet()
	if got := len(x.op.openCalls()); got != n {
		t.Fatalf("opens while waiting: %d -> %d", n, got)
	}
	// Ready again with a new target: refilled on the new host.
	x.gate.set(ready(H2))
	x.p.AdmissionChanged()
	x.waitStats("2 idle again", func(s pool.Stats) bool { return s.Idle == 2 })
	for _, cn := range x.op.all()[2:] {
		if cn.host != H2 {
			t.Fatalf("refill to %q, want %q", cn.host, H2)
		}
	}
}

// FR-27, AC-13 (unit): with MaxSize=1 and the lease held, a second checkout
// fails with pool_wait_timeout exactly at CheckoutTimeout; with MaxWaiters=1 a
// third fails at once with pool_queue_full.
func TestQueueTimeoutAndFull_AC13(t *testing.T) {
	c := pool.Config{MaxSize: 1, MaxWaiters: 1, CheckoutTimeout: 5 * time.Second}
	x := newPool(t, c, ready(H1), false)
	held := x.checkout(t)
	second := x.async(context.Background())
	x.waitStats("one waiter", func(s pool.Stats) bool { return s.Waiters == 1 })

	// Third: queue full, at once, without any clock movement.
	l, err := x.p.Checkout(context.Background())
	if l != nil || detailOf(err) != "pool_queue_full" || corralDetail(err) != "pool_queue_full" {
		t.Fatalf("third checkout: lease=%v detail=%q", l, detailOf(err))
	}

	x.clk.WaitForTimers(1, 2*time.Second)
	x.clk.Advance(5*time.Second - time.Millisecond)
	noResult(t, second)
	x.clk.Advance(time.Millisecond)
	r := recv(t, second)
	if r.err == nil || detailOf(r.err) != "pool_wait_timeout" || corralDetail(r.err) != "pool_wait_timeout" {
		t.Fatalf("second checkout: %v (detail %q)", r.err, detailOf(r.err))
	}
	if s := x.p.Stats(); s.Waiters != 0 {
		t.Fatalf("waiters after the timeout: %d", s.Waiters)
	}
	x.rec.with(func(r *recorder) {
		if len(r.failed) != 2 || r.failed[0] != "pool_queue_full" || r.failed[1] != "pool_wait_timeout" {
			t.Errorf("observer failures: %v", r.failed)
		}
		if len(r.failedWait) == 2 && r.failedWait[1] != 5*time.Second {
			t.Errorf("recorded wait for the timeout = %v, want 5s", r.failedWait[1])
		}
	})
	held.Return()
}

// FR-27: the wait queue is FIFO.
func TestFIFO_FR27(t *testing.T) {
	c := pool.Config{MaxSize: 1, MaxWaiters: 10, CheckoutTimeout: time.Hour}
	x := newPool(t, c, ready(H1), false)
	held := x.checkout(t)
	var chans []chan result
	for i := 0; i < 5; i++ {
		chans = append(chans, x.async(context.Background()))
		x.waitStats("queued", func(s pool.Stats) bool { return s.Waiters == i+1 })
	}
	cur := held
	for i, ch := range chans {
		cur.Return()
		r := recv(t, ch)
		if r.err != nil {
			t.Fatalf("waiter %d: %v", i, r.err)
		}
		for j := i + 1; j < len(chans); j++ {
			select {
			case <-chans[j]:
				t.Fatalf("waiter %d was served before waiter %d", j, i)
			default:
			}
		}
		cur = r.l
	}
	cur.Return()
}

// FR-56 (AC-32 unit): switching the gate to FailFast fails every queued waiter
// at once with the detail; new checkouts fail at once too.
func TestFailFast_AC32(t *testing.T) {
	for _, detail := range []string{"upstream_auth_failed", "upstream_incompatible", "upstream_unavailable"} {
		t.Run(detail, func(t *testing.T) {
			x := newPool(t, pool.Config{MaxSize: 1, MaxWaiters: 10, CheckoutTimeout: time.Hour}, ready(H1), false)
			held := x.checkout(t)
			var chans []chan result
			for i := 0; i < 4; i++ {
				chans = append(chans, x.async(context.Background()))
			}
			x.waitStats("4 waiters", func(s pool.Stats) bool { return s.Waiters == 4 })
			x.gate.set(failFast(detail))
			x.p.AdmissionChanged()
			for i, ch := range chans {
				r := recv(t, ch)
				if detailOf(r.err) != detail || corralDetail(r.err) != detail {
					t.Fatalf("waiter %d: detail %q err=%v", i, detailOf(r.err), r.err)
				}
			}
			_, err := x.p.Checkout(context.Background())
			if detailOf(err) != detail {
				t.Fatalf("new checkout: detail %q", detailOf(err))
			}
			if s := x.p.Stats(); s.Waiters != 0 {
				t.Fatalf("waiters left: %d", s.Waiters)
			}
			held.Return()
		})
	}
}

// FR-56: a Wait admission queues without opening anything; becoming Ready
// serves the queue by opening to the target.
func TestWaitAdmissionNoOpens_AC32(t *testing.T) {
	x := newPool(t, cfg(2), wait(), false)
	ch := x.async(context.Background())
	x.waitStats("queued", func(s pool.Stats) bool { return s.Waiters == 1 })
	x.quiet()
	if n := len(x.op.openCalls()); n != 0 || x.op.started.Load() != 0 {
		t.Fatalf("Open called %d times while waiting", n)
	}
	noResult(t, ch)
	x.gate.set(ready(H2))
	x.p.AdmissionChanged()
	r := recv(t, ch)
	if r.err != nil || r.l.Host() != H2 {
		t.Fatalf("after Ready: err=%v host=%q", r.err, r.l.Host())
	}
	r.l.Return()
}

// FR-56: a Wait admission also times out with pool_wait_timeout.
func TestWaitAdmissionTimesOut_FR56(t *testing.T) {
	x := newPool(t, pool.Config{MaxSize: 2, MaxWaiters: 5, CheckoutTimeout: 2 * time.Second}, wait(), false)
	ch := x.async(context.Background())
	x.waitStats("queued", func(s pool.Stats) bool { return s.Waiters == 1 })
	x.clk.WaitForTimers(1, 2*time.Second)
	x.clk.Advance(2 * time.Second)
	if r := recv(t, ch); detailOf(r.err) != "pool_wait_timeout" {
		t.Fatalf("detail %q", detailOf(r.err))
	}
}

// AC-21 (unit): a queued checkout whose context is canceled leaves the queue
// with no Open for it and no failure record.
func TestQueuedCancel_AC21(t *testing.T) {
	x := newPool(t, pool.Config{MaxSize: 1, MaxWaiters: 5, CheckoutTimeout: time.Hour}, ready(H1), false)
	held := x.checkout(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := x.async(ctx)
	x.waitStats("queued", func(s pool.Stats) bool { return s.Waiters == 1 })
	opens := len(x.op.openCalls())
	cancel()
	r := recv(t, ch)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", r.err)
	}
	x.waitStats("queue empty", func(s pool.Stats) bool { return s.Waiters == 0 })
	held.Return()
	x.quiet()
	if got := len(x.op.openCalls()); got != opens {
		t.Fatalf("Open called for a canceled waiter: %d -> %d", opens, got)
	}
	// The returned connection stays idle; nobody consumed it.
	if s := x.p.Stats(); s.Idle != 1 {
		t.Fatalf("stats: %+v", s)
	}
	x.rec.with(func(r *recorder) {
		if len(r.failed) != 0 {
			t.Errorf("a canceled waiter was recorded as a failure: %v", r.failed)
		}
	})
}

// FR-28: idle connections expire after MaxIdleTime, keeping MinSize.
func TestIdleExpiry_FR28(t *testing.T) {
	c := cfg(5)
	c.MinSize = 1
	c.MaxIdleTime = time.Minute
	x := newPool(t, c, ready(H1), false)
	var ls []*pool.Lease[*fconn]
	for i := 0; i < 3; i++ {
		ls = append(ls, x.checkout(t))
	}
	for _, l := range ls {
		l.Return()
	}
	x.waitStats("3 idle", func(s pool.Stats) bool { return s.Idle == 3 })
	x.clk.WaitForTimers(1, 2*time.Second)
	x.clk.Advance(59 * time.Second)
	x.quiet()
	if s := x.p.Stats(); s.Idle != 3 {
		t.Fatalf("expired early: %+v", s)
	}
	x.clk.Advance(2 * time.Second)
	x.waitStats("down to the minimum", func(s pool.Stats) bool { return s.Idle == 1 })
	x.quiet()
	if s := x.p.Stats(); s.Idle != 1 {
		t.Fatalf("below the minimum: %+v", s)
	}
	closed := 0
	for _, cn := range x.op.all() {
		for _, r := range cn.closeReasons() {
			if r != "idle" {
				t.Fatalf("close reason %q, want idle", r)
			}
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("closed %d connections, want 2", closed)
	}
}

func TestIdleExpiryDisabled_FR28(t *testing.T) {
	c := cfg(5)
	c.MaxIdleTime = 0
	x := newPool(t, c, ready(H1), false)
	l1, l2 := x.checkout(t), x.checkout(t)
	l1.Return()
	l2.Return()
	x.clk.Advance(24 * time.Hour)
	x.quiet()
	if s := x.p.Stats(); s.Idle != 2 {
		t.Fatalf("connections expired with MaxIdleTime=0: %+v", s)
	}
}

// AC-16 (unit): the default order is most-recently-used; OrderRoundRobin
// alternates between idle connections.
func TestOrders_AC16(t *testing.T) {
	prep := func(order pool.Order) (*h, []*fconn) {
		c := cfg(4)
		c.Order = order
		x := newPool(t, c, ready(H1), false)
		l1, l2 := x.checkout(t), x.checkout(t)
		cs := []*fconn{l1.Conn(), l2.Conn()}
		l1.Return()
		l2.Return()
		return x, cs
	}
	x, cs := prep(pool.OrderMRU)
	first := x.checkout(t)
	c := first.Conn()
	first.Return()
	for i := 0; i < 4; i++ {
		l := x.checkout(t)
		if l.Conn() != c {
			t.Fatal("MRU order alternated")
		}
		l.Return()
	}
	_ = cs

	x, _ = prep(pool.OrderRoundRobin)
	seen := map[*fconn]int{}
	var seq []*fconn
	for i := 0; i < 8; i++ {
		l := x.checkout(t)
		seen[l.Conn()]++
		seq = append(seq, l.Conn())
		l.Return()
	}
	if len(seen) != 2 {
		t.Fatalf("round robin used %d connections, want 2", len(seen))
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			t.Fatalf("round robin repeated a connection at step %d", i)
		}
	}
}

// FR-29, FR-35 (AC-14 unit): CloseHost closes idle connections at once, closes
// leased ones on return and aborts leases on that host; other hosts are
// untouched.
func TestCloseHost_AC14(t *testing.T) {
	x := newPool(t, cfg(6), ready(H1), false)
	leased := x.checkout(t)
	idleL := x.checkout(t)
	idleC := idleL.Conn()
	idleL.Return()
	x.gate.set(ready(H2))
	other := x.checkout(t) // a lease on H2
	if other.Host() != H2 {
		t.Fatalf("host %q", other.Host())
	}
	x.p.CloseHost(H1, "not_primary")
	if r := idleC.closeReasons(); len(r) != 1 || r[0] != "not_primary" {
		t.Fatalf("idle connection close reasons: %v", r)
	}
	select {
	case <-leased.Aborted():
	default:
		t.Fatal("Aborted() not closed for a lease on the host")
	}
	select {
	case <-other.Aborted():
		t.Fatal("a lease on another host was aborted")
	default:
	}
	if leased.Conn().isClosed() {
		t.Fatal("a leased connection was closed before return")
	}
	lc := leased.Conn()
	leased.Return()
	if r := lc.closeReasons(); len(r) != 1 || r[0] != "not_primary" {
		t.Fatalf("returned connection close reasons: %v", r)
	}
	if other.Conn().isClosed() {
		t.Fatal("a connection to another host was closed")
	}
	if s := x.p.Stats(); s.InUse != 1 || s.Idle != 0 {
		t.Fatalf("stats: %+v", s)
	}
	other.Return()
	// The closed connection is not handed out again: the next lease on H1 is new.
	x.gate.set(ready(H1))
	l := x.checkout(t)
	if l.Conn() == lc || l.Conn() == idleC {
		t.Fatal("a closed connection was reused")
	}
	l.Return()
}

// FR-35, FR-29: AbortBarriers aborts leases on the host without closing idle
// connections.
func TestAbortBarriers_AC14(t *testing.T) {
	x := newPool(t, cfg(4), ready(H1), false)
	leased := x.checkout(t)
	l2 := x.checkout(t)
	c2 := l2.Conn()
	l2.Return()
	x.p.AbortBarriers(H1)
	select {
	case <-leased.Aborted():
	default:
		t.Fatal("Aborted() not closed")
	}
	if c2.isClosed() {
		t.Fatal("AbortBarriers closed an idle connection")
	}
	if s := x.p.Stats(); s.Idle != 1 {
		t.Fatalf("stats: %+v", s)
	}
	// Aborted() of a fresh lease is open.
	x.p.Close(context.Background())
}

func TestAbortedOpenForFreshLease(t *testing.T) {
	x := newPool(t, cfg(2), ready(H1), false)
	l := x.checkout(t)
	select {
	case <-l.Aborted():
		t.Fatal("a fresh lease is already aborted")
	default:
	}
	l.Return()
}

// FR-18: a closed host is not used again until the gate names it.
func TestCloseHostIncompatible_FR18(t *testing.T) {
	x := newPool(t, cfg(2), ready(H1), false)
	l := x.checkout(t)
	c := l.Conn()
	l.Return()
	x.p.CloseHost(H1, "incompatible")
	if r := c.closeReasons(); len(r) != 1 || r[0] != "incompatible" {
		t.Fatalf("close reasons: %v", r)
	}
}

// FR-47 step 5: Close closes idle connections, fails queued waiters with
// shutting_down, closes leased ones on return, and refuses new checkouts.
func TestCloseSequence_FR47(t *testing.T) {
	x := newPool(t, pool.Config{MaxSize: 2, MaxWaiters: 5, CheckoutTimeout: time.Hour}, ready(H1), false)
	l1, l2 := x.checkout(t), x.checkout(t)
	c2 := l2.Conn()
	l2.Return() // one idle, one leased
	// Take the idle one so the next checkout has to wait.
	l3 := x.checkout(t)
	w := x.async(context.Background())
	x.waitStats("a waiter", func(s pool.Stats) bool { return s.Waiters >= 1 })
	x.p.Close(context.Background())
	r := recv(t, w)
	if detailOf(r.err) != "shutting_down" || corralDetail(r.err) != "shutting_down" {
		t.Fatalf("waiter after Close: detail %q", detailOf(r.err))
	}
	if _, err := x.p.Checkout(context.Background()); detailOf(err) != "shutting_down" {
		t.Fatalf("checkout after Close: detail %q", detailOf(err))
	}
	l1c := l1.Conn()
	l1.Return()
	l3.Return()
	for _, c := range []*fconn{l1c, c2} {
		eventually(t, "closed", func() bool { return c.isClosed() })
		if r := c.closeReasons(); r[0] != "shutdown" {
			t.Fatalf("close reason %q, want shutdown", r[0])
		}
	}
}

// Observer: wait times come from the fake clock; opens are reported.
func TestObserverCalls_AC2(t *testing.T) {
	c := pool.Config{MaxSize: 1, MaxWaiters: 5, CheckoutTimeout: time.Hour}
	x := newPool(t, c, ready(H1), false)
	held := x.checkout(t)
	ch := x.async(context.WithValue(context.Background(), ctxKey{}, "caller"))
	x.waitStats("queued", func(s pool.Stats) bool { return s.Waiters == 1 })
	x.clk.Advance(3 * time.Second)
	held.Return()
	r := recv(t, ch)
	if r.err != nil {
		t.Fatal(r.err)
	}
	x.rec.with(func(o *recorder) {
		if o.started != 2 || o.queued != 1 {
			t.Errorf("started=%d queued=%d, want 2 and 1", o.started, o.queued)
		}
		if len(o.succeeded) != 2 || o.succeeded[0] != 0 || o.succeeded[1] != 3*time.Second {
			t.Errorf("wait times: %v, want [0 3s]", o.succeeded)
		}
		if o.opening != 1 || len(o.opened) != 1 {
			t.Errorf("opening=%d opened=%v", o.opening, o.opened)
		}
		seen := false
		for _, cx := range o.ctxs {
			if cx.Value(ctxKey{}) == "caller" {
				seen = true
			}
		}
		if !seen {
			t.Error("CheckoutSucceeded did not receive the caller's context")
		}
		if len(o.stats) == 0 {
			t.Error("StatsChanged never called")
		}
	})
	r.l.Return()
}

// AC-2 Part B: results with Noop and with a recording observer are the same.
func TestObserverEquivalence_AC2(t *testing.T) {
	run := func(noop bool) string {
		x := newPool(t, pool.Config{MaxSize: 2, MaxWaiters: 1, CheckoutTimeout: 5 * time.Second}, ready(H1), noop)
		a, b := x.checkout(t), x.checkout(t)
		w := x.async(context.Background())
		x.waitStats("queued", func(s pool.Stats) bool { return s.Waiters == 1 })
		_, err := x.p.Checkout(context.Background())
		out := fmt.Sprintf("full=%s ", detailOf(err))
		x.clk.WaitForTimers(1, time.Second)
		x.clk.Advance(5 * time.Second)
		out += fmt.Sprintf("timeout=%s ", detailOf(recv(t, w).err))
		a.Return()
		b.Discard("error")
		out += fmt.Sprintf("stats=%+v", x.p.Stats())
		return out
	}
	if a, b := run(true), run(false); a != b {
		t.Fatalf("differ:\n noop: %s\n rec:  %s", a, b)
	}
}
