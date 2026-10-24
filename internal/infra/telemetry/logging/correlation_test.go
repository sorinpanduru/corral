package logging_test

import (
	"context"
	"log/slog"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func syntheticSpanContext(sampled, remote bool) trace.SpanContext {
	cfg := trace.SpanContextConfig{
		TraceID: trace.TraceID{0x0a, 0xBC, 0xde, 0xf0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0xAA, 0xBB, 0xCC},
		SpanID:  trace.SpanID{0xFE, 0xDC, 0xBA, 0x98, 1, 2, 3, 4},
		Remote:  remote,
	}
	if sampled {
		cfg.TraceFlags = trace.FlagsSampled
	}
	return trace.NewSpanContext(cfg)
}

func correlationLogger(t *testing.T, f logging.Format) (*slog.Logger, *syncBuffer) {
	t.Helper()
	var buf syncBuffer
	h := logging.WithTraceCorrelation(checked(t, logging.NewHandler(&buf, f, slog.LevelDebug), true))
	l, err := logging.NewFactory(h).Logger(logging.Tracing)
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

// AC-6 (unit), FR-8, D9: records with a valid span context, sampled or
// unsampled, carry trace_id (32 lowercase hex) and span_id (16 lowercase hex)
// matching the span context.
func TestCorrelationAddsIDsSampledAndUnsampled(t *testing.T) {
	for _, sampled := range []bool{true, false} {
		sc := syntheticSpanContext(sampled, false)
		l, buf := correlationLogger(t, logging.FormatJSON)
		ctx := trace.ContextWithSpanContext(context.Background(), sc)
		l.InfoContext(ctx, "synthetic in span")
		recs := jsonLines(t, buf)
		if len(recs) != 1 {
			t.Fatalf("sampled=%v: %d records", sampled, len(recs))
		}
		r := recs[0]
		tid, _ := r["trace_id"].(string)
		sid, _ := r["span_id"].(string)
		if !hex32.MatchString(tid) || tid != sc.TraceID().String() {
			t.Errorf("sampled=%v trace_id = %q, want %q", sampled, tid, sc.TraceID())
		}
		if !hex16.MatchString(sid) || sid != sc.SpanID().String() {
			t.Errorf("sampled=%v span_id = %q, want %q", sampled, sid, sc.SpanID())
		}
		if r["component"] != "tracing" {
			t.Errorf("component lost: %v", r)
		}
	}
}

// FR-8: a remote (propagated) valid span context also correlates.
func TestCorrelationRemoteSpanContext(t *testing.T) {
	sc := syntheticSpanContext(true, true)
	l, buf := correlationLogger(t, logging.FormatJSON)
	l.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "synthetic remote")
	r := jsonLines(t, buf)[0]
	if r["trace_id"] != sc.TraceID().String() || r["span_id"] != sc.SpanID().String() {
		t.Errorf("remote context not correlated: %v", r)
	}
}

// FR-8: no span context, or an invalid one, gives neither attribute.
func TestCorrelationAbsentWithoutValidSpanContext(t *testing.T) {
	l, buf := correlationLogger(t, logging.FormatJSON)
	l.Info("synthetic no ctx")
	l.InfoContext(context.Background(), "synthetic background")
	l.InfoContext(trace.ContextWithSpanContext(context.Background(), trace.SpanContext{}), "synthetic invalid")
	recs := jsonLines(t, buf)
	if len(recs) != 3 {
		t.Fatalf("got %d records", len(recs))
	}
	for _, r := range recs {
		for _, k := range []string{"trace_id", "span_id"} {
			if _, ok := r[k]; ok {
				t.Errorf("record %v has %q without a valid span context", r, k)
			}
		}
	}
}

// FR-8: without the wrapper (tracing disabled), the attributes are absent
// even when the context carries a span context.
func TestNoCorrelationWithoutWrapper(t *testing.T) {
	l, buf := newLogger(t, logging.FormatJSON, slog.LevelInfo, logging.Tracing)
	l.InfoContext(trace.ContextWithSpanContext(context.Background(), syntheticSpanContext(true, false)), "synthetic")
	r := jsonLines(t, buf)[0]
	for _, k := range []string{"trace_id", "span_id"} {
		if _, ok := r[k]; ok {
			t.Errorf("unexpected %q without the correlation wrapper", k)
		}
	}
}

// FR-8: correlation survives With-derived loggers and applies in text format.
func TestCorrelationThroughWithAttrsAndText(t *testing.T) {
	sc := syntheticSpanContext(true, false)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	l, buf := correlationLogger(t, logging.FormatJSON)
	l.With("retry_count", 2).InfoContext(ctx, "synthetic with")
	r := jsonLines(t, buf)[0]
	if r["trace_id"] != sc.TraceID().String() || r["span_id"] != sc.SpanID().String() {
		t.Errorf("With lost correlation: %v", r)
	}
	if r["retry_count"] != float64(2) {
		t.Errorf("With attrs lost: %v", r)
	}

	tl, tbuf := correlationLogger(t, logging.FormatText)
	tl.InfoContext(ctx, "synthetic text")
	line := tbuf.String()
	for _, want := range []string{"trace_id=" + sc.TraceID().String(), "span_id=" + sc.SpanID().String()} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(line) {
			t.Errorf("text line lacks %q: %q", want, line)
		}
	}
}

// AC-4 with the wrapper: Enabled delegates to the wrapped handler.
func TestCorrelationEnabledDelegates(t *testing.T) {
	var buf syncBuffer
	h := logging.WithTraceCorrelation(logging.NewHandler(&buf, logging.FormatJSON, slog.LevelWarn))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info enabled through wrapper at warn")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("error not enabled through wrapper at warn")
	}
}
