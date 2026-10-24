package pool_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
)

func twoIdle(t *testing.T, c pool.Config) (*h, *fconn, *fconn) {
	x := newPool(t, c, ready(H1), false)
	l1, l2 := x.checkout(t), x.checkout(t)
	a, b := l1.Conn(), l2.Conn()
	l1.Return()
	l2.Return() // b is the most recently used
	return x, a, b
}

// FR-62 (AC-46 unit): an idle connection that reports unsolicited bytes is
// discarded at checkout with reason=error, reported as ConnUnsolicited, and
// the same checkout succeeds on the other idle connection.
func TestCheckoutDiscardsConnectionWithBytes_AC46(t *testing.T) {
	x, a, b := twoIdle(t, cfg(4))
	b.setIdleErr(unsolicited{})
	l := x.checkout(t)
	if l.Conn() != a {
		t.Fatal("the checkout did not continue on the other idle connection")
	}
	if r := b.closeReasons(); len(r) != 1 || r[0] != "error" {
		t.Fatalf("discarded connection close reasons: %v", r)
	}
	x.rec.with(func(r *recorder) {
		if len(r.unsolicited) != 1 || r.unsolicited[0] != H1 {
			t.Errorf("ConnUnsolicited calls: %v", r.unsolicited)
		}
	})
	if len(x.op.openCalls()) != 2 {
		t.Fatalf("a new connection was opened although another was idle: %d opens", len(x.op.openCalls()))
	}
	l.Return()
}

// With no other idle connection a new one is opened within MaxSize, counting
// the discarded one as gone.
func TestCheckoutOpensReplacement_AC46(t *testing.T) {
	x := newPool(t, cfg(1), ready(H1), false)
	l := x.checkout(t)
	bad := l.Conn()
	l.Return()
	bad.setIdleErr(unsolicited{})
	l2 := x.checkout(t)
	if l2.Conn() == bad {
		t.Fatal("the bad connection was handed out")
	}
	if len(x.op.openCalls()) != 2 {
		t.Fatalf("opens = %d, want 2", len(x.op.openCalls()))
	}
	if s := x.p.Stats(); s.InUse != 1 || s.Idle != 0 {
		t.Fatalf("stats: %+v", s)
	}
	if r := bad.closeReasons(); len(r) != 1 || r[0] != "error" {
		t.Fatalf("close reasons: %v", r)
	}
	l2.Return()
}

// A peer close is discarded with reason=error and no ConnUnsolicited.
func TestCheckoutDiscardsClosedConnection_AC46(t *testing.T) {
	x, a, b := twoIdle(t, cfg(4))
	b.setIdleErr(errors.New("synthetic peer close"))
	l := x.checkout(t)
	if l.Conn() != a {
		t.Fatal("checkout did not continue on the other connection")
	}
	if r := b.closeReasons(); len(r) != 1 || r[0] != "error" {
		t.Fatalf("close reasons: %v", r)
	}
	x.rec.with(func(r *recorder) {
		if len(r.unsolicited) != 0 {
			t.Errorf("ConnUnsolicited called for a peer close: %v", r.unsolicited)
		}
		if len(r.closed) == 0 || r.closed[len(r.closed)-1] != H1+":error" {
			t.Errorf("ConnClosed: %v", r.closed)
		}
	})
	l.Return()
}

// Several bad idle connections in a row are all discarded in one checkout.
func TestCheckoutDiscardsSeveralBad_AC46(t *testing.T) {
	x := newPool(t, cfg(4), ready(H1), false)
	var cs []*fconn
	var ls []*pool.Lease[*fconn]
	for i := 0; i < 3; i++ {
		l := x.checkout(t)
		cs = append(cs, l.Conn())
		ls = append(ls, l)
	}
	for _, l := range ls {
		l.Return()
	}
	for _, c := range cs {
		c.setIdleErr(unsolicited{})
	}
	l := x.checkout(t)
	for _, c := range cs {
		if l.Conn() == c || len(c.closeReasons()) != 1 {
			t.Fatal("a bad connection survived")
		}
	}
	x.rec.with(func(r *recorder) {
		if len(r.unsolicited) != 3 {
			t.Errorf("ConnUnsolicited calls: %d, want 3", len(r.unsolicited))
		}
	})
	l.Return()
}

// The checkout timeout is not reset by a discard: it runs from the start of
// the checkout even when the replacement open takes long.
func TestDiscardDoesNotResetTimeout_AC46(t *testing.T) {
	c := pool.Config{MaxSize: 1, MaxWaiters: 5, CheckoutTimeout: 5 * time.Second}
	x := newPool(t, c, ready(H1), false)
	l := x.checkout(t)
	bad := l.Conn()
	l.Return()
	bad.setIdleErr(unsolicited{})
	x.op.setGated(true)
	ch := x.async(context.Background())
	eventually(t, "replacement open started", func() bool { return x.op.started.Load() >= 2 })
	x.clk.WaitForTimers(1, 2*time.Second)
	x.clk.Advance(4*time.Second + 999*time.Millisecond)
	noResult(t, ch)
	x.clk.Advance(time.Millisecond)
	r := recv(t, ch)
	if detailOf(r.err) != "pool_wait_timeout" {
		t.Fatalf("err %v (detail %q), want pool_wait_timeout at 5 s from the start", r.err, detailOf(r.err))
	}
	x.op.releaseAll()
}

// DiscardIdle removes an idle connection at once with reason=error; with a
// bytes error it is reported as unsolicited.
func TestDiscardIdle_AC46(t *testing.T) {
	x := newPool(t, cfg(4), ready(H1), false)
	l := x.checkout(t)
	c := l.Conn()
	l.Return()
	x.p.DiscardIdle(c, unsolicited{})
	if r := c.closeReasons(); len(r) != 1 || r[0] != "error" {
		t.Fatalf("close reasons: %v", r)
	}
	if s := x.p.Stats(); s.Idle != 0 {
		t.Fatalf("stats: %+v", s)
	}
	x.rec.with(func(r *recorder) {
		if len(r.unsolicited) != 1 {
			t.Errorf("ConnUnsolicited calls: %d", len(r.unsolicited))
		}
	})
	// A peer close.
	l = x.checkout(t)
	c2 := l.Conn()
	l.Return()
	x.p.DiscardIdle(c2, errors.New("synthetic close"))
	if r := c2.closeReasons(); len(r) != 1 || r[0] != "error" {
		t.Fatalf("close reasons: %v", r)
	}
	x.rec.with(func(r *recorder) {
		if len(r.unsolicited) != 1 {
			t.Errorf("a peer close was reported as unsolicited data")
		}
	})
	// DiscardIdle twice is a no-op the second time.
	x.p.DiscardIdle(c2, errors.New("again"))
	if len(c2.closeReasons()) != 1 {
		t.Fatal("DiscardIdle closed a connection twice")
	}
}

// DiscardIdle on a leased connection is a no-op.
func TestDiscardIdleLeasedNoop_AC46(t *testing.T) {
	x := newPool(t, cfg(2), ready(H1), false)
	l := x.checkout(t)
	c := l.Conn()
	x.p.DiscardIdle(c, unsolicited{})
	if c.isClosed() {
		t.Fatal("DiscardIdle closed a leased connection")
	}
	if s := x.p.Stats(); s.InUse != 1 {
		t.Fatalf("stats: %+v", s)
	}
	x.rec.with(func(r *recorder) {
		if len(r.unsolicited) != 0 {
			t.Error("a leased connection was reported as unsolicited")
		}
	})
	l.Return()
	if s := x.p.Stats(); s.Idle != 1 {
		t.Fatalf("stats after return: %+v", s)
	}
}

// A healthy idle connection passes the probe and is handed out untouched.
func TestHealthyIdleHandedOut_AC46(t *testing.T) {
	x, _, b := twoIdle(t, cfg(4))
	l := x.checkout(t)
	if l.Conn() != b || b.isClosed() {
		t.Fatal("a healthy connection was not handed out")
	}
	l.Return()
}
