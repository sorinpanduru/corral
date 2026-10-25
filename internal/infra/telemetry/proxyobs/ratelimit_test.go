package proxyobs_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/domain/pool"
	"github.com/sorinpanduru/corral/internal/domain/proxy"
	"github.com/sorinpanduru/corral/internal/domain/upstream"
	"github.com/sorinpanduru/corral/internal/domain/wire"
)

// FR-49 (AC-27): the rate-limited warn and error events emit at most one record
// per 30 seconds per event, with suppressed_count on the next record.

type limited struct {
	msg     string
	trigger func(r *rig)
}

func limitedEvents() []limited {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	return []limited{
		{"client connection rejected", func(r *rig) { r.ad.Proxy().ConnRejected(r.ctx, proxy.RejectLimitExceeded) }},
		{"client authentication rejected", func(r *rig) { r.ad.Proxy().ClientAuthRejected(r.ctx, 1, proxy.ClientAuthSASL) }},
		{"client command blocked", func(r *rig) { r.ad.Proxy().CommandBlocked(r.ctx, 1, "currentOp") }},
		{"client protocol error", func(r *rig) { r.ad.Proxy().ClientProtocolError(r.ctx, 1, wire.ErrMalformed()) }},
		{"client I/O timed out", func(r *rig) { r.ad.Proxy().ClientIOTimeout(r.ctx, 1) }},
		{"client TLS handshake failed", func(r *rig) { r.ad.Proxy().TLSFailed(r.ctx, netErr) }},
		{"pool checkout timed out", func(r *rig) { r.ad.Pool().CheckoutFailed(r.ctx, "pool_wait_timeout", time.Second) }},
		{"pool wait queue full", func(r *rig) { r.ad.Pool().CheckoutFailed(r.ctx, "pool_queue_full", 0) }},
		{"upstream connection failed", func(r *rig) { r.ad.Pool().ConnOpenFailed(r.ctx, "h:1", netErr) }},
		{"upstream authentication failed", func(r *rig) {
			r.ad.Upstream().AuthFailed(r.ctx, "pool", "h:1", &upstream.AuthFailed{Err: srvErr{18, "AuthenticationFailed"}})
		}},
		{"upstream target marked not primary", func(r *rig) { r.ad.Upstream().TargetMarkedNotPrimary(r.ctx, "h:1", 10107, "NotWritablePrimary") }},
		{"upstream protocol error", func(r *rig) { r.ad.Pool().ConnUnsolicited(r.ctx, "h:1") }},
		// RC5: the panic record is rate-limited too, so a client that can
		// trigger panics cannot flood the log.
		{"request handler panic recovered", func(r *rig) { r.ad.Proxy().PanicRecovered(r.ctx, errors.New("synthetic panic")) }},
	}
}

func suppressedOf(r *rig, msg string, i int) (int64, bool) {
	a := attrs(r.records(msg)[i])
	v, ok := a["suppressed_count"]
	if !ok {
		return 0, false
	}
	return v.Int64(), true
}

func TestRateLimitedEvents_AC27(t *testing.T) {
	for _, ev := range limitedEvents() {
		t.Run(ev.msg, func(t *testing.T) {
			r := newRig(t)
			for i := 0; i < 10; i++ {
				ev.trigger(r)
				r.clk.Advance(2 * time.Second) // 10 triggers over 20 s
			}
			if n := len(r.records(ev.msg)); n != 1 {
				t.Fatalf("10 triggers in 30 s gave %d records, want 1", n)
			}
			if v, ok := suppressedOf(r, ev.msg, 0); ok && v != 0 {
				t.Fatalf("first record has suppressed_count=%d", v)
			}
			r.clk.Advance(10 * time.Second) // 30 s since the first emission
			ev.trigger(r)
			recs := r.records(ev.msg)
			if len(recs) != 2 {
				t.Fatalf("after 30 s: %d records, want 2", len(recs))
			}
			if v, ok := suppressedOf(r, ev.msg, 1); !ok || v != 9 {
				t.Fatalf("second record suppressed_count=%d (present=%v), want 9", v, ok)
			}
			// A trigger just inside the window is dropped again.
			r.clk.Advance(29 * time.Second)
			ev.trigger(r)
			if len(r.records(ev.msg)) != 2 {
				t.Fatal("a trigger inside the window emitted")
			}
			// The key is numeric: slog kind int.
			if k := attrs(r.records(ev.msg)[1])["suppressed_count"].Kind(); k != slog.KindInt64 && k != slog.KindUint64 {
				t.Fatalf("suppressed_count kind %v", k)
			}
		})
	}
}

// Each event has its own limiter: a flood of one does not hide another.
func TestLimitersAreIndependent_AC27(t *testing.T) {
	r := newRig(t)
	evs := limitedEvents()
	for i := 0; i < 5; i++ {
		evs[0].trigger(r)
	}
	for _, ev := range evs[1:] {
		ev.trigger(r)
		if len(r.records(ev.msg)) != 1 {
			t.Fatalf("%q was suppressed by another event's limiter", ev.msg)
		}
	}
	if len(r.records(evs[0].msg)) != 1 {
		t.Fatal("the flooded event emitted more than once")
	}
}

// The same event reached from the pool and the monitor shares one limiter
// ("one per event and level").
func TestSharedEventSharedLimiter_AC27(t *testing.T) {
	r := newRig(t)
	err := os.ErrDeadlineExceeded
	r.ad.Pool().ConnOpenFailed(r.ctx, "h:1", err)
	r.ad.Upstream().MonitorConnFailed(r.ctx, "h:1", err)
	if n := len(r.records("upstream connection failed")); n != 1 {
		t.Fatalf("pool and monitor failures gave %d records within the window, want 1", n)
	}
	// Metrics are not rate limited: both were counted.
	if r.sum("corral_upstream_connect_errors_total") != 2 {
		t.Fatalf("connect_errors_total = %v, want 2", r.sum("corral_upstream_connect_errors_total"))
	}
}

// Metrics are never rate limited.
func TestMetricsNotRateLimited_AC27(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 40; i++ {
		r.ad.Proxy().ClientAuthRejected(r.ctx, 1, proxy.ClientAuthSASL)
		r.ad.Pool().CheckoutFailed(r.ctx, "pool_queue_full", 0)
		r.ad.Pool().ConnUnsolicited(r.ctx, "h:1")
	}
	if r.val("corral_client_auth_rejected_total") != 40 ||
		r.val("corral_pool_checkout_failures_total", "error_detail", "pool_queue_full") != 40 ||
		r.val("corral_protocol_errors_total", "side", "upstream", "error_detail", "unexpected_response") != 40 {
		t.Fatal("a metric was rate limited")
	}
}

// Debug and transition records are not rate limited.
func TestDebugAndTransitionNotLimited(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 10; i++ {
		r.ad.Proxy().ConnOpened(r.ctx, proxy.ConnInfo{ID: int32(i + 1)})
		r.ad.Pool().ConnClosed(r.ctx, "h:1", pool.CloseReason("idle"))
	}
	if len(r.records("client connection opened")) != 10 || len(r.records("upstream connection closed")) != 10 {
		t.Fatal("debug records were limited")
	}
	_ = fmt.Sprint
}
