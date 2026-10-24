package tracing_test

// T16 (plans/SPEC-001.md): FR-34 and AC-28 through the real bootstrap.
// Expected API: none new; only tracing.Start / (*Tracing).Shutdown / ForceFlush
// as defined by T15. Synthetic data only. Not parallel (process globals).

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

type protoCase struct {
	name  string
	proto string
	url   func(*otlpcollector.Collector) string
	addr  func(*otlpcollector.Collector) string
}

var protoCases = []protoCase{
	{"http", "http/protobuf", httpURL, func(c *otlpcollector.Collector) string { return c.HTTPEndpoint() }},
	{"grpc", "grpc", grpcURL, func(c *otlpcollector.Collector) string { return c.GRPCEndpoint() }},
}

// slowEnv makes the exporter and processor timeouts far longer than 5 s, so
// only the FR-34 bound can end Shutdown early.
func slowEnv(pc protoCase, endpoint string) map[string]string {
	return map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL": pc.proto,
		"OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
		"OTEL_EXPORTER_OTLP_TIMEOUT":  "30000",
		"OTEL_BSP_EXPORT_TIMEOUT":     "30000",
		"OTEL_BSP_SCHEDULE_DELAY":     "60000", // only the shutdown flush exports
	}
}

// assertConstantErr checks the shutdown/flush error is a deadline error whose
// text carries no endpoint, host or port (FR-29, FR-34).
func assertConstantErr(t *testing.T, what string, err error, banned ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: want a non-nil error when spans could not be flushed in time", what)
		return
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%s: errors.Is(err, context.DeadlineExceeded) is false", what)
	}
	for _, b := range banned {
		if b != "" && strings.Contains(err.Error(), b) {
			t.Errorf("%s: error text contains endpoint data", what)
		}
	}
}

func bannedFor(addr string) []string {
	host, port, _ := net.SplitHostPort(addr)
	return []string{addr, host, port, "127.0.0.1", "localhost"}
}

// AC-28 / FR-34: caller context without a deadline; Shutdown still returns
// within 5 s plus teardown margin (< 7 s), with a constant-text deadline error.
func TestAC28_ShutdownNoCallerDeadlineBoundedByFiveSeconds(t *testing.T) {
	for _, pc := range protoCases {
		t.Run(pc.name, func(t *testing.T) {
			col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
			h := boot(t, opts{on: true, env: slowEnv(pc, pc.url(col))})
			h.emit("stuck-1", "stuck-2", "stuck-3")
			h.shutDown = true
			start := time.Now()
			err := h.tr.Shutdown(context.Background())
			d := time.Since(start)
			if d >= 7*time.Second {
				t.Errorf("AC-28: Shutdown took %v, want < 7s (5s bound plus teardown)", d)
			}
			if d < 4500*time.Millisecond {
				t.Errorf("Shutdown returned after %v against an unresponsive collector; the flush should have been attempted until the 5s bound", d)
			}
			assertConstantErr(t, "Shutdown", err, bannedFor(pc.addr(col))...)
			// FR-37: the counter counts only queue-full drops, not spans lost
			// to the shutdown flush deadline.
			if v, ok := h.droppedValue(); !ok || v != 0 {
				t.Errorf("FR-37: %s = %v (registered=%v) after a timed-out shutdown, want 0", droppedMetric, v, ok)
			}
			h.finish()
		})
	}
}

// FR-34: min(caller deadline, 5 s): a 300 ms caller deadline is honoured.
func TestAC28_ShutdownHonoursShortCallerDeadline(t *testing.T) {
	for _, pc := range protoCases {
		t.Run(pc.name, func(t *testing.T) {
			col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
			h := boot(t, opts{on: true, env: slowEnv(pc, pc.url(col))})
			h.emit("stuck")
			h.shutDown = true
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := h.tr.Shutdown(ctx)
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("FR-34: Shutdown took %v with a 300ms caller deadline, want ~2s at most", d)
			}
			assertConstantErr(t, "Shutdown", err, bannedFor(pc.addr(col))...)
			h.finish()
		})
	}
}

// FR-34: a caller deadline longer than 5 s does not extend the bound.
func TestAC28_ShutdownLongCallerDeadlineStillCappedAtFiveSeconds(t *testing.T) {
	col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
	h := boot(t, opts{on: true, env: slowEnv(protoCases[0], httpURL(col))})
	h.emit("stuck")
	h.shutDown = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	err := h.tr.Shutdown(ctx)
	if d := time.Since(start); d >= 7*time.Second {
		t.Errorf("FR-34: Shutdown took %v with a 60s caller deadline, want < 7s", d)
	}
	assertConstantErr(t, "Shutdown", err, bannedFor(col.HTTPEndpoint())...)
	h.finish()
}

// AC-28 / FR-34: with a responsive collector, spans ended just before Shutdown
// are all received and Shutdown returns nil.
func TestAC28_SpansEndedBeforeShutdownReachCollector(t *testing.T) {
	const n = 25
	for _, pc := range protoCases {
		t.Run(pc.name, func(t *testing.T) {
			col := otlpcollector.Start(t)
			env := slowEnv(pc, pc.url(col))
			h := boot(t, opts{on: true, env: env})
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, "pending-"+string(rune('a'+i)))
			}
			h.emit(names...)
			if err := h.shutdown(); err != nil {
				t.Errorf("Shutdown with a responsive collector returned %v", err)
			}
			got := map[string]int{}
			for _, s := range spanNames(col) {
				got[s]++
			}
			for _, nm := range names {
				if got[nm] != 1 {
					t.Errorf("AC-28: span %s received %d times after Shutdown, want 1", nm, got[nm])
				}
			}
			h.finish()
		})
	}
}

// Strengthened T15 review item 5 (FR-29, FR-34): Shutdown and ForceFlush against
// an unresponsive collector must fail, with errors.Is DeadlineExceeded, and the
// text must not hold the endpoint, credentials or a marker.
func TestShutdownAndFlushErrorsHaveConstantTextStrict(t *testing.T) {
	for _, pc := range protoCases {
		t.Run(pc.name, func(t *testing.T) {
			m := newMarker(t)
			col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
			endpoint := strings.Replace(pc.url(col), "http://", "http://synthuser:"+m+"@", 1)
			h := boot(t, opts{on: true, env: slowEnv(pc, endpoint)})
			h.emit("stuck")
			banned := append(bannedFor(pc.addr(col)), m, "synthuser")

			fctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			assertConstantErr(t, "ForceFlush", h.tr.ForceFlush(fctx), banned...)
			cancel()

			h.shutDown = true
			sctx, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
			assertConstantErr(t, "Shutdown", h.tr.Shutdown(sctx), banned...)
			cancel2()
			h.finish()
		})
	}
}

// FR-29 / AC-3 / FR-28: a timed-out shutdown writes nothing to stderr, logs
// exactly one fixed-message failure record, and neither stdout nor any record
// carries the userinfo marker, "synthuser" or the collector host:port.
func TestFR29_TimedOutShutdownLogsAreFixedMessages(t *testing.T) {
	const failMsg = "tracing shutdown failed"
	for _, pc := range protoCases {
		t.Run(pc.name, func(t *testing.T) {
			m := newMarker(t)
			col := otlpcollector.Start(t, otlpcollector.WithMode(otlpcollector.ModeUnresponsive))
			endpoint := strings.Replace(pc.url(col), "http://", "http://synthuser:"+m+"@", 1)
			h := boot(t, opts{on: true, env: slowEnv(pc, endpoint)})
			h.emit("stuck")
			h.shutDown = true
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_ = h.tr.Shutdown(ctx)
			stdout, _ := h.finish()

			n := 0
			for _, r := range h.records() {
				if r.Msg == failMsg {
					n++
				}
			}
			if n != 1 {
				t.Errorf("found %d records with message %q, want exactly 1", n, failMsg)
			}
			banned := append(bannedFor(pc.addr(col)), m, "synthuser")
			for _, b := range banned {
				if strings.Contains(string(stdout), b) {
					t.Errorf("stdout contains endpoint data")
				}
				for _, r := range h.records() {
					if strings.Contains(r.Msg, b) {
						t.Errorf("a log message contains endpoint data")
					}
					for k, v := range r.Attrs {
						if strings.Contains(v.String(), b) {
							t.Errorf("log attribute %q contains endpoint data", k)
						}
					}
				}
			}
			if v, ok := h.droppedValue(); !ok || v != 0 {
				t.Errorf("FR-37: %s = %v after a timed-out shutdown, want 0", droppedMetric, v)
			}
		})
	}
}
