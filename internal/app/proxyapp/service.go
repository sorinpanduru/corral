// Package proxyapp wires the proxy: it builds the upstream monitor, the pool,
// the connector, the exchange engine and the wire listener from settings.Proxy
// and connects their ports (SPEC-002 FR-18, FR-35, FR-37, FR-46, FR-47, FR-62,
// FR-63; ADR-0015, ADR-0019, ADR-0021).
//
// Compliance (SPEC-001 FR-24, SPEC-002 FR-24, GDPR data minimisation): the
// upstream password and TLS key are read from settings.Secret only where the
// connector and the TLS configuration need them, and are never logged. The
// only record written here is `wire listener bound`, with no attributes.
package proxyapp

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/domain/clock"
	"github.com/sorinpanduru/corral/internal/domain/health"
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
	"github.com/sorinpanduru/corral/internal/infra/upstreamnet"
)

// msgBound is the constant record written when the wire listener is bound.
const msgBound = "wire listener bound"

// HoldPoolClose is the hold point before FR-47 step 5.
const HoldPoolClose = "shutdown_pool_close"

// Hooks are the corraltest hooks (FR-64). All may be zero.
type Hooks struct {
	Hold         func(ctx context.Context, point string)
	RoundRobin   bool
	PanicCommand string
	Probe        exchange.Probe
}

// Deps are the service's dependencies.
type Deps struct {
	Logs           *logging.Factory
	Metrics        *metrics.Factory
	Tracer         trace.Tracer
	TracingEnabled bool
	Version        string
	Clock          clock.Clock
	Hooks          Hooks
}

// Service is the running proxy.
type Service struct {
	cfg     settings.Proxy
	d       Deps
	log     *slog.Logger
	mon     *upstream.Monitor
	pool    *pool.Pool[*upstreamconn.Conn]
	checker upstream.Checker
	sd      *proxy.Shutdown
	srv     *proxyserver.Server
}

// New builds the service. No I/O happens here.
func New(cfg settings.Proxy, d Deps) (*Service, error) {
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	ad, err := proxyobs.New(d.Logs, d.Metrics, time.Now)
	if err != nil {
		return nil, err
	}
	ad.SetPoolMax(cfg.Pool.MaxSize)
	log, err := d.Logs.Logger(logging.Proxy)
	if err != nil {
		return nil, err
	}
	u := cfg.Upstream
	ucfg := upstreamconn.Config{Version: d.Version, ConnectTimeout: u.ConnectTimeout, Clock: d.Clock}
	if u.Mechanism != "none" {
		ucfg.Credentials = &upstreamconn.Credentials{Mechanism: u.Mechanism, Source: u.Source,
			Username: u.Username.Bytes(), Password: u.Password.Bytes()}
	}
	if u.TLS != nil {
		ca := u.TLS.CAPEM
		ucfg.TLS = func(host string) (*tls.Config, error) {
			name, _, err := net.SplitHostPort(host)
			if err != nil {
				name = host
			}
			return upstreamnet.ClientTLS(ca, name)
		}
	}
	var serverTLS *tls.Config
	if cfg.TLS != nil {
		if serverTLS, err = upstreamnet.ServerTLS(cfg.TLS.CertPEM, cfg.TLS.Key.Bytes(), cfg.TLS.ClientCAPEM); err != nil {
			return nil, err
		}
	}

	s := &Service{cfg: cfg, d: d, log: log}
	s.checker = upstreamconn.NewChecker(ucfg, ad.Upstream())
	s.mon = upstream.NewMonitor(upstream.Config{Seeds: u.Hosts, ReplicaSet: u.ReplicaSet, HasReplicaSet: u.HasReplicaSet,
		HeartbeatInterval: u.HeartbeatInterval, Authenticate: u.Mechanism != "none"}, s.checker, d.Clock, ad.Upstream())

	order := pool.OrderMRU
	if d.Hooks.RoundRobin {
		order = pool.OrderRoundRobin
	}
	s.pool = pool.New[*upstreamconn.Conn](pool.Config{MaxSize: cfg.Pool.MaxSize, MinSize: cfg.Pool.MinSize,
		MaxWaiters: cfg.Pool.MaxWaiters, CheckoutTimeout: cfg.Pool.CheckoutTimeout, MaxIdleTime: u.MaxIdleTime, Order: order},
		upstreamconn.NewOpener(ucfg), gate{s.mon}, d.Clock,
		&poolObserver{Observer: ad.Pool(), mon: s.mon, auth: u.Mechanism != "none"})
	s.mon.OnChange(s.onChange)

	s.sd = proxy.NewShutdown(cfg.ListenerCloseDelay, cfg.DrainTimeout, d.Clock)
	eng := exchange.New(exchange.Deps{
		Leaser: leaser{p: s.pool, svc: s}, Upstream: view{s.mon}, Obs: ad.Proxy(),
		Spans: spans.New(d.Tracer, d.TracingEnabled), Clock: d.Clock,
		IOTimeout: cfg.ClientIOTimeout, IdleTimeout: cfg.ClientIdleTimeout, Shutdown: s.sd,
		ReplyCodes: s.mon.ReplyCodes, Probe: d.Hooks.Probe, PanicCommand: d.Hooks.PanicCommand,
	})
	s.srv = proxyserver.New(proxyserver.Options{Addr: cfg.Listen, MaxConns: cfg.MaxClientConnections, TLS: serverTLS,
		IOTimeout: cfg.ClientIOTimeout, Engine: eng, Obs: ad.Proxy(), Shutdown: s.sd, IDs: proxy.NewConnIDs(), Hold: d.Hooks.Hold})
	return s, nil
}

// onChange drives the pool from monitor changes (FR-18, FR-35).
func (s *Service) onChange(c upstream.Change) {
	switch c.Kind {
	case upstream.BecameIncompatible:
		s.pool.CloseHost(c.Host, pool.CloseIncompatible)
	case upstream.HostNotPrimary:
		s.pool.CloseHost(c.Host, pool.CloseNotPrimary)
	case upstream.HostUnknown:
		s.pool.AbortBarriers(c.Host)
	}
	s.pool.AdmissionChanged()
}

// startWatch (re)starts the FR-62 idle watcher on c.
func (s *Service) startWatch(c *upstreamconn.Conn) {
	c.StopIdleWatch()
	c.StartIdleWatch(func(err error) { s.pool.DiscardIdle(c, err) })
}

// Start starts monitoring in the background (FR-46: startup does not wait for
// the upstream).
func (s *Service) Start(ctx context.Context) { s.mon.Start(ctx) }

// Bind binds the wire listener and, on success, writes the `wire listener
// bound` record (no attributes: never the address).
func (s *Service) Bind() error {
	if err := s.srv.Bind(); err != nil {
		return err
	}
	s.log.Info(msgBound)
	return nil
}

// Serve runs the accept loop until Shutdown or ctx ends.
func (s *Service) Serve(ctx context.Context) error { return s.srv.Serve(ctx) }

// Readiness is the readiness port for the admin listener (FR-46).
func (s *Service) Readiness() health.Readiness { return s.mon }

// ForceDrainEnd is the second signal (FR-63).
func (s *Service) ForceDrainEnd() { s.srv.ForceDrainEnd() }

// Shutdown runs FR-47: steps 1 to 4 in the server, then step 5, closing the
// pool and the monitoring connections.
func (s *Service) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if h := s.d.Hooks.Hold; h != nil {
		h(ctx, HoldPoolClose)
	}
	s.pool.Close(ctx)
	s.mon.Stop(ctx)
	if c, ok := s.checker.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return err
}

// ---- adapters ----

type gate struct{ m *upstream.Monitor }

func (g gate) Admission() pool.Admission {
	a := g.m.Admission()
	out := pool.Admission{Target: a.Target, Detail: a.Detail}
	switch a.Kind {
	case upstream.Ready:
		out.Kind = pool.Ready
	case upstream.Wait:
		out.Kind = pool.Wait
	default:
		out.Kind = pool.FailFast
	}
	return out
}

type view struct{ m *upstream.Monitor }

func (v view) HandshakeAdmission() (upstream.Params, string) { return v.m.HandshakeAdmission() }

// defaultMaxMessage is the server default before any hello reply is known.
const defaultMaxMessage = 48000000

func (v view) MaxMessageSize() int32 {
	if st := v.m.State(); st.HasParams && st.Params.MaxMessageSizeBytes > 0 {
		return st.Params.MaxMessageSizeBytes
	}
	return defaultMaxMessage
}

// poolObserver forwards pool events to the telemetry adapter and feeds
// connection results back to the monitor (FR-22, FR-37).
type poolObserver struct {
	pool.Observer
	mon  *upstream.Monitor
	auth bool
}

func (o *poolObserver) ConnOpenFailed(ctx context.Context, host string, err error) {
	o.Observer.ConnOpenFailed(ctx, host, err)
	o.mon.PoolConnectFailed(ctx, host, err)
	if o.auth {
		o.mon.PoolAuthResult(ctx, host, err)
	}
}

func (o *poolObserver) ConnOpened(ctx context.Context, host string, id uint64) {
	o.Observer.ConnOpened(ctx, host, id)
	if o.auth {
		o.mon.PoolAuthResult(ctx, host, nil)
	}
}

type lease struct {
	l   *pool.Lease[*upstreamconn.Conn]
	svc *Service
}

func (l *lease) Upstream() exchange.Upstream { return l.l.Conn() }
func (l *lease) Return() {
	// FR-62: start watching before the return, so a connection handed straight
	// to a waiter (whose checkout stops the watch) is never left watched.
	l.svc.startWatch(l.l.Conn())
	l.l.Return()
}
func (l *lease) Discard(r pool.CloseReason) { l.l.Discard(r) }
func (l *lease) Aborted() <-chan struct{}   { return l.l.Aborted() }

type leaser struct {
	p   *pool.Pool[*upstreamconn.Conn]
	svc *Service
}

func (l leaser) Checkout(ctx context.Context) (exchange.Lease, error) {
	ls, err := l.p.Checkout(ctx)
	if err != nil {
		return nil, err
	}
	ls.Conn().StopIdleWatch() // nothing may peek while the exchange reads
	return &lease{l: ls, svc: l.svc}, nil
}
