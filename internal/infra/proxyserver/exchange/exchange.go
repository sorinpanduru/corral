// Package exchange is the per-client-connection message loop of the proxy
// (SPEC-002 FR-4, FR-10 to FR-12, FR-26 to FR-30, FR-35, FR-38 to FR-41,
// FR-57 to FR-60; ADR-0017, ADR-0018): it reads client messages within a
// bounded buffer, validates them before forwarding, multiplexes them over
// leased upstream connections, and streams replies back in bounded chunks.
//
// Compliance (SPEC-001 FR-24, SPEC-002 FR-9, FR-38): message bytes are held
// only up to the request buffer limit before a lease and one reply chunk while
// streaming. Only the FR-9 fields reach observers, and errors carry no
// message bytes. A message that fails validation is never delivered complete
// to an upstream: the last byte is held back until every check has passed.
package exchange

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime/debug"
	"sync"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
)

// Chunk sizes (FR-58, FR-38): K, the reply and forward chunk, equals L.
const (
	ReplyChunk   = 65536
	ForwardChunk = 65536
)

// Upstream is the upstream connection as the engine uses it.
type Upstream interface {
	ID() uint64
	Host() string
	NetConn() net.Conn
	Correlator() *wire.Correlator
	MaxMessageSize() int32
}

// Lease is a checked-out upstream connection.
type Lease interface {
	Upstream() Upstream
	Return()
	Discard(reason pool.CloseReason)
	Aborted() <-chan struct{}
}

// Leaser checks out a lease.
type Leaser interface {
	Checkout(ctx context.Context) (Lease, error)
}

// UpstreamView gives handshake availability and the current maximum message
// size.
type UpstreamView interface {
	proxy.HandshakeView
	MaxMessageSize() int32
}

// Probe is the corraltest hook for bounds checks; nil in production.
type Probe interface {
	ReadBeforeLease(connID int32, n int)
	ReplyHeld(connID int32, n int)
}

// Deps are the engine's dependencies.
type Deps struct {
	Leaser       Leaser
	Upstream     UpstreamView
	Obs          proxy.Observer
	Spans        *spans.Factory
	Clock        clock.Clock
	IOTimeout    time.Duration
	IdleTimeout  time.Duration
	Shutdown     *proxy.Shutdown
	ReplyCodes   func(ctx context.Context, host string, src upstream.ReplySource, f upstream.ReplyCodeInfo) bool
	Probe        Probe
	PanicCommand string // corraltest only: a command that panics the handler
}

// ClientConn is one accepted client connection.
type ClientConn struct {
	Conn  net.Conn
	ID    int32
	Addr  netip.AddrPort
	IsTLS bool
}

// Engine serves client connections.
type Engine struct {
	d Deps

	mu      sync.Mutex
	idle    map[*session]struct{} // sessions waiting for a first byte
	closing bool                  // CloseIdle was called
}

// New returns an engine.
func New(d Deps) *Engine {
	if d.Obs == nil {
		d.Obs = proxy.Noop{}
	}
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	return &Engine{d: d, idle: map[*session]struct{}{}}
}

// session is the state of one client connection.
type session struct {
	e      *Engine
	ctx    context.Context
	cc     *ClientConn
	budget *proxy.IOBudget
	state  proxy.ConnState
	ids    *wire.ClientIDs
	meta   proxy.Metadata
	buf    []byte
	lease  Lease // the current lease, for the panic path
	kicked bool  // CloseIdle interrupted the idle wait (guarded by e.mu)
}

// CloseIdle is FR-47 step 3 for connections between exchanges: it interrupts
// every first-byte wait, and any later idle wait ends at once. An exchange in
// progress is not affected; it finishes and the connection then closes.
func (e *Engine) CloseIdle() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closing = true
	for s := range e.idle {
		s.kicked = true
		_ = s.cc.Conn.SetReadDeadline(time.Unix(1, 0))
	}
}

// beginIdle registers s as idle and sets the idle deadline; false means the
// engine is already closing idle connections.
func (s *session) beginIdle() bool {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return false
	}
	if e.d.IdleTimeout > 0 {
		_ = s.cc.Conn.SetReadDeadline(e.d.Clock.Now().Add(e.d.IdleTimeout))
	} else {
		_ = s.cc.Conn.SetReadDeadline(time.Time{})
	}
	e.idle[s] = struct{}{}
	return true
}

// endIdle unregisters s and reports whether CloseIdle interrupted the wait.
func (s *session) endIdle() bool {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	delete(s.e.idle, s)
	return s.kicked
}

// Serve runs the message loop until the connection ends and returns why.
func (e *Engine) Serve(ctx context.Context, cc *ClientConn) (reason proxy.CloseReason) {
	s := &session{e: e, ctx: ctx, cc: cc, budget: proxy.NewIOBudget(e.d.IOTimeout, e.d.Clock),
		ids: wire.NewClientIDs(), buf: make([]byte, proxy.RequestBufferLimit)}
	defer func() {
		if v := recover(); v != nil {
			if s.lease != nil {
				s.lease.Discard(pool.CloseError)
			}
			e.d.Obs.PanicRecovered(ctx, errclass.FromPanic(v, debug.Stack()))
			reason = proxy.CloseInternalError
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = cc.Conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	for {
		r, more := s.exchange()
		if more && e.d.Shutdown != nil && e.d.Shutdown.Phase() == proxy.PhaseLate {
			return proxy.CloseShutdown // FR-47: finish the exchange, then close
		}
		if !more {
			if ctx.Err() != nil && r == proxy.CloseClientClosed {
				return proxy.CloseShutdown
			}
			return r
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readBudget reads exactly len(dst) bytes within the I/O budget.
func (s *session) readBudget(dst []byte) error {
	_, err := s.readBudgetN(dst)
	return err
}

// readBudgetN is readBudget that also reports how many bytes arrived.
func (s *session) readBudgetN(dst []byte) (int, error) {
	_ = s.cc.Conn.SetReadDeadline(s.budget.BeginRead())
	n, err := io.ReadFull(s.cc.Conn, dst)
	s.budget.EndRead()
	return n, err
}

// exchange handles one message. It reports the close reason and whether the
// loop continues.
func (s *session) exchange() (proxy.CloseReason, bool) {
	d := &s.e.d
	// Between exchanges: read the first byte under the idle deadline.
	if !s.beginIdle() {
		return proxy.CloseShutdown, false
	}
	var first [1]byte
	_, err := io.ReadFull(s.cc.Conn, first[:])
	kicked := s.endIdle()
	if err != nil {
		switch {
		case s.ctx.Err() != nil || kicked:
			return proxy.CloseShutdown, false
		case isTimeout(err):
			return proxy.CloseIdleTimeout, false
		}
		return proxy.CloseClientClosed, false
	}
	start := d.Clock.Now()
	s.budget.Reset()
	x := &msg{s: s, start: start}
	copy(s.buf[:1], first[:])
	return x.run()
}

func (s *session) write(b []byte) error {
	_ = s.cc.Conn.SetWriteDeadline(s.e.d.Clock.Now().Add(s.e.d.IOTimeout))
	_, err := s.cc.Conn.Write(b)
	return err
}

func (s *session) rootInfo(req *wire.Request) *spans.RootInfo {
	return &spans.RootInfo{
		Command: req.Command, DB: req.DB, Collection: req.Collection, HasCollection: req.HasCollection,
		Client: s.cc.Addr, ConnID: s.cc.ID, RequestID: req.RequestID,
		SessionID: req.LSID, HasSession: req.HasLSID, CursorID: req.CursorID, HasCursor: req.HasCursorID,
		AppName: s.meta.AppName, DriverName: s.meta.DriverName, DriverVersion: s.meta.DriverVersion,
	}
}

func errorf(msg string, cause error) error { return fmt.Errorf("%s: %w", msg, cause) }
