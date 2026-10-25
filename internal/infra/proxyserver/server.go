// Package proxyserver is the wire listener and per-connection server of the
// proxy (SPEC-002 FR-1 to FR-4, FR-42 to FR-44, FR-47, FR-63; ADR-0018,
// ADR-0021). It accepts plain TCP, applies the connection limit, and only then
// wraps the connection in TLS, so an over-limit client receives no TLS bytes.
//
// Compliance (SPEC-001 FR-24, GDPR data minimisation): bind errors are config
// errors that name the setting and never the address. TLS failures reach the
// observer as errors only; no certificate data or client identity is read or
// logged here. Client addresses reach the observer only as the connection
// info the domain defines.
package proxyserver

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

// Hold-point names passed to Options.Hold (ADR-0022). The hook blocks the
// shutdown at that point until it returns.
const (
	HoldShutdownDelay  = "shutdown_delay"
	HoldListenerClosed = "shutdown_listener_closed"
	HoldStep3          = "shutdown_step3"
	HoldDrain          = "shutdown_drain"
	HoldDrainExpired   = "shutdown_drain_expired"
)

// SettingListen is the setting a bind failure is reported against.
const SettingListen = "proxy.listen"

// Options configure a Server.
type Options struct {
	Addr      string
	MaxConns  int
	TLS       *tls.Config
	IOTimeout time.Duration
	Engine    *exchange.Engine
	Obs       proxy.Observer
	Shutdown  *proxy.Shutdown
	IDs       *proxy.ConnIDs
	// Hold is the corraltest hook; nil in production.
	Hold func(ctx context.Context, point string)
	// Clock times the accept retry delay; nil means the system clock.
	Clock clock.Clock
	// WrapListener, when set, wraps the bound listener (a test seam for
	// injecting accept errors).
	WrapListener func(net.Listener) net.Listener
}

// Server is the wire listener.
type Server struct {
	o  Options
	ln net.Listener

	mu       sync.Mutex
	open     int
	conns    map[net.Conn]struct{}
	lnClosed bool

	wg     sync.WaitGroup
	base   context.Context
	cancel context.CancelFunc
	forced atomic.Bool // ForceDrainEnd was called (it may precede Shutdown)
}

// New returns a server.
func New(o Options) *Server {
	if o.Obs == nil {
		o.Obs = proxy.Noop{}
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	s := &Server{o: o, conns: map[net.Conn]struct{}{}}
	s.base, s.cancel = context.WithCancel(context.Background())
	return s
}

// Bind opens the listener. A failure is a config error for proxy.listen that
// wraps the net error as its cause and carries no address.
func (s *Server) Bind() error {
	ln, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		return config.NewError(SettingListen, err)
	}
	if s.o.WrapListener != nil {
		ln = s.o.WrapListener(ln)
	}
	s.ln = ln
	return nil
}

// Addr returns the listening address, or nil before Bind.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) hold(ctx context.Context, point string) {
	if s.o.Hold != nil {
		s.o.Hold(ctx, point)
	}
}

// Serve runs the accept loop until ctx ends or the listener is closed by
// Shutdown. When ctx ends, it closes the listener and every connection.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		s.closeListener()
		s.forceClose()
	})
	defer stop()
	var delay time.Duration
	for {
		c, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.lnClosed
			s.mu.Unlock()
			if closed || ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if retryableAccept(err) {
				// A transient failure (out of descriptors, an aborted
				// connection) must not end the listener: back off 5 ms
				// doubling to 1 s, like net/http.
				if delay == 0 {
					delay = acceptBackoffMin
				} else if delay *= 2; delay > acceptBackoffMax {
					delay = acceptBackoffMax
				}
				// No jitter, no record, no metric (FR-66).
				wake := make(chan struct{})
				t := s.o.Clock.AfterFunc(delay, func() { close(wake) })
				select {
				case <-wake:
				case <-ctx.Done():
					t.Stop()
					return nil
				}
				continue
			}
			return err
		}
		delay = 0
		s.admit(c)
	}
}

// Accept retry bounds (net/http uses the same).
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// retryableAccept reports whether an Accept error is transient.
func retryableAccept(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) { //nolint:staticcheck // Temporary is how net/http classifies accept errors
		return true
	}
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED, syscall.EINTR} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func (s *Server) closeListener() {
	s.mu.Lock()
	if s.lnClosed {
		s.mu.Unlock()
		return
	}
	s.lnClosed = true
	s.mu.Unlock()
	_ = s.ln.Close()
}

// forceClose ends every connection: its context is cancelled and its socket
// closed, so a handler blocked on either side returns.
func (s *Server) forceClose() {
	s.cancel()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
}

// admit applies the connection limit (before any TLS bytes) and starts the
// connection goroutine.
func (s *Server) admit(c net.Conn) {
	ctx := s.base
	s.mu.Lock()
	var id int32
	ok := s.open < s.o.MaxConns
	if ok {
		id, ok = s.o.IDs.Acquire()
	}
	if !ok {
		s.o.Obs.ConnRejected(ctx, proxy.RejectLimitExceeded)
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.open++
	s.conns[c] = struct{}{}
	info := proxy.ConnInfo{ID: id, TLS: s.o.TLS != nil}
	var ap netip.AddrPort
	if a, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		ap = a
		info.ClientAddr, info.ClientPort = a.Addr().String(), int(a.Port())
	}
	s.o.Obs.ConnOpened(ctx, info)
	s.wg.Add(1)
	s.mu.Unlock()
	go s.handle(ctx, c, id, ap)
}

// finish records the close reason and frees the slot atomically with respect to
// admission, so the gauge never exceeds the limit.
func (s *Server) finish(ctx context.Context, c net.Conn, id int32, r proxy.CloseReason) {
	_ = c.Close()
	s.mu.Lock()
	s.o.Obs.ConnClosed(ctx, id, r)
	s.o.IDs.Release(id)
	s.open--
	delete(s.conns, c)
	s.mu.Unlock()
	s.wg.Done()
}

func (s *Server) handle(ctx context.Context, raw net.Conn, id int32, ap netip.AddrPort) {
	reason := proxy.CloseInternalError
	defer func() {
		// The engine recovers its own panics; this guards the rest. Only the
		// classified error reaches the observer, never the panic value.
		if v := recover(); v != nil {
			s.o.Obs.PanicRecovered(ctx, errclass.FromPanic(v, debug.Stack()))
			reason = proxy.CloseInternalError
		}
		s.finish(ctx, raw, id, reason)
	}()
	conn := raw
	if s.o.TLS != nil {
		tc := tls.Server(raw, s.o.TLS)
		_ = raw.SetDeadline(time.Now().Add(s.o.IOTimeout))
		if err := tc.Handshake(); err != nil {
			s.o.Obs.TLSFailed(ctx, errclass.TLSHandshake(err))
			reason = proxy.CloseTLSFailure
			return
		}
		_ = raw.SetDeadline(time.Time{})
		conn = tc
	}
	reason = s.o.Engine.Serve(ctx, &exchange.ClientConn{Conn: conn, ID: id, Addr: ap, IsTLS: s.o.TLS != nil})
}

// ForceDrainEnd is the second signal (FR-63): it skips the rest of the
// listener-close delay and ends the drain at once. It is idempotent.
func (s *Server) ForceDrainEnd() {
	s.forced.Store(true)
	s.o.Shutdown.Force()
}

// Shutdown drives FR-47 steps 1 to 4 and returns when they are done.
func (s *Server) Shutdown(ctx context.Context) error { return s.shutdown(ctx, false) }

// ShutdownNow is Shutdown without step 1: the listener-close delay is not
// applied (FR-66, after a permanent accept error).
func (s *Server) ShutdownNow(ctx context.Context) error { return s.shutdown(ctx, true) }

func (s *Server) shutdown(ctx context.Context, noDelay bool) error {
	sd := s.o.Shutdown
	if noDelay {
		sd.BeginNow()
	} else {
		// Step 1: keep serving for the listener-close delay.
		sd.Begin()
	}
	if s.forced.Load() {
		sd.Force()
	}
	if !noDelay {
		s.hold(ctx, HoldShutdownDelay)
		select {
		case <-sd.DelayExpired():
		case <-ctx.Done():
		}
	}
	// Step 2: close the listener.
	s.closeListener()
	sd.ListenerClosed()
	s.hold(ctx, HoldListenerClosed)
	// Step 3: from here a first byte read gets 91; idle connections close at
	// once; exchanges in progress finish.
	sd.Step3()
	s.hold(ctx, HoldStep3)
	s.o.Engine.CloseIdle()
	// Step 4: the drain. A held drain stays open as if an exchange were in
	// progress.
	// The hold is cancelled when the drain ends (timeout or forced end).
	holdCtx, endHold := context.WithCancel(ctx)
	defer endHold()
	go func() {
		select {
		case <-sd.DrainExpired():
			endHold()
		case <-holdCtx.Done():
		}
	}()
	held := make(chan struct{})
	go func() { defer close(held); s.hold(holdCtx, HoldDrain) }()
	done := make(chan struct{})
	go func() { s.wg.Wait(); <-held; close(done) }()
	select {
	case <-done:
		return nil
	case <-sd.DrainExpired():
	case <-ctx.Done():
	}
	s.hold(ctx, HoldDrainExpired)
	s.forceClose()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	return nil
}
