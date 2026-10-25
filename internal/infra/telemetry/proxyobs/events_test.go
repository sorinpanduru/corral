package proxyobs_test

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/domain/health"
	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/proxyobs"
	"github.com/sorinpanduru/corral/internal/testkit/logcheck"
)

type srvErr struct {
	code int32
	name string
}

func (e srvErr) Error() string          { return "synthetic upstream reply text SHOULD-NOT-APPEAR" }
func (e srvErr) ServerCode() int32      { return e.code }
func (e srvErr) ServerCodeName() string { return e.name }

var (
	closeReasons  = []string{"client_closed", "idle_timeout", "io_timeout", "protocol_error", "upstream_failure", "shutdown", "tls_failure", "limit_exceeded", "internal_error"}
	upCloseReason = []string{"idle", "error", "not_primary", "client_abandoned", "shutdown", "incompatible"}
	readiness     = []string{"upstream_unavailable", "upstream_auth_failed", "upstream_incompatible"}
	incompat      = []string{"wire_version", "standalone", "topology_mismatch", "replica_set_name_mismatch"}
)

// New refuses a metrics factory that lacks the SPEC-002 metrics (an error, not
// a panic later).
func TestNewRequiresCatalog_FR48(t *testing.T) {
	r := newRig(t)
	empty, err := metrics.NewFactory(metrics.NewRegistry(), &metrics.Catalog{})
	if err != nil {
		t.Skipf("an empty catalog is refused by the factory itself: %v", err)
	}
	if _, err := proxyobs.New(logging.NewFactory(r.lh), empty, r.clk.Now); err == nil {
		t.Fatal("New accepted a factory without the SPEC-002 metrics")
	}
}

// ---- proxy port: one test per FR-49 event ----

func TestEventClientConnectionOpened(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ConnOpened(r.ctx, proxy.ConnInfo{ID: 17, ClientAddr: "192.0.2.9", ClientPort: 40123})
	a := r.one("client connection opened", slog.LevelDebug, "proxy", "corral.connection.id", "client.address", "client.port")
	if a["corral.connection.id"].Int64() != 17 || a["client.address"].String() != "192.0.2.9" || a["client.port"].Int64() != 40123 {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_client_connections_accepted_total") != 1 || r.val("corral_client_connections") != 1 {
		t.Fatalf("accepted=%v gauge=%v", r.val("corral_client_connections_accepted_total"), r.val("corral_client_connections"))
	}
}

func TestEventClientConnectionClosed(t *testing.T) {
	for _, reason := range closeReasons {
		t.Run(reason, func(t *testing.T) {
			r := newRig(t)
			r.ad.Proxy().ConnOpened(r.ctx, proxy.ConnInfo{ID: 3})
			r.ad.Proxy().ConnClosed(r.ctx, 3, proxy.CloseReason(reason))
			a := r.one("client connection closed", slog.LevelDebug, "proxy", "reason", "corral.connection.id")
			if a["reason"].String() != reason || a["corral.connection.id"].Int64() != 3 {
				t.Fatalf("attributes: %v", a)
			}
			if !contains(closeReasons, a["reason"].String()) {
				t.Fatalf("reason %q outside the enumeration", a["reason"].String())
			}
			if r.val("corral_client_connections_closed_total", "reason", reason) != 1 {
				t.Fatalf("closed_total{reason=%s} = %v", reason, r.val("corral_client_connections_closed_total", "reason", reason))
			}
			if r.val("corral_client_connections") != 0 {
				t.Fatal("the gauge did not return to 0")
			}
		})
	}
}

// AC-39 (unit): over-limit connections count in accepted_total and
// closed_total{limit_exceeded}, never in the gauge.
func TestEventConnectionRejected_AC39(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ConnRejected(r.ctx, proxy.RejectLimitExceeded)
	a := r.one("client connection rejected", slog.LevelWarn, "proxy", "reason")
	if a["reason"].String() != "limit_exceeded" {
		t.Fatalf("reason %q", a["reason"].String())
	}
	if r.val("corral_client_connections_accepted_total") != 1 {
		t.Fatal("an over-limit connection must count as accepted")
	}
	if r.val("corral_client_connections") != 0 {
		t.Fatal("an over-limit connection reached the gauge")
	}
	if r.val("corral_client_connections_closed_total", "reason", "limit_exceeded") != 1 {
		t.Fatal("closed_total{limit_exceeded} not incremented")
	}
}

// AC-39 (unit): a TLS-failed connection counts as accepted, is in the gauge
// while in the handshake, and leaves it when closed.
func TestEventTLSFailed_AC39(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ConnOpened(r.ctx, proxy.ConnInfo{ID: 8, TLS: true})
	if r.val("corral_client_connections") != 1 {
		t.Fatal("a connection in its TLS handshake must be in the gauge")
	}
	m := "SHOULD-NOT-APPEAR-CERT"
	r.ad.Proxy().TLSFailed(r.ctx, fmt.Errorf("%s: %w", m, tls.AlertError(42)))
	a := r.one("client TLS handshake failed", slog.LevelWarn, "proxy", "error", "corral.error.detail")
	if a["error"].String() != "network" || a["corral.error.detail"].String() != "tls_handshake" {
		t.Fatalf("error=%q detail=%q", a["error"].String(), a["corral.error.detail"].String())
	}
	noMarker(t, r, m)
	r.ad.Proxy().ConnClosed(r.ctx, 8, proxy.CloseTLSFailure)
	if r.val("corral_client_connections_accepted_total") != 1 || r.val("corral_client_connections") != 0 ||
		r.val("corral_client_connections_closed_total", "reason", "tls_failure") != 1 {
		t.Fatal("TLS failure accounting wrong")
	}
}

func TestEventHandshakeAnswered(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().HandshakeAnswered(r.ctx, proxy.HandshakeInfo{ID: 4, OpCode: "op_msg", OK: true,
		Metadata: proxy.Metadata{Has: true, AppName: "synthetic-app", DriverName: "synthetic-driver", DriverVersion: "2.1"}})
	a := r.one("client handshake answered", slog.LevelDebug, "proxy", "corral.connection.id", "corral.client.app_name", "corral.client.driver.name", "corral.client.driver.version")
	if a["corral.client.app_name"].String() != "synthetic-app" || a["corral.client.driver.version"].String() != "2.1" || a["corral.connection.id"].Int64() != 4 {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_handshakes_total", "opcode", "op_msg", "outcome", "ok") != 1 {
		t.Fatal("handshakes_total{op_msg, ok} not incremented")
	}
	// An error outcome over OP_QUERY, with no metadata: no metadata keys.
	r2 := newRig(t)
	r2.ad.Proxy().HandshakeAnswered(r2.ctx, proxy.HandshakeInfo{ID: 5, OpCode: "op_query", OK: false})
	r2.one("client handshake answered", slog.LevelDebug, "proxy", "corral.connection.id")
	if r2.val("corral_handshakes_total", "opcode", "op_query", "outcome", "error") != 1 {
		t.Fatal("handshakes_total{op_query, error} not incremented")
	}
}

// SPEC-001 FR-25: metadata is sanitised (invalid UTF-8 replaced, 128 / 64
// byte caps) before it reaches a record, and nothing else leaks.
func TestHandshakeMetadataSanitised_FR25(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().HandshakeAnswered(r.ctx, proxy.HandshakeInfo{ID: 1, OpCode: "op_msg", OK: true, Metadata: proxy.Metadata{
		Has: true, AppName: strings.Repeat("€", 100), DriverName: "d\xffd" + strings.Repeat("n", 100), DriverVersion: strings.Repeat("9", 100)}})
	a := attrs(r.records("client handshake answered")[0])
	app, drv, ver := a["corral.client.app_name"].String(), a["corral.client.driver.name"].String(), a["corral.client.driver.version"].String()
	if len(app) > 128 || !utf8.ValidString(app) || len(app) == 0 {
		t.Errorf("app name: %d bytes", len(app))
	}
	if len(drv) > 64 || !utf8.ValidString(drv) || !strings.HasPrefix(drv, "d�d") {
		t.Errorf("driver name: %q", drv)
	}
	if len(ver) > 64 {
		t.Errorf("driver version: %d bytes", len(ver))
	}
}

func TestEventClientAuthRejected(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ClientAuthRejected(r.ctx, 6, proxy.ClientAuthSASL)
	a := r.one("client authentication rejected", slog.LevelWarn, "proxy", "error", "corral.error.detail", "corral.connection.id")
	if a["error"].String() != "rejected" || a["corral.error.detail"].String() != "client_auth_unsupported" || a["corral.connection.id"].Int64() != 6 {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_client_auth_rejected_total") != 1 {
		t.Fatal("client_auth_rejected_total not incremented")
	}
}

func TestEventCommandBlocked(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().CommandBlocked(r.ctx, 7, "currentOp")
	a := r.one("client command blocked", slog.LevelWarn, "proxy", "error", "corral.error.detail", "db.operation.name", "corral.connection.id")
	if a["error"].String() != "rejected" || a["corral.error.detail"].String() != "command_blocked" || a["db.operation.name"].String() != "currentOp" {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_commands_blocked_total") != 1 {
		t.Fatal("commands_blocked_total not incremented")
	}
	// The command name is sanitised (as received, 64 bytes, valid UTF-8).
	r2 := newRig(t)
	r2.ad.Proxy().CommandBlocked(r2.ctx, 7, "a\xff"+strings.Repeat("b", 200))
	n := attrs(r2.records("client command blocked")[0])["db.operation.name"].String()
	if len(n) > 64 || !utf8.ValidString(n) {
		t.Fatalf("operation name: %d bytes, valid=%v", len(n), utf8.ValidString(n))
	}
}

func TestEventClientProtocolError(t *testing.T) {
	for _, tc := range []struct {
		err    error
		detail string
	}{{wire.ErrMalformed(), "malformed_message"}, {wire.ErrMessageTooLarge(), "message_too_large"}, {wire.ErrUnsupportedOpcode(), "unsupported_opcode"}, {wire.ErrChecksum(), "checksum_mismatch"}} {
		t.Run(tc.detail, func(t *testing.T) {
			r := newRig(t)
			r.ad.Proxy().ClientProtocolError(r.ctx, 9, tc.err)
			a := r.one("client protocol error", slog.LevelWarn, "proxy", "error", "corral.error.detail", "corral.connection.id")
			if a["error"].String() != "protocol" || a["corral.error.detail"].String() != tc.detail {
				t.Fatalf("attributes: %v", a)
			}
			if r.val("corral_protocol_errors_total", "side", "client", "error_detail", tc.detail) != 1 {
				t.Fatal("protocol_errors_total{client} not incremented")
			}
		})
	}
}

func TestEventClientIOTimeout(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ClientIOTimeout(r.ctx, 12)
	a := r.one("client I/O timed out", slog.LevelWarn, "proxy", "error", "corral.error.detail", "corral.connection.id")
	if a["error"].String() != "timeout" || a["corral.error.detail"].String() != "deadline_exceeded" || a["corral.connection.id"].Int64() != 12 {
		t.Fatalf("attributes: %v", a)
	}
}

func TestEventPanicRecovered(t *testing.T) {
	r := newRig(t)
	m := "SHOULD-NOT-APPEAR-PANIC"
	r.ad.Proxy().PanicRecovered(r.ctx, errclass.FromPanic(m, []byte("goroutine 1 [running]:\nsynthetic frame")))
	a := r.one("request handler panic recovered", slog.LevelError, "proxy", "error", "corral.error.detail", "stack")
	if a["error"].String() != "internal" || a["corral.error.detail"].String() != "other" || !strings.Contains(a["stack"].String(), "synthetic frame") {
		t.Fatalf("attributes: %v", a)
	}
	noMarker(t, r, m)
}

// ---- pool port ----

func TestEventPoolCheckoutFailures(t *testing.T) {
	r := newRig(t)
	r.ad.Pool().CheckoutFailed(r.ctx, "pool_wait_timeout", 5*time.Second)
	r.one("pool checkout timed out", slog.LevelWarn, "pool")
	r.ad.Pool().CheckoutFailed(r.ctx, "pool_queue_full", 0)
	r.one("pool wait queue full", slog.LevelWarn, "pool")
	for _, d := range []string{"pool_wait_timeout", "pool_queue_full"} {
		if r.val("corral_pool_checkout_failures_total", "error_detail", d) != 1 {
			t.Errorf("failures_total{%s} not incremented", d)
		}
	}
	// Fail-fast details are counted but not logged per request.
	for _, d := range []string{"upstream_unavailable", "upstream_auth_failed", "upstream_incompatible", "shutting_down"} {
		r.ad.Pool().CheckoutFailed(r.ctx, d, 0)
		if r.val("corral_pool_checkout_failures_total", "error_detail", d) != 1 {
			t.Errorf("failures_total{%s} not incremented", d)
		}
	}
	if len(r.records("pool checkout timed out")) != 1 || len(r.records("pool wait queue full")) != 1 {
		t.Error("fail-fast refusals produced pool log records")
	}
	if r.val("corral_pool_checkouts_total", "outcome", "failed") != 6 {
		t.Fatalf("checkouts_total{failed} = %v, want 6", r.val("corral_pool_checkouts_total", "outcome", "failed"))
	}
	if n, _ := r.hist("corral_pool_checkout_wait_seconds"); n != 6 {
		t.Fatalf("checkout wait observations = %d, want 6", n)
	}
}

func TestPoolCheckoutSuccessAndStats(t *testing.T) {
	r := newRig(t)
	r.ad.Pool().CheckoutStarted(r.ctx)
	r.ad.Pool().CheckoutQueued(r.ctx)
	r.ad.Pool().CheckoutSucceeded(r.ctx, 250*time.Millisecond)
	if r.val("corral_pool_checkouts_total", "outcome", "ok") != 1 {
		t.Fatal("checkouts_total{ok} not incremented")
	}
	n, sum := r.hist("corral_pool_checkout_wait_seconds")
	if n != 1 || sum < 0.249 || sum > 0.251 {
		t.Fatalf("wait histogram: count=%d sum=%v", n, sum)
	}
	r.ad.Pool().StatsChanged(r.ctx, pool.Stats{Idle: 1, InUse: 2, Connecting: 3, Waiters: 4})
	for state, want := range map[string]float64{"idle": 1, "in_use": 2, "connecting": 3} {
		if got := r.val("corral_pool_connections", "state", state); got != want {
			t.Errorf("pool_connections{%s} = %v, want %v", state, got, want)
		}
	}
	if r.val("corral_pool_waiters") != 4 {
		t.Fatalf("pool_waiters = %v", r.val("corral_pool_waiters"))
	}
	r.ad.Pool().StatsChanged(r.ctx, pool.Stats{})
	if r.sum("corral_pool_connections") != 0 || r.val("corral_pool_waiters") != 0 {
		t.Fatal("gauges did not follow the second StatsChanged")
	}
}

// ---- pool connections on the upstream log (kind=pool) ----

func TestEventPoolConnOpenedClosed(t *testing.T) {
	r := newRig(t)
	r.ad.Pool().ConnOpened(r.ctx, "db1.test:27017", 31)
	a := r.one("upstream connection opened", slog.LevelDebug, "upstream", "server.address", "server.port", "corral.upstream.connection.id")
	if a["server.address"].String() != "db1.test" || a["server.port"].Int64() != 27017 || a["corral.upstream.connection.id"].Int64() != 31 {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_upstream_connections_opened_total", "kind", "pool") != 1 {
		t.Fatal("opened_total{pool} not incremented")
	}
	for _, reason := range upCloseReason {
		r2 := newRig(t)
		r2.ad.Pool().ConnClosed(r2.ctx, "db1.test:27017", pool.CloseReason(reason))
		a := r2.one("upstream connection closed", slog.LevelDebug, "upstream", "reason", "server.address", "server.port")
		if a["reason"].String() != reason || !contains(upCloseReason, a["reason"].String()) {
			t.Fatalf("reason %q", a["reason"].String())
		}
		if r2.val("corral_upstream_connections_closed_total", "kind", "pool", "reason", reason) != 1 {
			t.Fatalf("closed_total{pool,%s} not incremented", reason)
		}
	}
	// IPv6 hosts split correctly.
	r3 := newRig(t)
	r3.ad.Pool().ConnOpened(r3.ctx, "[2001:db8::5]:27018", 1)
	a = attrs(r3.records("upstream connection opened")[0])
	if a["server.address"].String() != "2001:db8::5" || a["server.port"].Int64() != 27018 {
		t.Fatalf("IPv6 attributes: %v", a)
	}
}

func TestEventUpstreamConnectionFailed(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for kind, call := range map[string]func(r *rig){
		"pool":    func(r *rig) { r.ad.Pool().ConnOpenFailed(r.ctx, "db1.test:27017", cause) },
		"monitor": func(r *rig) { r.ad.Upstream().MonitorConnFailed(r.ctx, "db1.test:27017", cause) },
	} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			call(r)
			a := r.one("upstream connection failed", slog.LevelWarn, "upstream", "error", "corral.error.detail", "server.address", "server.port")
			if a["error"].String() != "network" || a["corral.error.detail"].String() != "ECONNREFUSED" {
				t.Fatalf("attributes: %v", a)
			}
			if r.val("corral_upstream_connect_errors_total", "kind", kind, "error_class", "network") != 1 {
				t.Fatal("connect_errors_total not incremented")
			}
			if r.sum("corral_upstream_auth_failures_total") != 0 {
				t.Fatal("a network failure counted as an authentication failure")
			}
		})
	}
}

// AC-39 (unit): an authentication failure counts in connect_errors{server} and
// auth_failures for its kind; the record carries the upstream's code.
func TestEventUpstreamAuthFailed_AC39(t *testing.T) {
	for _, kind := range []string{"pool", "monitor"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			err := &upstream.AuthFailed{Err: srvErr{18, "AuthenticationFailed"}}
			r.ad.Upstream().AuthFailed(r.ctx, kind, "db1.test:27017", err)
			a := r.one("upstream authentication failed", slog.LevelWarn, "upstream", "error", "corral.error.detail", "server.address", "server.port",
				"db.response.status_code", "corral.db.response.code_name")
			if a["error"].String() != "server" || a["corral.error.detail"].String() != "upstream_auth_failed" ||
				a["db.response.status_code"].String() != "18" || a["corral.db.response.code_name"].String() != "AuthenticationFailed" {
				t.Fatalf("attributes: %v", a)
			}
			if r.val("corral_upstream_connect_errors_total", "kind", kind, "error_class", "server") != 1 || r.val("corral_upstream_auth_failures_total", "kind", kind) != 1 {
				t.Fatalf("connect_errors{server}=%v auth_failures=%v", r.val("corral_upstream_connect_errors_total", "kind", kind, "error_class", "server"), r.val("corral_upstream_auth_failures_total", "kind", kind))
			}
			noMarker(t, r, "SHOULD-NOT-APPEAR")
		})
	}
	// A signature failure has no upstream code: the record has no status code.
	r := newRig(t)
	r.ad.Upstream().AuthFailed(r.ctx, "monitor", "db1.test:27017", &upstream.AuthFailed{Err: errors.New("synthetic signature failure")})
	r.one("upstream authentication failed", slog.LevelWarn, "upstream", "error", "corral.error.detail", "server.address", "server.port")
	// AuthSucceeded emits no warn or error record.
	r.ad.Upstream().AuthSucceeded(r.ctx, "pool", "db1.test:27017")
	for _, rec := range r.lh.Records() {
		if rec.Msg != "upstream authentication failed" && rec.Level >= slog.LevelWarn {
			t.Fatalf("unexpected warn record %q", rec.Msg)
		}
	}
}

// For each kind, auth failures never exceed connect errors with class server
// (the subset rule), even when pool opens and AuthFailed are both reported.
func TestAuthSubsetRule_AC39(t *testing.T) {
	r := newRig(t)
	af := &upstream.AuthFailed{Err: srvErr{18, "AuthenticationFailed"}}
	for i := 0; i < 5; i++ {
		r.ad.Pool().ConnOpenFailed(r.ctx, "h:1", af)
		r.ad.Upstream().AuthFailed(r.ctx, "pool", "h:1", af)
		r.ad.Upstream().AuthFailed(r.ctx, "monitor", "h:1", af)
		r.ad.Pool().ConnOpenFailed(r.ctx, "h:1", context_deadline())
		r.ad.Upstream().MonitorConnFailed(r.ctx, "h:1", context_deadline())
	}
	for _, kind := range []string{"pool", "monitor"} {
		if a, c := r.val("corral_upstream_auth_failures_total", "kind", kind), r.val("corral_upstream_connect_errors_total", "kind", kind, "error_class", "server"); a > c || a == 0 {
			t.Errorf("%s: auth_failures=%v connect_errors{server}=%v", kind, a, c)
		}
	}
}

func context_deadline() error { return os.ErrDeadlineExceeded }

func TestEventUpstreamMonitorConnections(t *testing.T) {
	r := newRig(t)
	r.ad.Upstream().MonitorConnOpened(r.ctx, "db2.test:27017", 5)
	a := r.one("upstream monitoring connection opened", slog.LevelDebug, "upstream", "server.address", "server.port", "corral.upstream.connection.id")
	if a["corral.upstream.connection.id"].Int64() != 5 {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_upstream_connections_opened_total", "kind", "monitor") != 1 {
		t.Fatal("opened_total{monitor} not incremented")
	}
	for _, reason := range upCloseReason {
		r2 := newRig(t)
		r2.ad.Upstream().MonitorConnClosed(r2.ctx, "db2.test:27017", reason)
		a := r2.one("upstream monitoring connection closed", slog.LevelDebug, "upstream", "reason", "server.address", "server.port")
		if a["reason"].String() != reason {
			t.Fatalf("reason %q", a["reason"].String())
		}
		if r2.val("corral_upstream_connections_closed_total", "kind", "monitor", "reason", reason) != 1 {
			t.Fatalf("closed_total{monitor,%s}", reason)
		}
	}
	// An unknown reason never becomes a label value or an out-of-set reason.
	r3 := newRig(t)
	r3.ad.Upstream().MonitorConnClosed(r3.ctx, "h:1", "some-new-reason")
	for _, rec := range r3.records("upstream monitoring connection closed") {
		if v := attrs(rec)["reason"].String(); !contains(upCloseReason, v) {
			t.Fatalf("reason %q outside the enumeration", v)
		}
	}
}

// ---- readiness, compatibility, primary ----

func TestEventUpstreamAvailability(t *testing.T) {
	for _, reason := range readiness {
		t.Run(reason, func(t *testing.T) {
			r := newRig(t)
			r.ad.Upstream().AvailabilityChanged(r.ctx, false, health.Reason(reason))
			a := r.one("upstream unavailable", slog.LevelWarn, "upstream", "reason")
			if a["reason"].String() != reason {
				t.Fatalf("reason %q", a["reason"].String())
			}
			if r.val("corral_upstream_available") != 0 {
				t.Fatal("gauge not 0")
			}
		})
	}
	r := newRig(t)
	r.ad.Upstream().AvailabilityChanged(r.ctx, false, health.ReasonUpstreamUnavailable)
	r.ad.Upstream().AvailabilityChanged(r.ctx, true, "")
	r.ad.Upstream().Available(r.ctx)
	if r.val("corral_upstream_available") != 1 {
		t.Fatal("gauge not 1 when ready")
	}
	r.one("upstream available", slog.LevelInfo, "upstream")
	// Each transition produces one record.
	r.ad.Upstream().AvailabilityChanged(r.ctx, false, health.ReasonUpstreamAuthFailed)
	if len(r.records("upstream unavailable")) != 2 {
		t.Fatalf("unavailable records = %d, want 2 (one per transition)", len(r.records("upstream unavailable")))
	}
	if r.val("corral_upstream_available") != 0 {
		t.Fatal("gauge not 0 after the second outage")
	}
}

func TestEventUpstreamIncompatible(t *testing.T) {
	for _, reason := range incompat {
		r := newRig(t)
		r.ad.Upstream().Incompatible(r.ctx, upstream.Reason(reason), "db3.test:27017")
		a := r.one("upstream incompatible", slog.LevelError, "upstream", "reason", "server.address", "server.port")
		if a["reason"].String() != reason || a["server.address"].String() != "db3.test" {
			t.Fatalf("attributes: %v", a)
		}
	}
}

func TestEventUpstreamPrimaryChanged(t *testing.T) {
	r := newRig(t)
	r.ad.Upstream().PrimaryChanged(r.ctx, "db4.test:27017")
	r.ad.Upstream().PrimaryChanged(r.ctx, "db5.test:27017")
	if len(r.records("upstream primary changed")) != 2 {
		t.Fatal("one record per change expected")
	}
	rec := r.records("upstream primary changed")[0]
	a := attrs(rec)
	if rec.Level != slog.LevelInfo || !sameKeys(a, "component", "server.address", "server.port") {
		t.Fatalf("level %v keys %v", rec.Level, keysOf(a))
	}
	if r.val("corral_upstream_primary_changes_total") != 2 {
		t.Fatalf("primary_changes_total = %v", r.val("corral_upstream_primary_changes_total"))
	}
}

// AC-27: this record carries the upstream's code (it is an error record
// reporting the upstream's error); no synthesized error record does.
func TestEventTargetMarkedNotPrimary_AC27(t *testing.T) {
	r := newRig(t)
	r.ad.Upstream().TargetMarkedNotPrimary(r.ctx, "db1.test:27017", 10107, "NotWritablePrimary")
	a := r.one("upstream target marked not primary", slog.LevelWarn, "upstream", "error", "corral.error.detail", "server.address", "server.port",
		"db.response.status_code", "corral.db.response.code_name")
	if a["error"].String() != "server" || a["corral.error.detail"].String() != "other" ||
		a["db.response.status_code"].String() != "10107" || a["corral.db.response.code_name"].String() != "NotWritablePrimary" {
		t.Fatalf("attributes: %v", a)
	}
}

// FR-62 (AC-46): unsolicited data is an upstream protocol error.
func TestEventUnsolicited_AC46(t *testing.T) {
	r := newRig(t)
	r.ad.Pool().ConnUnsolicited(r.ctx, "db1.test:27017")
	a := r.one("upstream protocol error", slog.LevelWarn, "upstream", "error", "corral.error.detail", "server.address", "server.port")
	if a["error"].String() != "protocol" || a["corral.error.detail"].String() != "unexpected_response" {
		t.Fatalf("attributes: %v", a)
	}
	if r.val("corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response") != 1 {
		t.Fatal("protocol_errors_total{upstream,unexpected_response} not incremented")
	}
	if r.val("corral_protocol_errors_total", "side", "client", "error_detail", "unexpected_response") != 0 {
		t.Fatal("the client side was incremented")
	}
}

// AC-27: synthesized errors never carry a status code in any record.
func TestSynthesizedNeverCarryStatusCode_AC27(t *testing.T) {
	r := newRig(t)
	r.ad.Proxy().ClientAuthRejected(r.ctx, 1, proxy.ClientAuthSASL)
	r.ad.Proxy().CommandBlocked(r.ctx, 1, "killOp")
	r.ad.Proxy().ClientProtocolError(r.ctx, 1, wire.ErrMalformed())
	r.ad.Proxy().ClientIOTimeout(r.ctx, 1)
	r.ad.Proxy().TLSFailed(r.ctx, tls.AlertError(40))
	r.ad.Pool().CheckoutFailed(r.ctx, "pool_wait_timeout", time.Second)
	r.ad.Pool().CheckoutFailed(r.ctx, "pool_queue_full", 0)
	r.ad.Pool().ConnUnsolicited(r.ctx, "h:1")
	r.ad.Proxy().RequestCompleted(r.ctx, proxy.RequestResult{Command: "find", Outcome: proxy.OutcomeProxyError, Err: proxy.UpstreamAuthFailed})
	for _, rec := range r.lh.Records() {
		a := attrs(rec)
		for _, k := range []string{"db.response.status_code", "corral.db.response.code_name"} {
			if _, ok := a[k]; ok {
				t.Errorf("record %q carries %s", rec.Msg, k)
			}
		}
	}
}

// FR-24, FR-26: no error text, panic value or marker reaches any record.
func TestNoMarkersInRecords_FR24(t *testing.T) {
	r := newRig(t)
	m := "SHOULD-NOT-APPEAR-X"
	wrap := func(e error) error { return fmt.Errorf("%s: %w", m, e) }
	r.ad.Proxy().ClientProtocolError(r.ctx, 1, wrap(wire.ErrMalformed()))
	r.ad.Proxy().TLSFailed(r.ctx, wrap(tls.AlertError(1)))
	r.ad.Pool().ConnOpenFailed(r.ctx, "h:1", wrap(os.ErrDeadlineExceeded))
	r.ad.Upstream().MonitorConnFailed(r.ctx, "h:1", wrap(os.ErrDeadlineExceeded))
	r.ad.Upstream().AuthFailed(r.ctx, "pool", "h:1", wrap(&upstream.AuthFailed{Err: srvErr{18, "AuthenticationFailed"}}))
	r.ad.Proxy().PanicRecovered(r.ctx, errclass.FromPanic(m, nil))
	noMarker(t, r, m)
	_ = logcheck.Record{}
}
