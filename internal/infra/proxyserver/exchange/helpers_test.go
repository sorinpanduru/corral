package exchange_test

// Integration-level tests of the exchange engine (T19): real loopback client
// connections (rawwire), the real pool and upstream connector, and the mongosim
// twin as the upstream. The ReplyCodes hook, the observer and the probe are
// recorders.
//
// Interface assumptions (stated in the report):
//   - exchange.Lease/Leaser are satisfied by the small adapter below around
//     pool.Pool[*upstreamconn.Conn] (the app layer owns the real one, T21).
//   - ReplyCodes is called as ReplyCodes(ctx, host, src, info) with info built
//     from the reply fields: HasTop = a top-level code is present, OKZero = ok
//     is 0, Top/TopName, HasWCE/WCE.
//   - A forwarded reply's lease is returned before ReplyCodes is called.
//   - Serve returns the proxy.CloseReason of the client connection.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/internal/testkit/rawwire"
	"github.com/sorinpanduru/corral/twins/mongosim"
)

// ---- pool adapter ----

type lease struct {
	l *pool.Lease[*upstreamconn.Conn]
}

func (l *lease) Upstream() exchange.Upstream { return l.l.Conn() }
func (l *lease) Return()                     { l.l.Return() }
func (l *lease) Discard(r pool.CloseReason)  { l.l.Discard(r) }
func (l *lease) Aborted() <-chan struct{}    { return l.l.Aborted() }

type leaser struct {
	p *pool.Pool[*upstreamconn.Conn]
}

func (l leaser) Checkout(ctx context.Context) (exchange.Lease, error) {
	ls, err := l.p.Checkout(ctx)
	if err != nil {
		return nil, err
	}
	return &lease{ls}, nil
}

type gate struct {
	mu sync.Mutex
	a  pool.Admission
}

func (g *gate) Admission() pool.Admission { g.mu.Lock(); defer g.mu.Unlock(); return g.a }
func (g *gate) set(a pool.Admission)      { g.mu.Lock(); g.a = a; g.mu.Unlock() }

// ---- recorders ----

type poolObs struct {
	pool.Noop
	mu     sync.Mutex
	closed []string
}

func (o *poolObs) ConnClosed(ctx context.Context, host string, r pool.CloseReason) {
	o.mu.Lock()
	o.closed = append(o.closed, string(r))
	o.mu.Unlock()
}
func (o *poolObs) reasons() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.closed...)
}

type result struct {
	Command string
	Outcome proxy.Outcome
	Err     error
	Dur     time.Duration
	Dropped bool
}

type proxyObs struct {
	proxy.Noop
	mu        sync.Mutex
	results   []result
	protoErrs []error
	timeouts  int
	panics    []error
	received  []string
	blocked   []string
	authRej   int
	hs        []proxy.HandshakeInfo
}

func (o *proxyObs) RequestReceived(ctx context.Context, c string) {
	o.mu.Lock()
	o.received = append(o.received, c)
	o.mu.Unlock()
}
func (o *proxyObs) RequestCompleted(ctx context.Context, r proxy.RequestResult) {
	o.mu.Lock()
	o.results = append(o.results, result{r.Command, r.Outcome, r.Err, r.Duration, r.Dropped})
	o.mu.Unlock()
}
func (o *proxyObs) ClientProtocolError(ctx context.Context, id int32, err error) {
	o.mu.Lock()
	o.protoErrs = append(o.protoErrs, err)
	o.mu.Unlock()
}
func (o *proxyObs) ClientIOTimeout(ctx context.Context, id int32) {
	o.mu.Lock()
	o.timeouts++
	o.mu.Unlock()
}
func (o *proxyObs) PanicRecovered(ctx context.Context, err error) {
	o.mu.Lock()
	o.panics = append(o.panics, err)
	o.mu.Unlock()
}
func (o *proxyObs) CommandBlocked(ctx context.Context, id int32, c string) {
	o.mu.Lock()
	o.blocked = append(o.blocked, c)
	o.mu.Unlock()
}
func (o *proxyObs) ClientAuthRejected(ctx context.Context, id int32, r proxy.Refusal) {
	o.mu.Lock()
	o.authRej++
	o.mu.Unlock()
}
func (o *proxyObs) HandshakeAnswered(ctx context.Context, i proxy.HandshakeInfo) {
	o.mu.Lock()
	o.hs = append(o.hs, i)
	o.mu.Unlock()
}
func (o *proxyObs) snapshot() []result {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]result(nil), o.results...)
}

type probe struct {
	mu          sync.Mutex
	maxPreLease int
	maxReply    int
}

func (p *probe) ReadBeforeLease(id int32, n int) {
	p.mu.Lock()
	if n > p.maxPreLease {
		p.maxPreLease = n
	}
	p.mu.Unlock()
}
func (p *probe) ReplyHeld(id int32, n int) {
	p.mu.Lock()
	if n > p.maxReply {
		p.maxReply = n
	}
	p.mu.Unlock()
}
func (p *probe) get() (int, int) { p.mu.Lock(); defer p.mu.Unlock(); return p.maxPreLease, p.maxReply }

type rcall struct {
	Host    string
	Src     upstream.ReplySource
	Info    upstream.ReplyCodeInfo
	InUseAt int
}

type codesRec struct {
	mu    sync.Mutex
	calls []rcall
	rule  func(src upstream.ReplySource, f upstream.ReplyCodeInfo) bool
	p     *pool.Pool[*upstreamconn.Conn]
}

func (c *codesRec) hook(ctx context.Context, host string, src upstream.ReplySource, f upstream.ReplyCodeInfo) bool {
	inuse := -1
	if c.p != nil {
		inuse = c.p.Stats().InUse
	}
	marked := false
	if c.rule != nil {
		marked = c.rule(src, f)
	}
	c.mu.Lock()
	c.calls = append(c.calls, rcall{host, src, f, inuse})
	c.mu.Unlock()
	return marked
}
func (c *codesRec) all() []rcall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rcall(nil), c.calls...)
}

// replicaRule is the FR-35 rule for replica-set mode (top-level code in ok:0
// or writeConcernError.code; barrier: top-level only); mongosRule the
// single-mongos rule (top-level 91 or 11600 in ok:0 only).
var fr35 = map[int32]bool{10107: true, 13435: true, 13436: true, 11602: true, 11600: true, 189: true, 91: true}

func replicaRule(src upstream.ReplySource, f upstream.ReplyCodeInfo) bool {
	if f.HasTop && f.OKZero && fr35[f.Top] {
		return true
	}
	return src == upstream.Forwarded && f.HasWCE && fr35[f.WCE]
}
func mongosRule(src upstream.ReplySource, f upstream.ReplyCodeInfo) bool {
	return f.HasTop && f.OKZero && (f.Top == 91 || f.Top == 11600)
}

// ---- the rig ----

type opts struct {
	poolMax       int
	maxWaiters    int
	checkout      time.Duration
	ioTimeout     time.Duration
	idleTimeout   time.Duration
	connectTO     time.Duration
	rule          func(upstream.ReplySource, upstream.ReplyCodeInfo) bool
	panicCommand  string
	tracer        *spansRec
	twinOpts      mongosim.Options
	shutdownDelay time.Duration
}

type rig struct {
	t      *testing.T
	twin   *mongosim.Server
	p      *pool.Pool[*upstreamconn.Conn]
	gate   *gate
	eng    *exchange.Engine
	ln     net.Listener
	pobs   *poolObs
	obs    *proxyObs
	probe  *probe
	codes  *codesRec
	sd     *proxy.Shutdown
	ctx    context.Context
	cancel context.CancelFunc

	nextID  atomic.Int32
	mu      sync.Mutex
	reasons []proxy.CloseReason
	served  chan proxy.CloseReason
}

type view struct{}

func (view) HandshakeAdmission() (upstream.Params, string) {
	return upstream.Params{MaxBsonObjectSize: 16777216, MaxMessageSizeBytes: 48000000, MaxWriteBatchSize: 100000, LogicalSessionTimeoutMinutes: 30, HasLSTM: true, MinWire: 0, MaxWire: 21}, ""
}
func (view) MaxMessageSize() int32 { return 48000000 }

func newRig(t *testing.T, o opts) *rig {
	t.Helper()
	if o.poolMax == 0 {
		o.poolMax = 4
	}
	if o.maxWaiters == 0 {
		o.maxWaiters = 1000
	}
	if o.checkout == 0 {
		o.checkout = 5 * time.Second
	}
	if o.ioTimeout == 0 {
		o.ioTimeout = 5 * time.Second
	}
	if o.connectTO == 0 {
		o.connectTO = 2 * time.Second
	}
	twin := mongosim.Start(t, o.twinOpts)
	r := &rig{t: t, twin: twin, gate: &gate{a: pool.Admission{Kind: pool.Ready, Target: twin.Addr()}},
		pobs: &poolObs{}, obs: &proxyObs{}, probe: &probe{}, served: make(chan proxy.CloseReason, 256)}
	r.codes = &codesRec{rule: o.rule}
	if r.codes.rule == nil {
		r.codes.rule = replicaRule
	}
	r.p = pool.New[*upstreamconn.Conn](pool.Config{MaxSize: o.poolMax, MaxWaiters: o.maxWaiters, CheckoutTimeout: o.checkout},
		upstreamconn.NewOpener(upstreamconn.Config{Version: "test", ConnectTimeout: o.connectTO, Clock: clock.System()}), r.gate, clock.System(), r.pobs)
	r.codes.p = r.p
	r.sd = proxy.NewShutdown(o.shutdownDelay, time.Hour, clock.System())
	tr := spans.New(noop.NewTracerProvider().Tracer("x"), false)
	if o.tracer != nil {
		tr = o.tracer.factory()
	}
	r.eng = exchange.New(exchange.Deps{Leaser: leaser{r.p}, Upstream: view{}, Obs: r.obs, Spans: tr, Clock: clock.System(),
		IOTimeout: o.ioTimeout, IdleTimeout: o.idleTimeout, Shutdown: r.sd, ReplyCodes: r.codes.hook, Probe: r.probe, PanicCommand: o.panicCommand})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.ln = ln
	r.ctx, r.cancel = context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			id := r.nextID.Add(1)
			go func() {
				reason := r.eng.Serve(r.ctx, &exchange.ClientConn{Conn: c, ID: id, Addr: netip.MustParseAddrPort("127.0.0.1:1")})
				c.Close()
				r.mu.Lock()
				r.reasons = append(r.reasons, reason)
				r.mu.Unlock()
				r.served <- reason
			}()
		}
	}()
	t.Cleanup(func() {
		r.cancel()
		ln.Close()
		r.p.Close(context.Background())
	})
	return r
}

func (r *rig) dial() *rawwire.Conn { return rawwire.Dial(r.t, r.ln.Addr().String()) }

func (r *rig) waitServed() proxy.CloseReason {
	r.t.Helper()
	select {
	case reason := <-r.served:
		return reason
	case <-time.After(10 * time.Second):
		r.t.Fatal("Serve did not return")
		return ""
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *rig) waitResults(n int) []result {
	r.t.Helper()
	var res []result
	eventually(r.t, "request results", func() bool { res = r.obs.snapshot(); return len(res) >= n })
	return res
}

// roundTrip sends raw and reads one reply.
func roundTrip(t *testing.T, c *rawwire.Conn, raw []byte) *rawwire.Message {
	t.Helper()
	if err := c.Send(raw); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadMessage(10 * time.Second)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return m
}

func ping(reqID int32) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: reqID, Body: bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}})
}

func asErr[T any](err error, target *T) bool { return errors.As(err, target) }

func classOf(err error) string {
	if err == nil {
		return ""
	}
	c, _ := errclass.Classify(err)
	return string(c)
}

func (o *proxyObs) nTimeouts() int { o.mu.Lock(); defer o.mu.Unlock(); return o.timeouts }

func mongosimMatch(marker string) mongosim.Match { return mongosim.Match{Marker: marker} }
func holdAction() mongosim.Action                { return mongosim.Hold() }

func queryCmd(cmd string) []byte {
	return rawwire.OpQuery(rawwire.OpQueryOpts{RequestID: 7, FullCollection: "admin.$cmd", NumberToReturn: -1, Query: bson.D{{Key: cmd, Value: 1}}})
}
