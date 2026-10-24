package tracing

import (
	"context"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// resourceSpan overrides Resource() of the embedded span.
type resourceSpan struct {
	sdktrace.ReadOnlySpan
	res *resource.Resource
}

func (s resourceSpan) Resource() *resource.Resource { return s.res }

type filterExporter struct {
	exp sdktrace.SpanExporter
	res *resource.Resource
}

func (f *filterExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, sp := range spans {
		out[i] = resourceSpan{ReadOnlySpan: sp, res: f.res}
	}
	return f.exp.ExportSpans(ctx, out)
}

func (f *filterExporter) Shutdown(ctx context.Context) error { return f.exp.Shutdown(ctx) }

type flushFilterExporter struct{ *filterExporter }

func (f flushFilterExporter) ForceFlush(ctx context.Context) error {
	return f.exp.(interface{ ForceFlush(context.Context) error }).ForceFlush(ctx)
}

// FilterResource wraps exp so every exported span carries exactly res (FR-30).
// sdktrace.WithResource re-merges the process environment, which could bring
// back the banned process.* keys; this wrapper replaces the span resource
// at export time. ForceFlush is exposed only if exp implements it.
func FilterResource(exp sdktrace.SpanExporter, res *resource.Resource) sdktrace.SpanExporter {
	f := &filterExporter{exp: exp, res: res}
	if _, ok := exp.(interface{ ForceFlush(context.Context) error }); ok {
		return flushFilterExporter{f}
	}
	return f
}
