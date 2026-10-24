package tracing_test

import (
	"context"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/testkit/otlpcollector"
)

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func findRecord(t *testing.T, h *harness, msg string) record {
	t.Helper()
	for _, r := range h.records() {
		if r.Msg == msg {
			return r
		}
	}
	t.Fatalf("no record with message %q", msg)
	return record{}
}

func assertCorrelated(t *testing.T, r record, sc trace.SpanContext) {
	t.Helper()
	tid, sid := r.str("trace_id"), r.str("span_id")
	if !hex32.MatchString(tid) {
		t.Errorf("trace_id %q is not 32 lowercase hex", tid)
	}
	if !hex16.MatchString(sid) {
		t.Errorf("span_id %q is not 16 lowercase hex", sid)
	}
	if tid != sc.TraceID().String() {
		t.Errorf("trace_id %q does not match span context %q", tid, sc.TraceID())
	}
	if sid != sc.SpanID().String() {
		t.Errorf("span_id %q does not match span context %q", sid, sc.SpanID())
	}
}

func assertNotCorrelated(t *testing.T, r record) {
	t.Helper()
	for _, k := range []string{"trace_id", "span_id"} {
		if _, ok := r.Attrs[k]; ok {
			t.Errorf("record %q carries %s", r.Msg, k)
		}
	}
}

// AC-6 (FR-8, D9): with tracing on, records emitted inside a sampled span and
// inside an unsampled span both carry trace_id and span_id matching the span
// context; records outside any span carry neither. Covers the otlp exporter
// and OTEL_TRACES_EXPORTER=none.
func TestAC6_LogsCarryTraceIDsWhenTracingIsOn(t *testing.T) {
	for _, exporter := range []string{"otlp", "none"} {
		t.Run(exporter, func(t *testing.T) {
			col := otlpcollector.Start(t)
			h := boot(t, opts{on: true, env: map[string]string{
				"OTEL_TRACES_EXPORTER":        exporter,
				"OTEL_EXPORTER_OTLP_ENDPOINT": httpURL(col),
			}})
			if !h.tr.Enabled() {
				t.Fatal("Enabled() = false")
			}
			tracer := h.tr.TracerProvider().Tracer("corral-test")

			sctx, sampled := tracer.Start(context.Background(), "sampled-root")
			if !sampled.SpanContext().IsSampled() {
				t.Fatal("root span with the default sampler at ratio 1.0 is not sampled")
			}
			h.logger.InfoContext(sctx, msgInsideSampled)
			sampled.End()

			// An unsampled remote parent makes the parent-based default sampler
			// drop the child: the span context is valid but not sampled.
			remote := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
				SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
				TraceFlags: 0,
				Remote:     true,
			})
			pctx := trace.ContextWithRemoteSpanContext(context.Background(), remote)
			uctx, unsampled := tracer.Start(pctx, "unsampled-child")
			if unsampled.SpanContext().IsSampled() || !unsampled.SpanContext().IsValid() {
				t.Fatalf("expected a valid, unsampled span context, got sampled=%v valid=%v",
					unsampled.SpanContext().IsSampled(), unsampled.SpanContext().IsValid())
			}
			h.logger.InfoContext(uctx, msgInsideUnsampled)
			unsampled.End()

			h.logger.InfoContext(context.Background(), msgOutsideSpan)

			assertCorrelated(t, findRecord(t, h, msgInsideSampled), sampled.SpanContext())
			assertCorrelated(t, findRecord(t, h, msgInsideUnsampled), unsampled.SpanContext())
			assertNotCorrelated(t, findRecord(t, h, msgOutsideSpan))
			h.finish()
		})
	}
}

// AC-6 (FR-8): with tracing off, neither attribute appears, even when the
// record's context holds a valid span context.
func TestAC6_LogsCarryNoTraceIDsWhenTracingIsOff(t *testing.T) {
	cases := []struct {
		name string
		on   bool
		env  map[string]string
	}{
		{"default settings", false, nil},
		{"OTEL_SDK_DISABLED=true", true, map[string]string{"OTEL_SDK_DISABLED": "true"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := boot(t, opts{on: c.on, env: c.env})
			if h.tr.Enabled() {
				t.Fatal("Enabled() = true")
			}
			sctx, sp := h.tr.TracerProvider().Tracer("corral-test").Start(context.Background(), "off-root")
			forged := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID:    trace.TraceID{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9},
				SpanID:     trace.SpanID{9, 9, 9, 9, 9, 9, 9, 9},
				TraceFlags: trace.FlagsSampled,
			})
			fctx := trace.ContextWithSpanContext(sctx, forged)
			h.logger.InfoContext(fctx, msgInsideSampled)
			sp.End()
			assertNotCorrelated(t, findRecord(t, h, msgInsideSampled))
			h.finish()
		})
	}
}
