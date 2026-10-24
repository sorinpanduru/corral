package runtime_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

// FR-28 / ADR-0006 step 2, AC-3: logbridge is installed before any grpc, otel,
// tracing or admin-listener code, so nothing reaches stderr even with tracing
// on and an unreachable collector, for both OTLP protocols. Every line written
// to the supplied stdout writer is JSON. Not parallel: CaptureStdio swaps
// process-wide state, and Run installs process-wide bridges.
func TestNoStderrWithTracingAndUnreachableCollector(t *testing.T) { // AC-3, FR-28
	for _, proto := range []string{"http/protobuf", "grpc"} {
		t.Run(proto, func(t *testing.T) {
			stop := markerscan.CaptureStdio(t)
			ep := otlpcollector.UnreachableEndpoint(t)
			h := run(t, runOpts{
				args: []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
				env: map[string]string{
					"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + ep,
					"OTEL_EXPORTER_OTLP_PROTOCOL": proto,
				},
			})
			hd := h.waitStarted()
			_, span := hd.TracerProvider.Tracer("t18-test").Start(context.Background(), "t18-span",
				trace.WithSpanKind(trace.SpanKindInternal))
			span.End()
			time.Sleep(300 * time.Millisecond)
			if code := h.stop(); code != 0 {
				t.Errorf("exit code = %d, want 0 (graceful shutdown with an unreachable collector)", code)
			}
			_, stderr := stop()
			if len(stderr) != 0 {
				t.Errorf("process wrote %d bytes to stderr, want 0", len(stderr))
			}
			h.out.records(t) // every stdout line must be JSON
			if len(h.out.lines()) == 0 {
				t.Errorf("no log output at all on the supplied writer")
			}
		})
	}
}

// ADR-0006 steps 6-9 (T18 review D1): the admin listener serves first, then
// BeforeLastStep, then tracing starts as the last step. While held in
// BeforeLastStep, nothing has been exported, no collector connection exists,
// and no span from the global tracer is valid (tracing not yet started). After
// Started, the handed-out provider produces real spans that reach the
// collector at shutdown.
func TestTracingStartsAfterBeforeLastStep(t *testing.T) { // AC-31, FR-18, ADR-0006
	col := otlpcollector.Start(t)
	var mu sync.Mutex
	var spans, conns int
	var heldValid, heldCalled bool
	h := run(t, runOpts{
		args: []string{"-tracing.enabled=true", "-admin.listen=" + reserveAddr(t)},
		env: map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + col.HTTPEndpoint(),
			"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		},
		before: func(ctx context.Context) {
			_, sp := otel.Tracer("t18-held").Start(ctx, "held-span")
			valid := sp.SpanContext().IsValid()
			sp.End()
			mu.Lock()
			spans, conns, heldValid, heldCalled = len(col.Spans()), col.ConnCount(), valid, true
			mu.Unlock()
		},
	})
	hd := h.waitStarted()
	mu.Lock()
	if !heldCalled {
		t.Error("BeforeLastStep not called")
	}
	if spans != 0 || conns != 0 {
		t.Errorf("during BeforeLastStep: %d spans exported, %d collector connections; want none (tracing starts after the hook)", spans, conns)
	}
	if heldValid {
		t.Errorf("during BeforeLastStep the global tracer already produced a valid span; tracing must start after the hook")
	}
	mu.Unlock()

	_, sp := hd.TracerProvider.Tracer("t18-after").Start(context.Background(), "after-span")
	if !sp.SpanContext().IsValid() || !sp.IsRecording() {
		t.Errorf("Handles.TracerProvider does not produce a recording span after startup")
	}
	sp.End()
	if code := h.stop(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(col.Spans()) == 0 {
		t.Errorf("collector received no spans after the shutdown flush")
	}
}

// T18 review D1: on a bind failure with tracing on, tracing was never started,
// so the exit is quick (no 5 s shutdown wait on an unreachable collector).
func TestBindFailureWithTracingExitsQuickly(t *testing.T) { // AC-30, FR-35
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	ep := otlpcollector.UnreachableEndpoint(t)
	begin := time.Now()
	h := run(t, runOpts{
		args: []string{"-tracing.enabled=true", "-admin.listen=" + busy.Addr().String()},
		env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + ep},
	})
	if code := h.wait(); code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if d := time.Since(begin); d >= 2*time.Second {
		t.Errorf("bind failure with tracing on took %v, want < 2s", d)
	}
}

// T18 review D2 (FR-8, FR-19): trace correlation is installed only when
// Resolve says tracing is on. A record emitted under a forged valid span
// context (through the slog.Default bridge, component thirdparty) carries
// trace_id/span_id only then.
func TestCorrelationOnlyWhenTracingResolvedOn(t *testing.T) { // FR-8, FR-19
	tid := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	sid := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	forged := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{"tracing off", nil, nil, false},
		{"enabled but OTEL_SDK_DISABLED=TRUE", []string{"-tracing.enabled=true"}, map[string]string{"OTEL_SDK_DISABLED": "TRUE"}, false},
		{"enabled, exporter none", []string{"-tracing.enabled=true"}, map[string]string{"OTEL_TRACES_EXPORTER": "none"}, true},
		{"enabled, exporter otlp", []string{"-tracing.enabled=true"}, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + otlpcollector.UnreachableEndpoint(t)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := run(t, runOpts{
				args: append([]string{"-admin.listen=" + reserveAddr(t)}, tc.args...),
				env:  tc.env,
			})
			h.waitStarted()
			slog.WarnContext(forged, "forged-span-probe")
			var got []map[string]any
			for _, r := range byComponent(h.out.records(t), logging.ThirdParty) {
				got = append(got, r)
			}
			if len(got) != 1 {
				t.Fatalf("thirdparty records = %d, want 1 (slog.Default is bridged by Run)", len(got))
			}
			gt, hasT := got[0]["trace_id"].(string)
			gs, hasS := got[0]["span_id"].(string)
			if tc.want {
				if !hasT || gt != tid.String() || !hasS || gs != sid.String() {
					t.Errorf("record lacks the forged trace_id/span_id with tracing on")
				}
			} else if hasT || hasS {
				t.Errorf("record carries trace_id/span_id although tracing is not on")
			}
			h.stop()
		})
	}
}

// T18 review D4: an admin Serve failure after a successful Listen cannot be
// forced through the exposed hooks.
func TestAdminServeFailure(t *testing.T) {
	t.Skip("no hook can make admin.Serve fail after a successful Listen; reviewed instead")
}

// T18 review D7: a context cancel during startup (inside BeforeLastStep) is a
// graceful shutdown: exit 0, the shutting-down record exists, and no error or
// startup-failed record is written.
func TestCancelDuringStartupIsGraceful(t *testing.T) { // FR-18, FR-34
	h := run(t, runOpts{
		args:           []string{"-admin.listen=" + reserveAddr(t)},
		cancelInBefore: true,
	})
	if code := h.wait(); code != 0 {
		t.Fatalf("exit code = %d, want 0 for a cancel during startup", code)
	}
	recs := h.out.records(t)
	if !hasLifecycle(recs, "shutting down") {
		t.Errorf("no lifecycle shutting-down record after a cancel during startup")
	}
	for _, r := range recs {
		if str(r, "level") == "ERROR" || strings.Contains(strings.ToLower(str(r, "msg")), "fail") {
			t.Errorf("unexpected error/failed record after a graceful cancel: level=%v", r["level"])
		}
	}
}
