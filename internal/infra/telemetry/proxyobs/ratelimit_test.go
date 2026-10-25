package proxyobs_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
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

// AC-22 (FR-42, FR-49; Amendment 7), on a fake clock: the hook panics on 10
// client connections within 30 s, then an 11th after the window. One record in
// the window; the 11th gives one record with suppressed_count 9; every close is
// counted (corral_client_connections_closed_total{reason="internal_error"}
// rises by exactly 11, so no metric is limited); the record carries class
// internal and no panic value.
func TestPanicRecordRateLimitAndEveryCloseCounted_AC22(t *testing.T) {
	r := newRig(t)
	const marker = "SynthPanicValueMarkerZ4"
	panicOnce := func() {
		r.ad.Proxy().PanicRecovered(r.ctx, errors.New(marker))
		r.ad.Proxy().ConnClosed(r.ctx, 1, proxy.CloseInternalError)
	}
	before := r.val("corral_client_connections_closed_total", "reason", "internal_error")
	for i := 0; i < 10; i++ {
		panicOnce()
		r.clk.Advance(2 * time.Second) // 10 panics over 20 s
	}
	if n := len(r.records("request handler panic recovered")); n != 1 {
		t.Fatalf("10 panics within 30 s gave %d records, want 1", n)
	}
	r.clk.Advance(30 * time.Second) // beyond the window
	panicOnce()
	recs := r.records("request handler panic recovered")
	if len(recs) != 2 {
		t.Fatalf("the 11th panic after the window gave %d records in total, want 2", len(recs))
	}
	if v, ok := suppressedOf(r, "request handler panic recovered", 1); !ok || v != 9 {
		t.Fatalf("suppressed_count = %d (present %v), want 9", v, ok)
	}
	if got := r.val("corral_client_connections_closed_total", "reason", "internal_error") - before; got != 11 {
		t.Fatalf("internal_error closes counted: %v, want exactly 11", got)
	}
	for _, rec := range recs {
		a := attrs(rec)
		if v, ok := a["error"]; !ok || v.String() != "internal" {
			t.Fatalf("record class %v, want internal", v)
		}
		for k, v := range a {
			if strings.Contains(v.String(), marker) {
				t.Fatalf("the panic value appears in attribute %q", k)
			}
		}
	}
}
