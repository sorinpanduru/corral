package proxyserver_test

// In-process tests of the wire listener and per-connection server (T20): the
// real server, the real exchange engine, the real pool and connector, and the
// mongosim twin. Observability goes through the real proxyobs adapters into a
// logcheck handler and a private metrics registry, so accounting is checked on
// the real metrics.
//
// Interface assumptions (stated in the report): proxyserver.Options as in the
// plan; the Hold callback receives the point names HoldShutdownDelay,
// HoldListenerClosed, HoldStep3, HoldDrain, HoldDrainExpired and blocks the
// shutdown until it returns; Server.Shutdown drives FR-47 steps 1 to 4 and
// returns when they are done.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver"
	"github.com/sorinpanduru/corral/internal/infra/proxyserver/exchange"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/proxyobs"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/spans"
	"github.com/sorinpanduru/corral/internal/infra/upstreamconn"
	"github.com/sorinpanduru/corral/internal/testkit/fakeclock"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
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

type gate struct{ target string }

func (g gate) Admission() pool.Admission { return pool.Admission{Kind: pool.Ready, Target: g.target} }

type view struct{}

func (view) HandshakeAdmission() (upstream.Params, string) {
	return upstream.Params{MaxBsonObjectSize: 16777216, MaxMessageSizeBytes: 48000000, MaxWriteBatchSize: 100000, LogicalSessionTimeoutMinutes: 30, HasLSTM: true, MaxWire: 21}, ""
}
func (view) MaxMessageSize() int32 { return 48000000 }

type poolRec struct {
	pool.Noop
	mu     sync.Mutex
	closed []string
}

func (o *poolRec) ConnClosed(ctx context.Context, h string, r pool.CloseReason) {
	o.mu.Lock()
	o.closed = append(o.closed, string(r))
	o.mu.Unlock()
}
func (o *poolRec) reasons() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.closed...)
}

// recObs wraps the real proxyobs adapter and records what the server reports.
type recObs struct {
	proxy.Observer
	mu       sync.Mutex
	opened   int
	rejected int
	closed   []proxy.CloseReason
	tlsErrs  []error
	panics   []error
}

func (o *recObs) ConnOpened(ctx context.Context, i proxy.ConnInfo) {
	o.mu.Lock()
	o.opened++
	o.mu.Unlock()
	o.Observer.ConnOpened(ctx, i)
}
func (o *recObs) ConnClosed(ctx context.Context, id int32, r proxy.CloseReason) {
	o.mu.Lock()
	o.closed = append(o.closed, r)
	o.mu.Unlock()
	o.Observer.ConnClosed(ctx, id, r)
}
func (o *recObs) ConnRejected(ctx context.Context, r proxy.RejectReason) {
	o.mu.Lock()
	o.rejected++
	o.mu.Unlock()
	o.Observer.ConnRejected(ctx, r)
}
func (o *recObs) TLSFailed(ctx context.Context, err error) {
	o.mu.Lock()
	o.tlsErrs = append(o.tlsErrs, err)
	o.mu.Unlock()
	o.Observer.TLSFailed(ctx, err)
}
func (o *recObs) PanicRecovered(ctx context.Context, err error) {
	o.mu.Lock()
	o.panics = append(o.panics, err)
	o.mu.Unlock()
	o.Observer.PanicRecovered(ctx, err)
}
func (o *recObs) reasons() []proxy.CloseReason {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]proxy.CloseReason(nil), o.closed...)
}
func (o *recObs) counts() (opened, rejected int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened, o.rejected
}

// ---- the rig ----

type opts struct {
	maxConns    int
	ioTimeout   time.Duration
	idleTimeout time.Duration
	drain       time.Duration
	delay       time.Duration
	tlsCfg      *tls.Config
	ids         *proxy.ConnIDs
	panicCmd    string
	hold        func(ctx context.Context, point string)
	poolMax     int
}

type rig struct {
	t      *testing.T
	twin   *mongosim.Server
	p      *pool.Pool[*upstreamconn.Conn]
	srv    *proxyserver.Server
	obs    *recObs
	pobs   *poolRec
	sd     *proxy.Shutdown
	lh     *logcheck.Handler
	reg    *prometheus.Registry
	cancel context.CancelFunc
	served chan error
}

func newRig(t *testing.T, o opts) *rig {
	t.Helper()
	if o.maxConns == 0 {
		o.maxConns = 100
	}
	if o.ioTimeout == 0 {
		o.ioTimeout = 5 * time.Second
	}
	if o.drain == 0 {
		o.drain = 20 * time.Second
	}
	if o.poolMax == 0 {
		o.poolMax = 4
	}
	if o.ids == nil {
		o.ids = proxy.NewConnIDs()
	}
	twin := mongosim.Start(t, mongosim.Options{})
	lh := logcheck.New(t)
	reg := metrics.NewRegistry()
	cat, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	mf, err := metrics.NewFactory(reg, cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RegisterAll(mf); err != nil {
		t.Fatal(err)
	}
	ad, err := proxyobs.New(logging.NewFactory(lh), mf, fakeclock.New(time.Now()).Now)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, twin: twin, lh: lh, reg: reg, pobs: &poolRec{}, obs: &recObs{Observer: ad.Proxy()}, served: make(chan error, 1)}
	r.p = pool.New[*upstreamconn.Conn](pool.Config{MaxSize: o.poolMax, MaxWaiters: 1000, CheckoutTimeout: 5 * time.Second},
		upstreamconn.NewOpener(upstreamconn.Config{Version: "test", ConnectTimeout: 2 * time.Second, Clock: clock.System()}), gate{twin.Addr()}, clock.System(), r.pobs)
	r.sd = proxy.NewShutdown(o.delay, o.drain, clock.System())
	eng := exchange.New(exchange.Deps{Leaser: leaser{r.p}, Upstream: view{}, Obs: r.obs, Spans: spans.New(noop.NewTracerProvider().Tracer("x"), false),
		Clock: clock.System(), IOTimeout: o.ioTimeout, IdleTimeout: o.idleTimeout, Shutdown: r.sd,
		ReplyCodes: func(context.Context, string, upstream.ReplySource, upstream.ReplyCodeInfo) bool { return false }, PanicCommand: o.panicCmd})
	r.srv = proxyserver.New(proxyserver.Options{Addr: "127.0.0.1:0", MaxConns: o.maxConns, TLS: o.tlsCfg, IOTimeout: o.ioTimeout,
		Engine: eng, Obs: r.obs, Shutdown: r.sd, IDs: o.ids, Hold: o.hold})
	if err := r.srv.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var ctx context.Context
	ctx, r.cancel = context.WithCancel(context.Background())
	go func() { r.served <- r.srv.Serve(ctx) }()
	t.Cleanup(func() {
		r.cancel()
		r.p.Close(context.Background())
	})
	return r
}

func (r *rig) addr() string        { return r.srv.Addr().String() }
func (r *rig) dial() *rawwire.Conn { return rawwire.Dial(r.t, r.addr()) }

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

func hello(id int32) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: bson.D{{Key: "hello", Value: 1}, {Key: "$db", Value: "admin"}}})
}

func ping(id int32) []byte {
	return rawwire.OpMsg(rawwire.OpMsgOpts{RequestID: id, Body: bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}})
}

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

func connID(m *rawwire.Message) int64 { return m.Docs[0].Lookup("connectionId").AsInt64() }

// ---- metrics ----

func (r *rig) val(name string, labels ...string) float64 {
	r.t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	fams, err := r.reg.Gather()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matches(m, want) {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				if m.GetGauge() != nil {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return 0
}

func (r *rig) sum(name string) float64 {
	fams, _ := r.reg.Gather()
	t := 0.0
	for _, f := range fams {
		if f.GetName() == name {
			for _, m := range f.GetMetric() {
				if m.GetCounter() != nil {
					t += m.GetCounter().GetValue()
				}
			}
		}
	}
	return t
}

func matches(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// ---- TLS material ----

type pki struct {
	caPEM   []byte
	caCert  *x509.Certificate
	caKey   *ecdsa.PrivateKey
	srvCert tls.Certificate
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(der)
	p := &pki{caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caCert: ca, caKey: caKey}
	p.srvCert = p.issue(t, 2, "SYNTHETIC-SERVER-CN-MARKER", x509.ExtKeyUsageServerAuth)
	return p
}

func (p *pki) issue(t *testing.T, serial int64, cn string, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &k.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

func (p *pki) serverConfig(mtls bool) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{p.srvCert}}
	if mtls {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(p.caPEM)
		c.ClientCAs, c.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return c
}

func (p *pki) clientConfig(cert *tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	c := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "localhost"}
	if cert != nil {
		c.Certificates = []tls.Certificate{*cert}
	}
	return c
}

var _ = netip.Addr{}
