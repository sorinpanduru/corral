package upstreamconn_test

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

func idleIndex(t *testing.T, s *mongosim.Server) int {
	t.Helper()
	var idx []int
	eventually(t, "an idle pool connection", func() bool { idx = s.IdleConns("corral"); return len(idx) > 0 })
	return idx[len(idx)-1]
}

// FR-62 (AC-46 in-process): CheckIdle is nil on a quiet connection.
func TestCheckIdleQuiet_AC46(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := mustOpen(t, baseCfg(nil), s.Addr())
	if err := c.CheckIdle(); err != nil {
		t.Fatalf("CheckIdle on a quiet connection: %v", err)
	}
}

// FR-62: pending bytes fail the probe with ErrUnsolicited and are not
// consumed; the probe keeps failing and the bytes are still readable.
func TestCheckIdleUnsolicitedNotConsumed_AC46(t *testing.T) {
	s := sim(t, mongosim.Options{})
	c := mustOpen(t, baseCfg(nil), s.Addr())
	idx := idleIndex(t, s)
	if err := s.SendRaw(idx, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the probe sees the bytes", func() bool { return c.CheckIdle() != nil })
	err := c.CheckIdle()
	if !errors.Is(err, upstreamconn.ErrUnsolicited) {
		t.Fatalf("CheckIdle = %v, want ErrUnsolicited", err)
	}
	var d interface{ CorralErrorDetail() string }
	if !errors.As(err, &d) || d.CorralErrorDetail() != "unexpected_response" {
		t.Fatalf("ErrUnsolicited has no unexpected_response detail")
	}
	if cl, dt := errclass.Classify(err); cl != errclass.ClassProtocol || dt != errclass.DetailUnexpectedResponse {
		t.Fatalf("Classify = (%s, %s)", cl, dt)
	}
	if runtime.GOOS != "linux" {
		// Off Linux the probe falls back to a zero-deadline read (ADR-0019):
		// bytes it returns are a fault and the connection is discarded anyway,
		// so "not consumed" is only promised on Linux.
		return
	}
	// Not consumed: all 10 bytes are still in the socket.
	buf := make([]byte, 10)
	_ = c.NetConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c.NetConn(), buf); err != nil || string(buf) != "0123456789" {
		t.Fatalf("bytes were consumed by the probe: %v %q", err, buf)
	}
}

// FR-62: a peer close fails the probe with the classified network error, not
// ErrUnsolicited.
func TestCheckIdlePeerClose_AC46(t *testing.T) {
	for name, reset := range map[string]bool{"FIN": false, "RST": true} {
		t.Run(name, func(t *testing.T) {
			s := sim(t, mongosim.Options{})
			c := mustOpen(t, baseCfg(nil), s.Addr())
			idx := idleIndex(t, s)
			if err := s.CloseConn(idx, reset); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the probe sees the close", func() bool { return c.CheckIdle() != nil })
			err := c.CheckIdle()
			if errors.Is(err, upstreamconn.ErrUnsolicited) {
				t.Fatal("a close was reported as unsolicited data")
			}
			if cl, _ := errclass.Classify(err); cl != errclass.ClassNetwork {
				t.Fatalf("class %q, want network", cl)
			}
		})
	}
}

// FR-62: the same probe works on a TLS connection: any readable TLS record on
// an idle connection is a fault, and nothing is consumed.
func TestCheckIdleTLS_AC46(t *testing.T) {
	s := sim(t, mongosim.Options{})
	f := newTLSFront(t, s.Addr())
	c := mustOpen(t, f.cfg(), f.Addr)
	if err := c.CheckIdle(); err != nil {
		t.Fatalf("quiet TLS connection: %v", err)
	}
	idx := idleIndex(t, s)
	if err := s.SendRaw(idx, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the probe sees the TLS record", func() bool { return c.CheckIdle() != nil })
	if err := c.CheckIdle(); !errors.Is(err, upstreamconn.ErrUnsolicited) {
		t.Fatalf("CheckIdle on TLS = %v, want ErrUnsolicited", err)
	}
	if runtime.GOOS != "linux" {
		return // off-Linux fallback may consume (see above)
	}
	// The TLS bytes were not consumed: a read through TLS returns them.
	buf := make([]byte, 10)
	_ = c.NetConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c.NetConn(), buf); err != nil || string(buf) != "0123456789" {
		t.Fatalf("TLS data consumed or damaged: %v %q", err, buf)
	}
}

func TestCheckIdleTLSClose_AC46(t *testing.T) {
	s := sim(t, mongosim.Options{})
	f := newTLSFront(t, s.Addr())
	c := mustOpen(t, f.cfg(), f.Addr)
	idx := idleIndex(t, s)
	if err := s.CloseConn(idx, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the probe sees the close", func() bool { return c.CheckIdle() != nil })
}

// FR-62: the idle watcher reports unsolicited bytes and closes, once.
func TestIdleWatcher_AC46(t *testing.T) {
	t.Run("unsolicited bytes", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		c := mustOpen(t, baseCfg(nil), s.Addr())
		got := make(chan error, 4)
		c.StartIdleWatch(func(err error) { got <- err })
		idx := idleIndex(t, s)
		s.SendRaw(idx, []byte("0123456789"))
		select {
		case err := <-got:
			if !errors.Is(err, upstreamconn.ErrUnsolicited) {
				t.Fatalf("fault = %v, want ErrUnsolicited", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the watcher did not report the bytes")
		}
		select {
		case <-got:
			t.Fatal("the watcher reported twice")
		case <-time.After(100 * time.Millisecond):
		}
	})
	t.Run("peer close", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		c := mustOpen(t, baseCfg(nil), s.Addr())
		got := make(chan error, 4)
		c.StartIdleWatch(func(err error) { got <- err })
		idx := idleIndex(t, s)
		s.CloseConn(idx, false)
		select {
		case err := <-got:
			if errors.Is(err, upstreamconn.ErrUnsolicited) {
				t.Fatal("a close was reported as unsolicited data")
			}
			if cl, _ := errclass.Classify(err); cl != errclass.ClassNetwork {
				t.Fatalf("class %q", cl)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the watcher did not report the close")
		}
	})
	t.Run("stopped watcher is silent", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		c := mustOpen(t, baseCfg(nil), s.Addr())
		got := make(chan error, 4)
		c.StartIdleWatch(func(err error) { got <- err })
		c.StopIdleWatch()
		idx := idleIndex(t, s)
		s.SendRaw(idx, []byte("x"))
		select {
		case err := <-got:
			t.Fatalf("a stopped watcher reported %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		// Stop twice is fine.
		c.StopIdleWatch()
	})
	t.Run("TLS", func(t *testing.T) {
		s := sim(t, mongosim.Options{})
		f := newTLSFront(t, s.Addr())
		c := mustOpen(t, f.cfg(), f.Addr)
		got := make(chan error, 4)
		c.StartIdleWatch(func(err error) { got <- err })
		idx := idleIndex(t, s)
		s.SendRaw(idx, []byte("0123456789"))
		select {
		case err := <-got:
			if !errors.Is(err, upstreamconn.ErrUnsolicited) {
				t.Fatalf("fault = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the watcher did not report data on a TLS connection")
		}
	})
}

type poolObs struct {
	pool.Noop
	mu     sync.Mutex
	uns    int
	closed []string
}

func (o *poolObs) ConnUnsolicited(ctx context.Context, host string) {
	o.mu.Lock()
	o.uns++
	o.mu.Unlock()
}
func (o *poolObs) ConnClosed(ctx context.Context, host string, r pool.CloseReason) {
	o.mu.Lock()
	o.closed = append(o.closed, string(r))
	o.mu.Unlock()
}
func (o *poolObs) snapshot() (int, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.uns, append([]string(nil), o.closed...)
}

type gate struct{ host string }

func (g gate) Admission() pool.Admission { return pool.Admission{Kind: pool.Ready, Target: g.host} }

// AC-46 (in-process): the watcher feeds pool.DiscardIdle. Unsolicited bytes
// on an idle pool connection remove it and report ConnUnsolicited; a peer
// close removes it with reason=error and no ConnUnsolicited.
func TestWatcherDiscardsThroughPool_AC46(t *testing.T) {
	setup := func(t *testing.T) (*mongosim.Server, *pool.Pool[*upstreamconn.Conn], *poolObs) {
		s := sim(t, mongosim.Options{})
		obs := &poolObs{}
		p := pool.New[*upstreamconn.Conn](pool.Config{MaxSize: 4, MaxWaiters: 10, CheckoutTimeout: 5 * time.Second},
			upstreamconn.NewOpener(baseCfg(nil)), gate{s.Addr()}, clock.System(), obs)
		t.Cleanup(func() { p.Close(context.Background()) })
		return s, p, obs
	}
	idle := func(t *testing.T, p *pool.Pool[*upstreamconn.Conn]) *upstreamconn.Conn {
		l, err := p.Checkout(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		c := l.Conn()
		l.Return()
		c.StartIdleWatch(func(err error) { p.DiscardIdle(c, err) })
		return c
	}

	t.Run("unsolicited", func(t *testing.T) {
		s, p, obs := setup(t)
		idle(t, p)
		if st := p.Stats(); st.Idle != 1 {
			t.Fatalf("stats: %+v", st)
		}
		s.SendRaw(idleIndex(t, s), []byte("0123456789"))
		eventually(t, "discarded", func() bool { return p.Stats().Idle == 0 })
		eventually(t, "reported", func() bool { u, _ := obs.snapshot(); return u == 1 })
		if _, closed := obs.snapshot(); len(closed) != 1 || closed[0] != "error" {
			t.Fatalf("closes: %v", closed)
		}
	})
	t.Run("peer close", func(t *testing.T) {
		s, p, obs := setup(t)
		idle(t, p)
		s.CloseConn(idleIndex(t, s), false)
		eventually(t, "discarded", func() bool { return p.Stats().Idle == 0 })
		eventually(t, "closed", func() bool { _, c := obs.snapshot(); return len(c) == 1 })
		if u, closed := obs.snapshot(); u != 0 || closed[0] != "error" {
			t.Fatalf("unsolicited=%d closes=%v", u, closed)
		}
	})
	t.Run("checkout probe without a watcher", func(t *testing.T) {
		s, p, obs := setup(t)
		l, err := p.Checkout(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		l.Return() // no watcher: only the checkout probe can notice
		s.SendRaw(idleIndex(t, s), []byte("0123456789"))
		time.Sleep(50 * time.Millisecond)
		l2, err := p.Checkout(context.Background())
		if err != nil {
			t.Fatalf("checkout: %v", err)
		}
		if n := s.ConnAttemptsBy("corral"); n != 2 {
			t.Fatalf("connections opened = %d, want 2 (the bad one replaced)", n)
		}
		if u, _ := obs.snapshot(); u != 1 {
			t.Fatalf("ConnUnsolicited calls = %d", u)
		}
		l2.Return()
	})
}
