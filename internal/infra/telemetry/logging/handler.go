package logging

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/attr"
)

// NewHandler returns a handler writing synchronously to w (FR-38: a blocked
// writer blocks the logging call; there is no buffering).
func NewHandler(w io.Writer, f Format, level slog.Leveler) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	if f == FormatText {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

type correlationHandler struct{ next slog.Handler }

// WithTraceCorrelation wraps h so records whose context holds a valid span
// context (sampled or not, FR-8, D9) carry trace_id and span_id in lowercase
// hex. Install it only when tracing is enabled.
func WithTraceCorrelation(h slog.Handler) slog.Handler { return correlationHandler{next: h} }

func (c correlationHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return c.next.Enabled(ctx, l)
}

func (c correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String(attr.TraceID, sc.TraceID().String()),
			slog.String(attr.SpanID, sc.SpanID().String()),
		)
	}
	return c.next.Handle(ctx, r)
}

func (c correlationHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return correlationHandler{next: c.next.WithAttrs(as)}
}

func (c correlationHandler) WithGroup(name string) slog.Handler {
	return correlationHandler{next: c.next.WithGroup(name)}
}
