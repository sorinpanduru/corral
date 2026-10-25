// Package proxyobs adapts the domain observer ports (proxy, pool, upstream) to
// the SPEC-002 metrics and the FR-49 log events: exact constant messages,
// components, allowed keys, and enumerated reasons.
//
// Compliance (SPEC-001 FR-24, FR-25, FR-26): errors reach records only through
// errclass.LogAttrs (class, detail, and for upstream error replies the server
// code), client-supplied strings go through sanitize, and message bytes never
// appear. Synthesized errors never carry db.response.status_code. Rate-limited
// warn and error events use one limiter per event (30 s) and report
// suppressed_count on the next record.
package proxyobs

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/ratelimit"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

// Fixed log messages (FR-49).
const (
	msgConnOpened      = "client connection opened"
	msgConnClosed      = "client connection closed"
	msgConnRejected    = "client connection rejected"
	msgTLSFailed       = "client TLS handshake failed"
	msgHandshake       = "client handshake answered"
	msgAuthRejected    = "client authentication rejected"
	msgBlocked         = "client command blocked"
	msgProtocolError   = "client protocol error"
	msgIOTimeout       = "client I/O timed out"
	msgPanic           = "request handler panic recovered"
	msgCheckoutTimeout = "pool checkout timed out"
	msgQueueFull       = "pool wait queue full"
	msgUpConnOpened    = "upstream connection opened"
	msgUpConnClosed    = "upstream connection closed"
	msgMonOpened       = "upstream monitoring connection opened"
	msgMonClosed       = "upstream monitoring connection closed"
	msgUpConnFailed    = "upstream connection failed"
	msgUpAuthFailed    = "upstream authentication failed"
	msgUnavailable     = "upstream unavailable"
	msgAvailable       = "upstream available"
	msgIncompatible    = "upstream incompatible"
	msgPrimaryChanged  = "upstream primary changed"
	msgMarkedNP        = "upstream target marked not primary"
	msgUpProtocolError = "upstream protocol error"
)

const limitWindow = 30 * time.Second

var (
	closeReasons   = []string{"client_closed", "idle_timeout", "io_timeout", "protocol_error", "upstream_failure", "shutdown", "tls_failure", "limit_exceeded", "internal_error"}
	upCloseReasons = []string{"idle", "error", "not_primary", "client_abandoned", "shutdown", "incompatible"}
	protoDetails   = []string{"unsupported_opcode", "message_too_large", "malformed_message", "checksum_mismatch", "unexpected_response"}
	checkoutDetail = []string{"pool_wait_timeout", "pool_queue_full", "upstream_unavailable", "upstream_auth_failed", "upstream_incompatible", "shutting_down"}
	readyReasons   = []string{"upstream_unavailable", "upstream_auth_failed", "upstream_incompatible"}
	incompatReason = []string{"wire_version", "standalone", "topology_mismatch", "replica_set_name_mismatch"}
)

func in(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Adapters holds the three observers.
type Adapters struct {
	proxyLog, poolLog, upLog *slog.Logger
	now                      func() time.Time
	limiters                 map[string]*ratelimit.Limiter

	m struct {
		accepted, closedConn, authRejected, blocked, dropped, primaryChanges *metrics.Counter
		clientConns, poolMax, poolWaiters, upAvailable                       *metrics.Gauge
		poolWait                                                             *metrics.Histogram

		closedVec, handshakes, protoErrs, requests, reqErrs, poolConns, checkouts *metrics.CounterVec
		checkoutFail, opened, upClosed, connectErrs, authFails                    *metrics.CounterVec
		poolState                                                                 *metrics.GaugeVec
		reqDur                                                                    *metrics.HistogramVec
	}
}

// New builds the adapters. It fails if the metrics factory does not carry the
// SPEC-002 metrics, so a missing catalog entry is an error at startup.
func New(logs *logging.Factory, mf *metrics.Factory, now func() time.Time) (*Adapters, error) {
	a := &Adapters{now: now, limiters: map[string]*ratelimit.Limiter{}}
	var err error
	if a.proxyLog, err = logs.Logger(logging.Proxy); err != nil {
		return nil, err
	}
	if a.poolLog, err = logs.Logger(logging.Pool); err != nil {
		return nil, err
	}
	if a.upLog, err = logs.Logger(logging.Upstream); err != nil {
		return nil, err
	}
	for _, msg := range []string{msgConnRejected, msgAuthRejected, msgBlocked, msgProtocolError, msgIOTimeout, msgTLSFailed,
		msgCheckoutTimeout, msgQueueFull, msgUpConnFailed, msgUpAuthFailed, msgMarkedNP, msgUpProtocolError} {
		a.limiters[msg] = ratelimit.New(limitWindow, now)
	}
	var errs []error
	c := func(name string) *metrics.Counter { x, e := mf.Counter(name); errs = append(errs, e); return x }
	g := func(name string) *metrics.Gauge { x, e := mf.Gauge(name); errs = append(errs, e); return x }
	cv := func(name string) *metrics.CounterVec { x, e := mf.CounterVec(name); errs = append(errs, e); return x }
	a.m.accepted = c(metrics.NameClientConnectionsAcceptedTotal)
	a.m.authRejected = c(metrics.NameClientAuthRejectedTotal)
	a.m.blocked = c(metrics.NameCommandsBlockedTotal)
	a.m.dropped = c(metrics.NameRequestsDroppedTotal)
	a.m.primaryChanges = c(metrics.NameUpstreamPrimaryChangesTotal)
	a.m.clientConns = g(metrics.NameClientConnections)
	a.m.poolMax = g(metrics.NamePoolMaxConnections)
	a.m.poolWaiters = g(metrics.NamePoolWaiters)
	a.m.upAvailable = g(metrics.NameUpstreamAvailable)
	h, e := mf.Histogram(metrics.NamePoolCheckoutWaitSeconds)
	a.m.poolWait, errs = h, append(errs, e)
	a.m.closedVec = cv(metrics.NameClientConnectionsClosedTotal)
	a.m.handshakes = cv(metrics.NameHandshakesTotal)
	a.m.protoErrs = cv(metrics.NameProtocolErrorsTotal)
	a.m.requests = cv(metrics.NameRequestsTotal)
	a.m.reqErrs = cv(metrics.NameRequestErrorsTotal)
	a.m.checkouts = cv(metrics.NamePoolCheckoutsTotal)
	a.m.checkoutFail = cv(metrics.NamePoolCheckoutFailuresTotal)
	a.m.opened = cv(metrics.NameUpstreamConnectionsOpenedTotal)
	a.m.upClosed = cv(metrics.NameUpstreamConnectionsClosedTotal)
	a.m.connectErrs = cv(metrics.NameUpstreamConnectErrorsTotal)
	a.m.authFails = cv(metrics.NameUpstreamAuthFailuresTotal)
	gv, e := mf.GaugeVec(metrics.NamePoolConnections)
	a.m.poolState, errs = gv, append(errs, e)
	hv, e := mf.HistogramVec(metrics.NameRequestDurationSeconds)
	a.m.reqDur, errs = hv, append(errs, e)
	if err := errors.Join(errs...); err != nil {
		return nil, errors.New("proxyobs: the metrics factory lacks SPEC-002 metrics")
	}
	return a, nil
}

// Proxy returns the client-side observer.
func (a *Adapters) Proxy() proxy.Observer { return &proxyObs{a} }

// Pool returns the pool observer.
func (a *Adapters) Pool() pool.Observer { return &poolObs{a} }

// Upstream returns the upstream monitor observer.
func (a *Adapters) Upstream() upstream.Observer { return &upstreamObs{a} }

// inc increments a labelled counter, ignoring a value outside its declared
// set (which the factory refuses).
func inc(v *metrics.CounterVec, labels ...string) {
	if c, err := v.WithLabelValues(labels...); err == nil {
		c.Inc()
	}
}

// allow applies the per-event limiter and appends suppressed_count.
func (a *Adapters) allow(msg string, attrs []slog.Attr) ([]slog.Attr, bool) {
	ok, n := a.limiters[msg].Allow()
	if !ok {
		return nil, false
	}
	if n > 0 {
		attrs = append(attrs, slog.Int(attr.SuppressedCount, n))
	}
	return attrs, true
}

func hostAttrs(host string) []slog.Attr {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return []slog.Attr{slog.String(attr.ServerAddress, sanitize.CollectionForTelemetry(host))}
	}
	out := []slog.Attr{slog.String(attr.ServerAddress, sanitize.CollectionForTelemetry(h))}
	if n, err := strconv.Atoi(p); err == nil {
		out = append(out, slog.Int(attr.ServerPort, n))
	}
	return out
}

func connAttr(id int32) slog.Attr { return slog.Int64(attr.ConnectionID, int64(id)) }

func classOf(err error) string { c, _ := errclass.Classify(err); return string(c) }

// ---- proxy port ----

type proxyObs struct{ a *Adapters }

func (o *proxyObs) ConnOpened(ctx context.Context, info proxy.ConnInfo) {
	o.a.m.accepted.Inc()
	o.a.m.clientConns.Inc()
	attrs := []slog.Attr{connAttr(info.ID)}
	if info.ClientAddr != "" {
		attrs = append(attrs, slog.String(attr.ClientAddress, info.ClientAddr), slog.Int(attr.ClientPort, info.ClientPort))
	}
	o.a.proxyLog.LogAttrs(ctx, slog.LevelDebug, msgConnOpened, attrs...)
}

func (o *proxyObs) ConnClosed(ctx context.Context, id int32, reason proxy.CloseReason) {
	r := string(reason)
	if !in(closeReasons, r) {
		r = "internal_error"
	}
	inc(o.a.m.closedVec, r)
	o.a.m.clientConns.Dec()
	o.a.proxyLog.LogAttrs(ctx, slog.LevelDebug, msgConnClosed, slog.String(attr.Reason, r), connAttr(id))
}

func (o *proxyObs) ConnRejected(ctx context.Context, reason proxy.RejectReason) {
	// Over-limit connections count as accepted and closed, never in the gauge.
	o.a.m.accepted.Inc()
	inc(o.a.m.closedVec, "limit_exceeded")
	if attrs, ok := o.a.allow(msgConnRejected, []slog.Attr{slog.String(attr.Reason, "limit_exceeded")}); ok {
		o.a.proxyLog.LogAttrs(ctx, slog.LevelWarn, msgConnRejected, attrs...)
	}
}

func (o *proxyObs) TLSFailed(ctx context.Context, err error) {
	if attrs, ok := o.a.allow(msgTLSFailed, errclass.LogAttrs(err)); ok {
		o.a.proxyLog.LogAttrs(ctx, slog.LevelWarn, msgTLSFailed, attrs...)
	}
}

func (o *proxyObs) HandshakeAnswered(ctx context.Context, info proxy.HandshakeInfo) {
	op := info.OpCode
	if op != "op_msg" && op != "op_query" {
		op = "op_msg"
	}
	outcome := "error"
	if info.OK {
		outcome = "ok"
	}
	inc(o.a.m.handshakes, op, outcome)
	attrs := []slog.Attr{connAttr(info.ID)}
	if m := info.Metadata; m.Has {
		if m.AppName != "" {
			attrs = append(attrs, slog.String(attr.ClientAppName, sanitize.AppName(m.AppName)))
		}
		if m.DriverName != "" {
			attrs = append(attrs, slog.String(attr.ClientDriverName, sanitize.DriverName(m.DriverName)))
		}
		if m.DriverVersion != "" {
			attrs = append(attrs, slog.String(attr.ClientDriverVersion, sanitize.DriverVersion(m.DriverVersion)))
		}
	}
	o.a.proxyLog.LogAttrs(ctx, slog.LevelDebug, msgHandshake, attrs...)
}

func (o *proxyObs) ClientAuthRejected(ctx context.Context, id int32, r proxy.Refusal) {
	o.a.m.authRejected.Inc()
	if attrs, ok := o.a.allow(msgAuthRejected, append(errclass.LogAttrs(r), connAttr(id))); ok {
		o.a.proxyLog.LogAttrs(ctx, slog.LevelWarn, msgAuthRejected, attrs...)
	}
}

func (o *proxyObs) CommandBlocked(ctx context.Context, id int32, command string) {
	o.a.m.blocked.Inc()
	attrs := append(errclass.LogAttrs(proxy.CommandBlocked),
		slog.String(attr.DBOperationName, sanitize.CommandForTelemetry(command)), connAttr(id))
	if attrs, ok := o.a.allow(msgBlocked, attrs); ok {
		o.a.proxyLog.LogAttrs(ctx, slog.LevelWarn, msgBlocked, attrs...)
	}
}

func (o *proxyObs) ClientProtocolError(ctx context.Context, id int32, err error) {
	if _, d := errclass.Classify(err); in(protoDetails, string(d)) {
		inc(o.a.m.protoErrs, "client", string(d))
	}
	if attrs, ok := o.a.allow(msgProtocolError, append(errclass.LogAttrs(err), connAttr(id))); ok {
		o.a.proxyLog.LogAttrs(ctx, slog.LevelWarn, msgProtocolError, attrs...)
	}
}

func (o *proxyObs) ClientIOTimeout(ctx context.Context, id int32) {
	attrs := append(errclass.LogAttrs(context.DeadlineExceeded), connAttr(id))
	if attrs, ok := o.a.allow(msgIOTimeout, attrs); ok {
		o.a.proxyLog.LogAttrs(ctx, slog.LevelWarn, msgIOTimeout, attrs...)
	}
}

func (o *proxyObs) RequestReceived(context.Context, string) {}

func (o *proxyObs) RequestCompleted(ctx context.Context, res proxy.RequestResult) {
	cmd := sanitize.MetricLabel(res.Command)
	inc(o.a.m.requests, cmd, res.Outcome.String())
	if c, err := o.a.m.reqDur.WithLabelValues(cmd); err == nil {
		c.Observe(res.Duration.Seconds())
	}
	if res.Outcome == proxy.OutcomeProxyError {
		inc(o.a.m.reqErrs, classOf(res.Err))
	}
	if res.Dropped {
		o.a.m.dropped.Inc()
	}
}

func (o *proxyObs) PanicRecovered(ctx context.Context, err error) {
	o.a.proxyLog.LogAttrs(ctx, slog.LevelError, msgPanic, errclass.LogAttrs(err)...)
}

// ---- pool port ----

type poolObs struct{ a *Adapters }

func (o *poolObs) CheckoutStarted(context.Context) {}
func (o *poolObs) CheckoutQueued(context.Context)  {}

func (o *poolObs) CheckoutSucceeded(ctx context.Context, wait time.Duration) {
	inc(o.a.m.checkouts, "ok")
	o.a.m.poolWait.Observe(wait.Seconds())
}

func (o *poolObs) CheckoutFailed(ctx context.Context, detail string, wait time.Duration) {
	inc(o.a.m.checkouts, "failed")
	o.a.m.poolWait.Observe(wait.Seconds())
	if in(checkoutDetail, detail) {
		inc(o.a.m.checkoutFail, detail)
	}
	switch detail {
	case "pool_wait_timeout":
		if attrs, ok := o.a.allow(msgCheckoutTimeout, nil); ok {
			o.a.poolLog.LogAttrs(ctx, slog.LevelWarn, msgCheckoutTimeout, attrs...)
		}
	case "pool_queue_full":
		if attrs, ok := o.a.allow(msgQueueFull, nil); ok {
			o.a.poolLog.LogAttrs(ctx, slog.LevelWarn, msgQueueFull, attrs...)
		}
	}
}

func (o *poolObs) ConnOpening(context.Context, string) {}

func (o *poolObs) ConnOpened(ctx context.Context, host string, id uint64) {
	inc(o.a.m.opened, "pool")
	attrs := append(hostAttrs(host), slog.Int64(attr.UpstreamConnectionID, int64(id)))
	o.a.upLog.LogAttrs(ctx, slog.LevelDebug, msgUpConnOpened, attrs...)
}

func (o *poolObs) ConnOpenFailed(ctx context.Context, host string, err error) {
	// An authentication failure is reported by upstream.Observer.AuthFailed,
	// which counts both the connect error and the auth failure once.
	if upstream.IsAuthFailed(err) {
		return
	}
	connectFailed(o.a, ctx, "pool", host, err)
}

func (o *poolObs) ConnClosed(ctx context.Context, host string, reason pool.CloseReason) {
	r := string(reason)
	if !in(upCloseReasons, r) {
		r = "error"
	}
	inc(o.a.m.upClosed, "pool", r)
	o.a.upLog.LogAttrs(ctx, slog.LevelDebug, msgUpConnClosed, append(hostAttrs(host), slog.String(attr.Reason, r))...)
}

func (o *poolObs) ConnUnsolicited(ctx context.Context, host string) {
	inc(o.a.m.protoErrs, "upstream", "unexpected_response")
	attrs := append(errclass.LogAttrs(wireUnexpected{}), hostAttrs(host)...)
	if attrs, ok := o.a.allow(msgUpProtocolError, attrs); ok {
		o.a.upLog.LogAttrs(ctx, slog.LevelWarn, msgUpProtocolError, attrs...)
	}
}

// wireUnexpected is a DetailedError for unexpected_response (class protocol).
type wireUnexpected struct{}

func (wireUnexpected) Error() string             { return "unexpected upstream data" }
func (wireUnexpected) CorralErrorDetail() string { return "unexpected_response" }

func (o *poolObs) StatsChanged(ctx context.Context, s pool.Stats) {
	for state, n := range map[string]int{"idle": s.Idle, "in_use": s.InUse, "connecting": s.Connecting} {
		if g, err := o.a.m.poolState.WithLabelValues(state); err == nil {
			g.Set(float64(n))
		}
	}
	o.a.m.poolWaiters.Set(float64(s.Waiters))
}

func connectFailed(a *Adapters, ctx context.Context, kind, host string, err error) {
	inc(a.m.connectErrs, kind, classOf(err))
	attrs := append(errclass.LogAttrs(err), hostAttrs(host)...)
	if attrs, ok := a.allow(msgUpConnFailed, attrs); ok {
		a.upLog.LogAttrs(ctx, slog.LevelWarn, msgUpConnFailed, attrs...)
	}
}

// ---- upstream port ----

type upstreamObs struct{ a *Adapters }

func (o *upstreamObs) AvailabilityChanged(ctx context.Context, ready bool, reason health.Reason) {
	if ready {
		o.a.m.upAvailable.Set(1)
		return
	}
	o.a.m.upAvailable.Set(0)
	r := string(reason)
	if !in(readyReasons, r) {
		r = "upstream_unavailable"
	}
	o.a.upLog.LogAttrs(ctx, slog.LevelWarn, msgUnavailable, slog.String(attr.Reason, r))
}

func (o *upstreamObs) Available(ctx context.Context) {
	o.a.upLog.LogAttrs(ctx, slog.LevelInfo, msgAvailable)
}

func (o *upstreamObs) Incompatible(ctx context.Context, reason upstream.Reason, host string) {
	r := string(reason)
	if !in(incompatReason, r) {
		r = "topology_mismatch"
	}
	o.a.upLog.LogAttrs(ctx, slog.LevelError, msgIncompatible, append([]slog.Attr{slog.String(attr.Reason, r)}, hostAttrs(host)...)...)
}

func (o *upstreamObs) PrimaryChanged(ctx context.Context, host string) {
	o.a.m.primaryChanges.Inc()
	o.a.upLog.LogAttrs(ctx, slog.LevelInfo, msgPrimaryChanged, hostAttrs(host)...)
}

type notPrimaryErr struct {
	code int32
	name string
}

func (e *notPrimaryErr) Error() string          { return "upstream reported it is not primary" }
func (e *notPrimaryErr) ServerCode() int32      { return e.code }
func (e *notPrimaryErr) ServerCodeName() string { return e.name }

func (o *upstreamObs) TargetMarkedNotPrimary(ctx context.Context, host string, code int32, codeName string) {
	attrs := append(errclass.LogAttrs(&notPrimaryErr{code, codeName}), hostAttrs(host)...)
	if attrs, ok := o.a.allow(msgMarkedNP, attrs); ok {
		o.a.upLog.LogAttrs(ctx, slog.LevelWarn, msgMarkedNP, attrs...)
	}
}

func (o *upstreamObs) MonitorConnOpened(ctx context.Context, host string, id uint64) {
	inc(o.a.m.opened, "monitor")
	o.a.upLog.LogAttrs(ctx, slog.LevelDebug, msgMonOpened, append(hostAttrs(host), slog.Int64(attr.UpstreamConnectionID, int64(id)))...)
}

func (o *upstreamObs) MonitorConnClosed(ctx context.Context, host string, reason string) {
	if !in(upCloseReasons, reason) {
		reason = "error"
	}
	inc(o.a.m.upClosed, "monitor", reason)
	o.a.upLog.LogAttrs(ctx, slog.LevelDebug, msgMonClosed, append(hostAttrs(host), slog.String(attr.Reason, reason))...)
}

func (o *upstreamObs) MonitorConnFailed(ctx context.Context, host string, err error) {
	connectFailed(o.a, ctx, "monitor", host, err)
}

func (o *upstreamObs) AuthSucceeded(ctx context.Context, kind, host string) {}

func (o *upstreamObs) AuthFailed(ctx context.Context, kind, host string, err error) {
	if kind != "pool" && kind != "monitor" {
		kind = "pool"
	}
	inc(o.a.m.connectErrs, kind, "server")
	inc(o.a.m.authFails, kind)
	attrs := append(errclass.LogAttrs(err), hostAttrs(host)...)
	if attrs, ok := o.a.allow(msgUpAuthFailed, attrs); ok {
		o.a.upLog.LogAttrs(ctx, slog.LevelWarn, msgUpAuthFailed, attrs...)
	}
}
