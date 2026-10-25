// Package upstreamconn is the infrastructure side of upstream connections:
// the pool opener, the monitoring checker, SCRAM authentication through the
// scram adapter, request/reply exchange for corral's own small messages, and
// idle-connection probing (SPEC-002 FR-22, FR-24, FR-28, FR-37, FR-45, FR-62;
// ADR-0019).
//
// Compliance (FR-24, SPEC-001 FR-24): credentials live only in Config and the
// scram client, are never formatted, and no error returned from this package
// contains them or any text sent by the upstream. Errors carry constant text
// and wrap only transport errors, which are classified by errclass.
package upstreamconn

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/bson"
	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn/scram"
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// Credentials are corral's single upstream service identity. Mechanism "none"
// (or a nil pointer) means no authentication.
type Credentials struct {
	Mechanism          string
	Source             string
	Username, Password []byte
}

// Config configures the opener and the checker.
type Config struct {
	Version        string // corral version, sent as the driver version
	Credentials    *Credentials
	TLS            func(host string) (*tls.Config, error) // nil: plain TCP
	ConnectTimeout time.Duration
	Clock          clock.Clock
}

// ErrUnsolicited is returned for bytes that arrive on an idle connection
// (FR-62). Its detail is unexpected_response.
var ErrUnsolicited error = &unsolicitedError{}

type unsolicitedError struct{}

func (*unsolicitedError) Error() string {
	return "upstreamconn: unsolicited data on an idle connection"
}
func (*unsolicitedError) CorralErrorDetail() string { return "unexpected_response" }

// maxOwnReply bounds replies to corral's own exchanges (hello, SASL, ping).
const maxOwnReply = 1 << 20

var connSeq atomic.Uint64

// Conn is one upstream connection. It satisfies pool.Conn.
type Conn struct {
	id       uint64
	host     string
	nc       net.Conn
	corr     *wire.Correlator
	maxMsg   int32
	closeOne sync.Once
	authed   bool

	wmu     sync.Mutex
	watchMu sync.Mutex
	stop    chan struct{}
}

// ID returns the connection's unique ID.
func (c *Conn) ID() uint64 { return c.id }

// Host returns the host the connection goes to.
func (c *Conn) Host() string { return c.host }

// NetConn returns the underlying connection (a *tls.Conn when TLS is on).
func (c *Conn) NetConn() net.Conn { return c.nc }

// Correlator returns the connection's request-ID correlator.
func (c *Conn) Correlator() *wire.Correlator { return c.corr }

// MaxMessageSize is the upstream's maxMessageSizeBytes from its hello reply.
func (c *Conn) MaxMessageSize() int32 { return c.maxMsg }

// Close closes the socket and stops any idle watcher.
func (c *Conn) Close(reason pool.CloseReason) {
	c.StopIdleWatch()
	c.closeOne.Do(func() { _ = c.nc.Close() })
}

type closedError struct{ cause error }

func (e *closedError) Error() string { return "upstreamconn: connection closed by the peer" }
func (e *closedError) Unwrap() error { return e.cause }

// CheckIdle probes an idle connection without blocking and, on Linux, without
// consuming data (FR-62): unsolicited bytes give ErrUnsolicited, a close gives
// a network-class error. Off Linux it falls back to a zero-deadline read; the
// connection is discarded when it reports a fault, so consuming is harmless.
func (c *Conn) CheckIdle() error {
	if runtime.GOOS != "linux" {
		return c.readProbe()
	}
	st, err := upstreamnet.Peek(c.nc)
	switch {
	case err != nil:
		return &closedError{cause: err}
	case st == upstreamnet.PeekData:
		return ErrUnsolicited
	case st == upstreamnet.PeekClosed:
		return &closedError{cause: io.EOF}
	}
	return nil
}

func (c *Conn) readProbe() error {
	_ = c.nc.SetReadDeadline(time.Unix(1, 0))
	defer c.nc.SetReadDeadline(time.Time{})
	var b [1]byte
	n, err := c.nc.Read(b[:])
	if n > 0 {
		return ErrUnsolicited
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil
	}
	if err == nil {
		return nil
	}
	return &closedError{cause: err}
}

// StartIdleWatch watches the idle connection and calls onFault once, with
// ErrUnsolicited for bytes or a network-class error for a close. Nothing may
// read the connection while a watch runs; stop it first (see upstreamnet).
func (c *Conn) StartIdleWatch(onFault func(error)) {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	if c.stop != nil {
		return
	}
	stop := make(chan struct{})
	c.stop = stop
	ch := upstreamnet.WatchReadable(c.nc, stop)
	go func() {
		st, ok := <-ch
		if !ok {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		if st == upstreamnet.PeekData {
			onFault(ErrUnsolicited)
		} else {
			onFault(&closedError{cause: io.EOF})
		}
	}()
}

// StopIdleWatch stops the watcher; it is safe to call twice or without a
// running watch.
func (c *Conn) StopIdleWatch() {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	if c.stop != nil {
		close(c.stop)
		c.stop = nil
	}
}

// exchange sends body as an OP_MSG and returns the reply document. It
// validates the reply as it is read (FR-58) and honours ctx.
func (c *Conn) exchange(ctx context.Context, body []byte) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.nc.SetDeadline(dl)
	} else {
		_ = c.nc.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	id := c.corr.Next()
	if _, err := c.nc.Write(wire.BuildMsg(id, 0, 0, body)); err != nil {
		return nil, fmt.Errorf("upstreamconn: write failed: %w", err)
	}
	var hb [16]byte
	if _, err := io.ReadFull(c.nc, hb[:]); err != nil {
		return nil, fmt.Errorf("upstreamconn: read failed: %w", err)
	}
	h := wire.ParseHeader(hb[:])
	limit := c.maxMsg
	if limit > maxOwnReply {
		limit = maxOwnReply
	}
	if err := wire.CheckReplyHeader(h, limit); err != nil {
		return nil, err
	}
	if err := c.corr.Match(h.ResponseTo); err != nil {
		return nil, err
	}
	buf := make([]byte, int(h.Length)-16)
	if _, err := io.ReadFull(c.nc, buf); err != nil {
		return nil, fmt.Errorf("upstreamconn: read failed: %w", err)
	}
	s := wire.NewReplyScanner(h)
	if err := s.Write(buf); err != nil {
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	end := len(buf)
	if binary.LittleEndian.Uint32(buf)&wire.FlagChecksumPresent != 0 {
		end -= 4
	}
	if end < 9 || buf[4] != 0 {
		return nil, wire.ErrMalformed()
	}
	return buf[5:end], nil
}

// RoundTrip sends one command document and returns the reply document.
func RoundTrip(ctx context.Context, c *Conn, body []byte) ([]byte, error) {
	return c.exchange(ctx, body)
}

// serverError is an error reply from the upstream, reduced to its code and
// code name. Its text is constant (errclass.ServerError).
type serverError struct {
	code int32
	name string
}

func (e *serverError) Error() string          { return "upstreamconn: the upstream returned an error reply" }
func (e *serverError) ServerCode() int32      { return e.code }
func (e *serverError) ServerCodeName() string { return e.name }

// replyError returns nil for an ok:1 reply and a serverError otherwise.
func replyError(doc []byte) error {
	ok, found, _ := lookupNumber(doc, "ok")
	if found && ok == 1 {
		return nil
	}
	se := &serverError{}
	if n, f, _ := lookupNumber(doc, "code"); f {
		se.code = int32(n)
	}
	if e, f, err := bson.Lookup(doc, "codeName"); f && err == nil {
		if s, isStr := e.StringValue(); isStr && len(s) <= 64 {
			se.name = s
		}
	}
	return se
}

func lookupNumber(doc []byte, key string) (float64, bool, error) {
	e, found, err := bson.Lookup(doc, key)
	if err != nil || !found {
		return 0, false, err
	}
	n, ok := e.Number()
	return n, ok, nil
}

// connect dials, runs TLS and returns a Conn with no hello done yet. The
// deadline of ctx bounds both.
func connect(ctx context.Context, cfg Config, host string) (*Conn, error) {
	raw, err := upstreamnet.Dial(ctx, host)
	if err != nil {
		return nil, err
	}
	var nc net.Conn = raw
	if cfg.TLS != nil {
		tc, err := cfg.TLS(host)
		if err != nil {
			raw.Close()
			return nil, fmt.Errorf("upstreamconn: TLS configuration failed: %w", err)
		}
		t := tls.Client(raw, tc)
		if err := t.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("upstreamconn: TLS handshake failed: %w", err)
		}
		nc = t
	}
	return &Conn{id: connSeq.Add(1), host: host, nc: nc, corr: wire.NewCorrelator(), maxMsg: wire.DefaultMaxMessageSize}, nil
}

// hello runs the handshake with the given appName and returns the parsed reply.
func (c *Conn) hello(ctx context.Context, cfg Config, app string) (wire.HelloReply, error) {
	doc, err := c.exchange(ctx, wire.HelloBody(app, cfg.Version, runtime.GOOS))
	if err != nil {
		return wire.HelloReply{}, err
	}
	if err := replyError(doc); err != nil {
		return wire.HelloReply{}, err
	}
	r, err := wire.ParseHelloReply(doc)
	if err != nil {
		return wire.HelloReply{}, err
	}
	if r.MaxMessageSizeBytes > 0 {
		c.maxMsg = r.MaxMessageSizeBytes
	}
	return r, nil
}

type opener struct {
	cfg  Config
	auth *authenticator
}

// NewOpener returns the pool's opener: dial, TLS, hello with appName corral,
// then SCRAM-SHA-256 unless the mechanism is none, all within ConnectTimeout.
func NewOpener(cfg Config) pool.Opener[*Conn] {
	return &opener{cfg: cfg, auth: newAuthenticator(cfg)}
}

func (o *opener) Open(ctx context.Context, host string) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, o.cfg.ConnectTimeout)
	defer cancel()
	c, err := connect(ctx, o.cfg, host)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Conn, error) { c.Close(pool.CloseError); return nil, err }
	if _, err := c.hello(ctx, o.cfg, "corral"); err != nil {
		return fail(err)
	}
	if err := o.auth.authenticate(ctx, c); err != nil {
		return fail(err)
	}
	_ = c.nc.SetDeadline(time.Time{})
	return c, nil
}

// errNoCreds is returned when credentials were configured but unusable.
var errBadCredentials = errors.New("upstreamconn: the configured credentials cannot be used")

type authenticator struct {
	cl  *scram.Client
	src string
	err error
}

func newAuthenticator(cfg Config) *authenticator {
	cr := cfg.Credentials
	if cr == nil || cr.Mechanism == "" || cr.Mechanism == "none" {
		return &authenticator{}
	}
	cl, err := scram.NewClient(cr.Username, cr.Password)
	a := &authenticator{cl: cl, src: cr.Source}
	if err != nil {
		a.err = errBadCredentials
	}
	return a
}

func (a *authenticator) enabled() bool { return a.cl != nil || a.err != nil }

// authenticate runs SCRAM-SHA-256 on c. Authentication failures (codes 18 and
// 334, or a failed server signature) are *upstream.AuthFailed; everything else
// is transient and keeps its own class (FR-22).
func (a *authenticator) authenticate(ctx context.Context, c *Conn) error {
	if !a.enabled() {
		return nil
	}
	if a.err != nil {
		return a.err
	}
	conv := a.cl.NewConversation()
	first, err := conv.First()
	if err != nil {
		return mapScram(err)
	}
	doc, err := c.exchange(ctx, wire.SaslStartBody(a.src, first))
	if err != nil {
		return err
	}
	convDone := false
	for {
		r, err := parseSasl(doc)
		if err != nil {
			return err
		}
		var out []byte
		if !convDone {
			var done bool
			out, done, err = conv.Step(r.Payload)
			if err != nil {
				return mapScram(err)
			}
			convDone = done
		}
		if r.Done {
			if !convDone {
				return wire.ErrUnexpectedResponse()
			}
			c.authed = true
			return nil
		}
		doc, err = c.exchange(ctx, wire.SaslContinueBody(a.src, r.ConversationID, out))
		if err != nil {
			return err
		}
	}
}

func parseSasl(doc []byte) (wire.SaslReply, error) {
	if err := replyError(doc); err != nil {
		se := err.(*serverError)
		if se.code == 18 || se.code == 334 {
			return wire.SaslReply{}, &upstream.AuthFailed{Err: se}
		}
		return wire.SaslReply{}, se
	}
	r, err := wire.ParseSaslReply(doc)
	if err != nil {
		return wire.SaslReply{}, err
	}
	return r, nil
}

func mapScram(err error) error {
	switch {
	case errors.Is(err, scram.ErrServerSignature):
		return &upstream.AuthFailed{Err: scram.ErrServerSignature}
	case errors.Is(err, scram.ErrSASLprep):
		return errBadCredentials
	}
	return wire.ErrUnexpectedResponse()
}
