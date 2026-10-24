// Package tracing builds corral's tracer provider (SPEC-001 FR-8, FR-19..FR-21,
// FR-30, FR-33, FR-34, FR-37; ADR-0007).
//
// Compliance (GDPR data minimisation, FR-24): nothing here logs err.Error() or
// the collector endpoint. Errors returned by Start and Shutdown, and collector
// rejections, carry constant text. Other exporter errors are passed unchanged
// to otel.Handle and may contain endpoint text; their safety depends on the
// logbridge handler, which logs only errclass attributes (ADR-0007 c).
package tracing

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/tracing/spanqueue"
)

// shutdownCap is the FR-34 upper bound for Shutdown.
const shutdownCap = 5 * time.Second

// Options configures Start.
type Options struct {
	// Settings are the validated settings that Config was resolved from. Start
	// does not read them; they are kept so callers pass one coherent bundle.
	Settings config.Settings
	Env      LookupEnv
	Config   EnvConfig
	// Enabled is the result of Resolve.
	Enabled bool
	Metrics *metrics.Factory
	Logger  *slog.Logger
	// Exporter replaces the OTLP exporter. Tests only; used only when tracing
	// is enabled and the exporter is otlp, ignored otherwise. It is still
	// wrapped by FilterResource (ADR-0007 f).
	Exporter sdktrace.SpanExporter
}

// Tracing owns the tracer provider.
type Tracing struct {
	enabled bool
	tp      trace.TracerProvider
	sdk     *sdktrace.TracerProvider
	logger  *slog.Logger
}

// Start builds the provider. logbridge.Install must have run before (ADR-0006).
func Start(ctx context.Context, o Options) (*Tracing, error) {
	if !o.Enabled {
		return &Tracing{tp: noop.NewTracerProvider()}, nil
	}
	res, err := NewResource(ctx, o.Env)
	if err != nil {
		return nil, errStartResource
	}
	cfg := o.Config
	sampler := NewSampler(cfg, cfg.SampleRatio)

	var exp sdktrace.SpanExporter
	if cfg.Exporter == ExporterOTLP {
		if exp = o.Exporter; exp == nil {
			if o.Metrics == nil {
				return nil, errNoMetrics
			}
			exp, err = newOTLP(ctx, cfg.Protocol, o.Env)
			if err != nil {
				return nil, errStartExporter
			}
		}
	}

	provOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	if exp != nil {
		if o.Metrics == nil {
			_ = exp.Shutdown(ctx)
			return nil, errNoMetrics
		}
		dropped, cerr := o.Metrics.Counter(metrics.NameTracingSpansDropped)
		if cerr != nil {
			_ = exp.Shutdown(ctx)
			return nil, errStartMetrics
		}
		fexp := FilterResource(newClassifyExporter(exp), res)
		q := spanqueue.New(fexp, spanqueue.Config{
			MaxQueueSize:       cfg.Queue.MaxQueueSize,
			MaxExportBatchSize: cfg.Queue.MaxExportBatchSize,
			ScheduleDelay:      cfg.Queue.ScheduleDelay,
			ExportTimeout:      cfg.Queue.ExportTimeout,
		}, dropped)
		var sp sdktrace.SpanProcessor = q
		if f, ok := fexp.(interface{ ForceFlush(context.Context) error }); ok {
			sp = flushProcessor{SpanProcessor: q, exp: f}
		}
		provOpts = append(provOpts, sdktrace.WithSpanProcessor(sp))
	}
	tp := sdktrace.NewTracerProvider(provOpts...)
	if exp != nil {
		otel.SetTracerProvider(tp)
	}
	return &Tracing{enabled: true, tp: tp, sdk: tp, logger: o.Logger}, nil
}

func newOTLP(ctx context.Context, p Protocol, env LookupEnv) (sdktrace.SpanExporter, error) {
	// The exporters read OTEL_* from the process environment. For an http://
	// endpoint with TLS material, explicit options override the TLS the SDK
	// would build from that environment (Amendment 2: plaintext, FR-20).
	plain := env != nil && len(tlsNamesForHTTP(env)) > 0
	if p == ProtocolGRPC {
		var opts []otlptracegrpc.Option
		if plain {
			opts = plaintextGRPCOptions()
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	if plain {
		opts = plaintextHTTPOptions()
	}
	return otlptracehttp.New(ctx, opts...)
}

// constError has constant text; cause is kept only for errors.Is on context
// errors, never for rendering.
type constError struct {
	msg   string
	cause error
}

func (e *constError) Error() string { return e.msg }
func (e *constError) Is(target error) bool {
	return e.cause != nil && (target == context.Canceled || target == context.DeadlineExceeded) &&
		errors.Is(e.cause, target)
}

var (
	errStartResource = &constError{msg: "tracing: resource construction failed"}
	errStartExporter = &constError{msg: "tracing: exporter construction failed"}
	errStartMetrics  = &constError{msg: "tracing: dropped-spans counter unavailable"}
	errNoMetrics     = &constError{msg: "tracing: metrics factory required when exporting"}
)

// Enabled reports whether tracing is on (hook for FR-19).
func (t *Tracing) Enabled() bool { return t.enabled }

// TracerProvider returns the provider (no-op when disabled).
func (t *Tracing) TracerProvider() trace.TracerProvider { return t.tp }

// ForceFlush flushes queued spans, honouring ctx.
func (t *Tracing) ForceFlush(ctx context.Context) error {
	if t.sdk == nil {
		return nil
	}
	if err := t.sdk.ForceFlush(ctx); err != nil {
		return &constError{msg: "tracing: flush failed", cause: err}
	}
	return nil
}

// Shutdown stops the provider, bounded by ctx and by 5 s (FR-34).
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t.sdk == nil {
		return nil
	}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > shutdownCap {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownCap)
		defer cancel()
	}
	err := t.sdk.Shutdown(ctx)
	if err == nil {
		return nil
	}
	if t.logger != nil {
		t.logger.LogAttrs(ctx, slog.LevelWarn, "tracing shutdown failed", errclass.LogAttrs(err)...)
	}
	return &constError{msg: "tracing: shutdown failed", cause: err}
}

// rejectionError marks an OTLP collector rejection. Its text is constant so the
// response body and URL never reach logs (FR-24); it deliberately has no Unwrap.
type rejectionError struct{}

func (rejectionError) Error() string  { return "collector rejected export" }
func (rejectionError) ProtocolError() {}

var _ errclass.ProtocolError = rejectionError{}

// ClassifyExportError wraps collector rejections (gRPC status other than
// Unavailable/DeadlineExceeded/Canceled; plain non-transport errors such as the
// otlptracehttp non-2xx form) in a ProtocolError with constant text. Transport
// errors (*url.Error, net.Error) and everything else are returned unchanged.
func ClassifyExportError(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	var ne net.Error
	if errors.As(err, &ue) || errors.As(err, &ne) {
		return err
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return err
		}
		return rejectionError{}
	}
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		switch gs.GRPCStatus().Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return err
		}
		return rejectionError{}
	}
	if c, d := errclass.Classify(err); c == errclass.ClassInternal && d == errclass.DetailOther {
		return rejectionError{}
	}
	return err
}

type classifyExporter struct{ sdktrace.SpanExporter }

func (c classifyExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return ClassifyExportError(c.SpanExporter.ExportSpans(ctx, spans))
}

type flushClassifyExporter struct{ classifyExporter }

func (f flushClassifyExporter) ForceFlush(ctx context.Context) error {
	return f.SpanExporter.(interface{ ForceFlush(context.Context) error }).ForceFlush(ctx)
}

func newClassifyExporter(exp sdktrace.SpanExporter) sdktrace.SpanExporter {
	c := classifyExporter{exp}
	if _, ok := exp.(interface{ ForceFlush(context.Context) error }); ok {
		return flushClassifyExporter{c}
	}
	return c
}

// flushProcessor forwards ForceFlush to a flush-capable exporter after the
// queue has been drained (spanqueue itself only exports the queued batch).
type flushProcessor struct {
	sdktrace.SpanProcessor
	exp interface{ ForceFlush(context.Context) error }
}

func (f flushProcessor) ForceFlush(ctx context.Context) error {
	err := f.SpanProcessor.ForceFlush(ctx)
	if ferr := f.exp.ForceFlush(ctx); err == nil {
		err = ferr
	}
	return err
}
